package middleware

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sethvargo/go-limiter"
)

const (
	sweepEveryNTakes = 1024
	shardCount       = 16
)

type rateLimitShard struct {
	mu      sync.Mutex
	buckets map[string]*rateLimitBucket
	takes   uint64
}

type inMemoryRateLimitStore struct {
	shards [shardCount]rateLimitShard

	defaultTokens   uint64
	defaultInterval time.Duration

	closed atomic.Bool
}

type rateLimitBucket struct {
	tokens   uint64
	interval time.Duration

	remaining uint64
	resetAt   time.Time
}

func newInMemoryRateLimitStore(tokens uint64, interval time.Duration) limiter.Store {
	s := &inMemoryRateLimitStore{
		defaultTokens:   tokens,
		defaultInterval: interval,
	}
	for i := range s.shards {
		s.shards[i].buckets = make(map[string]*rateLimitBucket, 16)
	}
	return s
}

// shard returns the shard for the given key using FNV-1a hashing.
func (s *inMemoryRateLimitStore) shard(key string) *rateLimitShard {
	return &s.shards[fnv1aHash(key)%shardCount]
}

func (s *inMemoryRateLimitStore) Take(ctx context.Context, key string) (uint64, uint64, uint64, bool, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, 0, 0, false, err
		}
	}

	if s.closed.Load() {
		return 0, 0, 0, false, limiter.ErrStopped
	}

	now := time.Now().UTC()
	sh := s.shard(key)

	sh.mu.Lock()
	defer sh.mu.Unlock()

	b := s.ensureBucketLocked(sh, key, now)

	tokens := b.tokens
	remaining := b.remaining
	ok := false
	if remaining > 0 {
		remaining--
		b.remaining = remaining
		ok = true
	}

	sh.takes++
	if sh.takes%sweepEveryNTakes == 0 {
		sweepShardLocked(sh, now)
	}

	return tokens, remaining, uint64(b.resetAt.UnixNano()), ok, nil
}

func (s *inMemoryRateLimitStore) Get(ctx context.Context, key string) (uint64, uint64, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
	}

	if s.closed.Load() {
		return 0, 0, limiter.ErrStopped
	}

	now := time.Now().UTC()
	sh := s.shard(key)

	sh.mu.Lock()
	defer sh.mu.Unlock()

	b := s.ensureBucketLocked(sh, key, now)
	return b.tokens, b.remaining, nil
}

func (s *inMemoryRateLimitStore) Set(ctx context.Context, key string, tokens uint64, interval time.Duration) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if tokens == 0 {
		tokens = 1
	}
	if interval <= 0 {
		interval = time.Second
	}

	if s.closed.Load() {
		return limiter.ErrStopped
	}

	now := time.Now().UTC()
	sh := s.shard(key)

	sh.mu.Lock()
	defer sh.mu.Unlock()

	sh.buckets[key] = &rateLimitBucket{
		tokens:    tokens,
		interval:  interval,
		remaining: tokens,
		resetAt:   now.Add(interval),
	}

	return nil
}

func (s *inMemoryRateLimitStore) Burst(ctx context.Context, key string, tokens uint64) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if s.closed.Load() {
		return limiter.ErrStopped
	}

	now := time.Now().UTC()
	sh := s.shard(key)

	sh.mu.Lock()
	defer sh.mu.Unlock()

	b := s.ensureBucketLocked(sh, key, now)
	b.remaining += tokens

	return nil
}

func (s *inMemoryRateLimitStore) Close(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for key := range sh.buckets {
			delete(sh.buckets, key)
		}
		sh.mu.Unlock()
	}

	return nil
}

func (s *inMemoryRateLimitStore) ensureBucketLocked(sh *rateLimitShard, key string, now time.Time) *rateLimitBucket {
	b, ok := sh.buckets[key]
	if !ok {
		b = &rateLimitBucket{
			tokens:    s.defaultTokens,
			interval:  s.defaultInterval,
			remaining: s.defaultTokens,
			resetAt:   now.Add(s.defaultInterval),
		}
		sh.buckets[key] = b
		return b
	}

	if !now.Before(b.resetAt) {
		b.remaining = b.tokens
		b.resetAt = now.Add(b.interval)
	}

	return b
}

func sweepShardLocked(sh *rateLimitShard, now time.Time) {
	for key, bucket := range sh.buckets {
		if now.After(bucket.resetAt.Add(bucket.interval)) {
			delete(sh.buckets, key)
		}
	}
}

// fnv1aHash computes an FNV-1a hash of the string.
func fnv1aHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

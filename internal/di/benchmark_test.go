package di_test

import (
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// BenchmarkResolve_Singleton_Cached measures the cached singleton resolve path:
// reflect.TypeFor → findRegistration (RLock + map hit) → sync.Once no-op.
func BenchmarkResolve_Singleton_Cached(b *testing.B) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)
	c.MustResolve[*SimpleService]() // prime the singleton

	b.ReportAllocs()
	for b.Loop() {
		c.MustResolve[*SimpleService]()
	}
}

// BenchmarkResolve_ProvideValue measures the ProvideValue fast path
// (value pre-cached in singletonEntry at registration time).
func BenchmarkResolve_ProvideValue(b *testing.B) {
	c := di.New()
	svc := &SimpleService{Value: "bench"}
	c.MustProvideValue[*SimpleService](svc)

	b.ReportAllocs()
	for b.Loop() {
		c.MustResolve[*SimpleService]()
	}
}

// BenchmarkResolve_Parallel stresses RWMutex contention on the container
// under concurrent singleton resolution.
func BenchmarkResolve_Parallel(b *testing.B) {
	c := di.New()
	c.MustProvide[*SimpleService](NewSimpleService)
	c.MustResolve[*SimpleService]() // prime

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.MustResolve[*SimpleService]()
		}
	})
}

// BenchmarkResolveAll_Cached measures ordered interface collection resolution.
func BenchmarkResolveAll_Cached(b *testing.B) {
	c := di.New()
	c.MustProvide[*englishGreeter](NewEnglishGreeter)
	c.MustProvide[*frenchGreeter](NewFrenchGreeter)
	c.MustBindMany[Greeter, *englishGreeter]()
	c.MustBindMany[Greeter, *frenchGreeter]()
	c.MustResolveAll[Greeter]() // prime member singletons

	b.ReportAllocs()
	for b.Loop() {
		_, _ = c.ResolveAll[Greeter]()
	}
}

package credo

import (
	"context"
	"log/slog"
	"net/http"
)

// The Context owns the request's current context.Context, its head. A value
// set with [Context.Set] is a context.WithValue node over the head, so stdlib
// code reads it with the standard Value; a layer that derives a context
// (a deadline, a span) makes it the head of the inner chain through
// [Context.Within], and the head reverts when that layer returns.
//
// Bookkeeping:
//
//   - values records every Set, one entry per key with its latest value and
//     the sequence number of its latest Set. A scope reapplies, when it ends,
//     the entries set inside it over the outer head, so a value set inside a
//     layer — a user, a tenant — reaches the layers outside it.
//   - scope is the innermost open scope: its base (the context the scope
//     started from), the sequence number at its start and the nodes the
//     Context built over the base. Once the nodes outnumber twice the keys set
//     in the scope, they are rebuilt from the record, so one key set again and
//     again, as a streaming loop may set one, never lengthens the chain
//     without bound.
//   - The reapplication is lazy: a scope that ends marks the entries set
//     inside it as pending, and the first read of the head (Context, Std, a
//     Set, a new scope) builds their nodes. Nested scopes that end in turn
//     with no read between them build the nodes once, at the outermost; Get
//     and GetUser read a pending value from the record without building any.
//
// No node is ever modified, so a context handed out earlier — to a goroutine,
// to a stdlib handler — keeps the values it had.

// valueEntry is one key's latest Set.
type valueEntry struct {
	key any
	val any
	seq uint64
}

// headScope is the bookkeeping of the innermost open scope.
type headScope struct {
	// base is the context the scope started from: the request's own context
	// at the root, the context given to Within inside a scope.
	base context.Context
	// start is the Set sequence number when the scope opened; entries with a
	// greater one were set inside it.
	start uint64
	// nodes counts the nodes the Context built over base.
	nodes int
	// last is the node the Context built last, lastKey its key and
	// lastParent the context it was built over. A Set of lastKey while last
	// is still the head replaces that node instead of adding one.
	last       context.Context
	lastKey    any
	lastParent context.Context
}

// initHead makes ctx the head of a new request and forgets every value.
func (c *Context) initHead(r *http.Request) {
	c.head = r.Context()
	c.scope = headScope{base: c.head}
	clear(c.values)
	c.values = c.values[:0]
	c.seq = 0
	c.pending = false
	c.headGen++
	c.installed = r
	c.installedGen = c.headGen
}

// Set stores val under key for the rest of the request. The value is a node
// over the request's context, so [Context.Context], [Request.Context] and
// every context derived from them after the call carry it, and stdlib code
// reads it with ctx.Value(key). Setting a key again replaces its value.
//
// A value set inside a scope — [Context.Within], a stdlib middleware wrapped
// with [WrapStdMiddleware] — outlives the scope: when the scope ends, the
// values set inside it are applied again over the outer context. It records a
// fact about the request; a value that must end with its scope, such as a
// transaction, belongs in the context given to Within.
//
// Set panics for a zero Key.
func (c *Context) Set[T any](key Key[T], val T) {
	key.mustID("Context.Set")
	c.setValue(key, val)
}

// Get returns the value set under key and reports whether one was set. It reads
// the request's current context, so a value a stdlib middleware put there with
// the key is found too, while a layer that passed on a context not derived from
// the request's hides the values set before it. A nil interface value reports
// false.
//
// Get panics for a zero Key.
func (c *Context) Get[T any](key Key[T]) (T, bool) {
	key.mustID("Context.Get")
	v, ok := c.lookup(key).(T)
	return v, ok
}

// lookup reads key's value as the head carries it. A value pending
// reapplication (see materialize) is read from the record: it was set after
// every node of the head, so it is the head's value once materialized.
func (c *Context) lookup(key any) any {
	if c.pending {
		return c.lookupPending(key)
	}
	return c.head.Value(key)
}

func (c *Context) lookupPending(key any) any {
	for i := range c.values {
		if e := &c.values[i]; e.key == key && e.seq > c.pendingStart {
			return e.val
		}
	}
	return c.head.Value(key)
}

// materialize builds the nodes of the values pending reapplication over the
// head. It runs before anything reads or extends the head itself.
func (c *Context) materialize() {
	if c.pending {
		c.materializePending()
	}
}

func (c *Context) materializePending() {
	c.pending = false
	for i := range c.values {
		if e := c.values[i]; e.seq > c.pendingStart {
			c.push(e.key, e.val)
		}
	}
}

// setValue records val under key and pushes its node onto the head.
func (c *Context) setValue(key, val any) {
	c.materialize()
	c.seq++
	c.record(key, val, c.seq)
	c.push(key, val)
}

// record keeps key's latest value and sequence number.
func (c *Context) record(key, val any, seq uint64) {
	for i := range c.values {
		if c.values[i].key == key {
			c.values[i].val, c.values[i].seq = val, seq
			return
		}
	}
	c.values = append(c.values, valueEntry{key: key, val: val, seq: seq})
}

// push builds key's node over the head. When the head is the node the
// Context built last, for the same key, the new node replaces it: it is built
// over that node's parent, so the chain does not grow.
func (c *Context) push(key, val any) {
	s := &c.scope
	c.headGen++
	if s.last != nil && s.lastKey == key && c.headIsLast() {
		n := context.WithValue(s.lastParent, key, val)
		c.head, s.last = n, n
		return
	}
	parent := c.head
	n := context.WithValue(parent, key, val)
	c.head = n
	s.last, s.lastKey, s.lastParent = n, key, parent
	s.nodes++
	if s.nodes > 2 && s.nodes > 2*c.keysSince(s.start) {
		c.rebuild()
	}
}

// headIsLast reports whether the head is still the node the Context built
// last. The head changes only through push, rebuild and the scopes, which
// reset the scope record, so the comparison is between contexts the Context
// built itself: *valueCtx pointers, always comparable.
func (c *Context) headIsLast() bool {
	return c.head == c.scope.last
}

// keysSince counts the keys whose latest Set came after seq.
func (c *Context) keysSince(seq uint64) int {
	n := 0
	for i := range c.values {
		if c.values[i].seq > seq {
			n++
		}
	}
	return n
}

// rebuild replaces the nodes of the current scope with one node per key set
// in it, built over the scope's base from the record.
func (c *Context) rebuild() {
	s := &c.scope
	head := s.base
	s.nodes, s.last, s.lastKey, s.lastParent = 0, nil, nil, nil
	for i := range c.values {
		e := &c.values[i]
		if e.seq <= s.start {
			continue
		}
		parent := head
		head = context.WithValue(parent, e.key, e.val)
		s.nodes++
		s.last, s.lastKey, s.lastParent = head, e.key, parent
	}
	c.head = head
	c.headGen++
}

// Within runs next with ctx as the request's context and reverts when next
// returns or panics. It is how a layer opens a scope: inside it,
// [Context.Context], [Request.Context] and [Request.Std] carry ctx, so a
// deadline or a span ctx holds reaches the handler and everything it calls.
//
//	func Deadline(d time.Duration) credo.Middleware {
//		return func(next credo.Handler) credo.Handler {
//			return func(c *credo.Context) error {
//				ctx, cancel := context.WithTimeout(c.Context(), d)
//				defer cancel()
//				return c.Within(ctx, next)
//			}
//		}
//	}
//
// ctx is derived from c.Context(); one that is not cuts the inner chain off
// from the values set before it, through [Context.Get] as through Value. When
// next returns, the request's context is the outer one again — a deadline or a
// span ends with its scope — and the values set inside the scope with
// [Context.Set] are applied again over it: a user or a tenant is a fact about
// the request, not about one layer. A value the scope owns, such as a
// transaction, goes into ctx and ends with it.
//
// The embedded *http.Request is kept in step: Within installs one copy of it
// that carries ctx and restores the outer request afterwards. Assigning
// c.Request().Request a request with a new context does not change the
// request's context; in debug mode the Context reports such a request once
// per route.
func (c *Context) Within(ctx context.Context, next Handler) error {
	if ctx == nil {
		panic("credo: Context.Within: nil context.Context; derive it from c.Context()")
	}
	c.noteForeignRequest()
	return c.within(ctx, c.request.Request.WithContext(ctx), next)
}

// within opens a scope whose head is ctx and whose embedded request is req,
// which already carries ctx, runs next and closes the scope.
func (c *Context) within(ctx context.Context, req *http.Request, next Handler) error {
	c.materialize()
	outerHead, outerScope := c.head, c.scope
	outerReq, outerInstalled, outerGen := c.request.Request, c.installed, c.installedGen
	c.head = ctx
	c.scope = headScope{base: ctx, start: c.seq}
	c.headGen++
	c.request.Request = req
	c.installed, c.installedGen = req, c.headGen
	defer c.leave(outerHead, outerScope, outerReq, outerInstalled, outerGen)
	return next(c)
}

// leave closes the innermost scope: it restores the outer head and the outer
// embedded request, and marks the values set inside the scope for
// reapplication over the outer head (see materialize). It runs deferred, so
// after a panic the values are in place for the error rendering and the access
// record.
func (c *Context) leave(head context.Context, scope headScope, req, installed *http.Request, gen uint64) {
	c.noteForeignRequest()
	start := c.scope.start
	// A form the chain parsed on the scope's request copy stays with the
	// request that outlives it, which owns the cleanup of its files.
	handOverMultipartForm(req, c.request.Request)
	c.request.Request = req
	c.installed, c.installedGen = installed, gen
	c.head, c.scope = head, scope
	c.headGen++
	// A reapplication still pending from a scope nested in this one covers a
	// subset of this one's values: both are set after start.
	if c.keysSince(start) > 0 {
		c.pending, c.pendingStart = true, start
	}
}

// Std returns the standard request carrying the request's current context,
// [Context.Context]. The call may replace the embedded *http.Request: when
// the embedded request already carries the context, Std returns it, otherwise
// it installs a copy that does and returns the copy, so a second call without
// a new [Context.Set] copies nothing. Hand Std's result to stdlib code; a
// multipart form that stdlib code parses on it stays under the request-end
// cleanup.
func (r *Request) Std() *http.Request {
	c := r.owner
	if c == nil {
		return r.Request
	}
	c.noteForeignRequest()
	c.materialize()
	if r.Request == c.installed && c.installedGen == c.headGen {
		return r.Request
	}
	nr := r.Request.WithContext(c.head)
	r.Request = nr
	c.installed, c.installedGen = nr, c.headGen
	return nr
}

// Context returns the request's current context.Context, the one
// [Context.Context] returns: it carries the values set with [Context.Set] and
// the deadline or span of the scope it is read in. It shadows the embedded
// *http.Request's Context, which returns the context that request was built
// with and lags behind after a Set.
func (r *Request) Context() context.Context {
	if r.owner != nil {
		return r.owner.Context()
	}
	return r.Request.Context()
}

// noteForeignRequest notices an embedded request the Context did not
// install: code assigned c.Request().Request. The assignment does not move the
// head, which only Within and the stdlib boundary do; the request stays
// embedded, and the next Std copies it with the head. In debug mode the first
// such request on a route is reported once.
func (c *Context) noteForeignRequest() {
	if c.request.Request != c.installed {
		c.foreignRequest()
	}
}

func (c *Context) foreignRequest() {
	c.installed = c.request.Request
	c.installedGen = 0 // never equal to headGen: the next Std installs a copy
	if c.app == nil || !c.app.debug {
		return
	}
	flag := &c.app.foreignRequestReported
	pattern := ""
	if c.route != nil {
		flag = &c.route.foreignRequestReported
		pattern = c.route.method + " " + c.route.pattern
	}
	if flag.CompareAndSwap(false, true) {
		c.baseLogger().LogAttrs(c.head, slog.LevelWarn,
			"credo: c.Request().Request was replaced; the request's context is unchanged. "+
				"Open a scope with c.Within(ctx, next) to change the context, and take the standard request with c.Request().Std()",
			slog.String("route", pattern))
	}
}

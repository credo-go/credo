package di_test

import (
	"testing"

	"github.com/credo-go/credo/internal/di"
)

// BenchmarkResolve_Singleton_Cached measures the cached singleton resolve path:
// reflect.TypeFor → findRegistration (RLock + map hit) → sync.Once no-op.
func BenchmarkResolve_Singleton_Cached(b *testing.B) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)
	di.MustResolve[*SimpleService](c) // prime the singleton

	b.ReportAllocs()
	for b.Loop() {
		di.MustResolve[*SimpleService](c)
	}
}

// BenchmarkResolve_ProvideValue measures the ProvideValue fast path
// (value pre-cached in singletonEntry at registration time).
func BenchmarkResolve_ProvideValue(b *testing.B) {
	c := di.New()
	svc := &SimpleService{Value: "bench"}
	di.MustProvideValue[*SimpleService](c, svc)

	b.ReportAllocs()
	for b.Loop() {
		di.MustResolve[*SimpleService](c)
	}
}

// BenchmarkResolve_Parallel stresses RWMutex contention on the container
// under concurrent singleton resolution.
func BenchmarkResolve_Parallel(b *testing.B) {
	c := di.New()
	di.MustProvide[*SimpleService](c, NewSimpleService)
	di.MustResolve[*SimpleService](c) // prime

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			di.MustResolve[*SimpleService](c)
		}
	})
}

// BenchmarkResolveAll_Cached measures ordered interface collection resolution.
func BenchmarkResolveAll_Cached(b *testing.B) {
	c := di.New()
	di.MustProvide[*englishGreeter](c, NewEnglishGreeter)
	di.MustProvide[*frenchGreeter](c, NewFrenchGreeter)
	di.MustBindMany[Greeter, *englishGreeter](c)
	di.MustBindMany[Greeter, *frenchGreeter](c)
	di.MustResolveAll[Greeter](c) // prime member singletons

	b.ReportAllocs()
	for b.Loop() {
		_, _ = di.ResolveAll[Greeter](c)
	}
}

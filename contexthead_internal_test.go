package credo

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// chainNodes counts the nodes the Context built over the current scope's base.
func chainNodes(c *Context) int { return c.scope.nodes }

func TestContextHead_ChainStaysBounded(t *testing.T) {
	a := NewKey[int]("a")
	b := NewKey[int]("b")

	t.Run("one key set a thousand times keeps one node", func(t *testing.T) {
		c := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		for i := range 1000 {
			c.Set(a, i)
		}
		if n := chainNodes(c); n != 1 {
			t.Fatalf("nodes = %d, want 1", n)
		}
		if v, _ := c.Get(a); v != 999 {
			t.Fatalf("Get = %d, want the last value", v)
		}
	})

	t.Run("two keys set in turn keep at most four", func(t *testing.T) {
		c := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		most := 0
		for i := range 1000 {
			c.Set(a, i)
			c.Set(b, -i)
			most = max(most, chainNodes(c))
		}
		if most > 4 {
			t.Fatalf("nodes reached %d, want at most 4", most)
		}
		va, _ := c.Get(a)
		vb, _ := c.Get(b)
		if va != 999 || vb != -999 {
			t.Fatalf("Get = %d, %d, want the last values", va, vb)
		}
	})

	t.Run("a scope reapplying its values keeps the outer chain bounded", func(t *testing.T) {
		c := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		most := 0
		for i := range 1000 {
			_ = c.Within(c.Context(), func(c *Context) error {
				c.Set(a, i)
				return nil
			})
			c.Set(b, i)
			most = max(most, chainNodes(c))
		}
		if most > 4 {
			t.Fatalf("nodes reached %d, want at most 4", most)
		}
	})
}

func TestContextHead_ReapplyIsLazy(t *testing.T) {
	a := NewKey[string]("a")
	c := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	_ = c.Within(c.Context(), func(c *Context) error {
		return c.Within(c.Context(), func(c *Context) error {
			c.Set(a, "inner")
			return nil
		})
	})
	if n := chainNodes(c); n != 0 {
		t.Fatalf("nodes after the scopes ended = %d, want 0 before a read", n)
	}
	if v, _ := c.Get(a); v != "inner" {
		t.Fatalf("Get before materializing = %q, want %q", v, "inner")
	}
	if n := chainNodes(c); n != 0 {
		t.Fatalf("Get built %d nodes, want 0", n)
	}
	if v, _ := c.Context().Value(a).(string); v != "inner" {
		t.Fatalf("Context().Value = %q, want %q", v, "inner")
	}
	if n := chainNodes(c); n != 1 {
		t.Fatalf("nodes after the first read = %d, want 1: the nested scopes reapply once", n)
	}
}

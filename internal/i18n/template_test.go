package i18n

import (
	"fmt"
	"sync"
	"testing"
)

func TestTemplate_FastPath_NoTemplateVars(t *testing.T) {
	tpl, err := newTemplate("is required")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := tpl.execute(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != "is required" {
		t.Errorf("got %q, want %q", s, "is required")
	}
}

func TestTemplate_WithData(t *testing.T) {
	tpl, err := newTemplate("must be between {{.min}} and {{.max}} characters")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := tpl.execute(map[string]any{"min": 2, "max": 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "must be between 2 and 100 characters"
	if s != want {
		t.Errorf("got %q, want %q", s, want)
	}
}

func TestTemplate_MissingKey(t *testing.T) {
	tpl, err := newTemplate("value is {{.missing}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := tpl.execute(map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// missingkey=default produces empty string for missing keys
	want := "value is <no value>"
	if s != want {
		t.Errorf("got %q, want %q", s, want)
	}
}

func TestTemplate_NilData(t *testing.T) {
	tpl, err := newTemplate("{{.foo}} bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, err := tpl.execute(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With nil data, template variables resolve to <no value>
	want := "<no value> bar"
	if s != want {
		t.Errorf("got %q, want %q", s, want)
	}
}

func TestTemplate_ParseError(t *testing.T) {
	_, err := newTemplate("{{.unclosed")
	if err == nil {
		t.Error("expected parse error from newTemplate")
	}
}

func TestTemplate_ConcurrentExecute(t *testing.T) {
	tpl, err := newTemplate("hello {{.name}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			want := fmt.Sprintf("hello user-%d", i)
			data := map[string]any{"name": fmt.Sprintf("user-%d", i)}
			for range 200 {
				got, err := tpl.execute(data)
				if err != nil || got != want {
					t.Errorf("execute() = %q, %v; want %q", got, err, want)
					return
				}
			}
		})
	}
	wg.Wait()
}

func BenchmarkTemplateExecute(b *testing.B) {
	tpl, err := newTemplate("must be between {{.min}} and {{.max}} characters")
	if err != nil {
		b.Fatal(err)
	}
	data := map[string]any{"min": 2, "max": 100}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := tpl.execute(data); err != nil {
			b.Fatal(err)
		}
	}
}

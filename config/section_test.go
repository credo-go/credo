package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/credo-go/credo/config"
)

// mustGet reads one key of c through a Section, failing the test on error.
func mustGet[T any](t *testing.T, c config.RawConfig, key string) T {
	t.Helper()
	v, err := config.Section[T](key).Decode(c)
	if err != nil {
		t.Fatalf("Section[%T](%q).Decode: %v", v, key, err)
	}
	return v
}

type sectionDB struct {
	Host string
	Port int
}

// sectionApp validates itself, so Decode's validation path is exercised.
type sectionApp struct {
	Name string
}

func (a sectionApp) Validate() error {
	if a.Name == "" {
		return errors.New("name is required")
	}
	return nil
}

func loadSectionDoc(t *testing.T, doc string) *config.Config {
	t.Helper()
	c, err := config.LoadBytes([]byte(doc), config.FormatJSON, config.WithoutProcessEnv(), config.WithoutDotenv())
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	return c
}

func TestSectionDecode(t *testing.T) {
	c := loadSectionDoc(t, `{"database":{"host":"db","port":5432},"server":{"port":8080},"app":{"name":""}}`)

	t.Run("struct section", func(t *testing.T) {
		got, err := config.Section[sectionDB]("database").Decode(c)
		if err != nil || got != (sectionDB{Host: "db", Port: 5432}) {
			t.Errorf("Decode = %+v, %v; want {db 5432}", got, err)
		}
	})

	t.Run("primitive leaf", func(t *testing.T) {
		if got := mustGet[int](t, c, "server.port"); got != 8080 {
			t.Errorf("server.port = %d, want 8080", got)
		}
	})

	t.Run("missing section names the key", func(t *testing.T) {
		got, err := config.Section[sectionDB]("does.not.exist").Decode(c)
		if err == nil || !strings.Contains(err.Error(), `"does.not.exist"`) || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("Decode = %v, want a not-found error naming the key", err)
		}
		if got != (sectionDB{}) {
			t.Errorf("error path returned %+v, want the zero value", got)
		}
	})

	t.Run("failed validation names the key", func(t *testing.T) {
		_, err := config.Section[sectionApp]("app").Decode(c)
		if err == nil || !strings.Contains(err.Error(), `section "app"`) || !strings.Contains(err.Error(), "name is required") {
			t.Fatalf("Decode = %v, want the validation error naming section \"app\"", err)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		_, err := config.Section[sectionDB]("").Decode(c)
		if err == nil {
			t.Fatal("Decode with an empty key: expected an error, got nil")
		}
		for _, part := range []string{"Section.Decode", "empty section key", `config.Section[T]("app")`} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("error %q does not contain %q", err, part)
			}
		}
	})
}

// customRaw is a RawConfig of the application's own, which Decode reads
// through its Unmarshal like any other.
type customRaw struct{}

func (customRaw) Unmarshal(key string, dst any) error {
	if key != "port" {
		return errors.New("absent")
	}
	*(dst.(*int)) = 7
	return nil
}

func (customRaw) Exists(key string) bool { return key == "port" }

func TestSectionDecodeCustomRawConfig(t *testing.T) {
	if got := mustGet[int](t, customRaw{}, "port"); got != 7 {
		t.Errorf("port = %d, want 7", got)
	}
}

func TestSectionExists(t *testing.T) {
	c := loadSectionDoc(t, `{"database":{"host":"db","port":5432},"app":{"name":""},"flag":false}`)

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"a present section", config.Section[sectionDB]("database").Exists(c), true},
		{"a present leaf", config.Section[int]("database.port").Exists(c), true},
		{"a present leaf holding a zero value", config.Section[bool]("flag").Exists(c), true},
		{"a missing section", config.Section[sectionDB]("cache").Exists(c), false},
		{"a missing leaf of a present section", config.Section[string]("database.user").Exists(c), false},
		// Exists decodes nothing: the section fails validation where it is
		// decoded, and is present all the same.
		{"a present section that fails validation", config.Section[sectionApp]("app").Exists(c), true},
		{"a present section named with another type", config.Section[int]("database").Exists(c), true},
		{"an empty key", config.Section[sectionDB]("").Exists(c), false},
		{"a custom RawConfig, present", config.Section[int]("port").Exists(customRaw{}), true},
		{"a custom RawConfig, missing", config.Section[int]("host").Exists(customRaw{}), false},
		{"a custom RawConfig, empty key", config.Section[int]("").Exists(wholeTreeRaw{}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("Exists = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

// wholeTreeRaw reports every key present, the empty one included, as a
// RawConfig that treats "" as the whole tree would.
type wholeTreeRaw struct{}

func (wholeTreeRaw) Unmarshal(string, any) error { return nil }
func (wholeTreeRaw) Exists(string) bool          { return true }

// Exists records no read, so a section that was probed is read as static or
// as live afterwards, and a section that was read either way is still probed.
func TestSectionExists_RecordsNoRead(t *testing.T) {
	const doc = `{"database":{"host":"db","port":5432}}`
	section := config.Section[sectionDB]("database")

	t.Run("probed, then read as live", func(t *testing.T) {
		c := loadSectionDoc(t, doc)
		if !section.Exists(c) {
			t.Fatal("Exists = false, want true")
		}
		if _, err := section.Live(c); err != nil {
			t.Fatalf("Live after Exists: %v", err)
		}
		if !section.Exists(c) {
			t.Error("Exists after Live = false, want true")
		}
	})

	t.Run("probed, then read as static", func(t *testing.T) {
		c := loadSectionDoc(t, doc)
		if !section.Exists(c) {
			t.Fatal("Exists = false, want true")
		}
		if _, err := section.Decode(c); err != nil {
			t.Fatalf("Decode after Exists: %v", err)
		}
		if !section.Exists(c) {
			t.Error("Exists after Decode = false, want true")
		}
	})
}

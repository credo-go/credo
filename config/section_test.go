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

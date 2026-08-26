package references_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/config"
)

type referenceConfig struct {
	Server struct {
		Host                  string
		Port                  int
		ReadTimeout           time.Duration
		WriteTimeout          time.Duration
		IdleTimeout           time.Duration
		ReadHeaderTimeout     time.Duration
		ShutdownTimeout       time.Duration
		ReloadTimeout         time.Duration
		MaxHeaderBytes        int
		MaxHeaderValueCount   int
		MaxBodyBytes          int64
		RedirectTrailingSlash bool
		Debug                 bool
		StrictBodies          bool
		TrustedProxies        []string
		TLS                   struct {
			CertFile string
			KeyFile  string
		}
	}
	I18n struct {
		Dir     string
		Default string
	}
	Databases map[string]struct {
		Driver         string
		Host           string
		Port           int
		Name           string
		User           string
		Password       string
		ConnectTimeout time.Duration
		MaxOpen        int
		MaxIdle        *int
		MaxIdleTime    time.Duration
		MaxLifetime    time.Duration
		SSLMode        string
		Options        map[string]string
	}
	App struct {
		Name        string
		Environment string
		Debug       bool
	}
}

func TestReferenceConfigsAreEquivalentAndAccepted(t *testing.T) {
	yamlConfig := loadReferenceConfig(t, "config/config.yaml", config.FormatYAML)
	jsonConfig := loadReferenceConfig(t, "config/config.json", config.FormatJSON)

	if !reflect.DeepEqual(yamlConfig, jsonConfig) {
		t.Fatalf("YAML and JSON reference configurations differ:\nYAML: %#v\nJSON: %#v", yamlConfig, jsonConfig)
	}
	if yamlConfig.I18n.Dir != "locales/" || yamlConfig.I18n.Default != "en" {
		t.Fatalf("unexpected i18n reference: %#v", yamlConfig.I18n)
	}
}

func TestReferenceLocaleCatalogsLoad(t *testing.T) {
	wants := map[string]struct {
		notFound            string
		contentTypeRequired string
	}{
		"en": {notFound: "Not found", contentTypeRequired: "Content-Type is required for QUERY requests."},
		"tr": {notFound: "Bulunamadı", contentTypeRequired: "QUERY istekleri için Content-Type zorunludur."},
	}
	for lang, want := range wants {
		t.Run(lang, func(t *testing.T) {
			raw, err := config.LoadBytes([]byte(`{}`), config.FormatJSON, isolatedConfigOptions(t)...)
			if err != nil {
				t.Fatal(err)
			}
			app, err := credo.New(credo.WithRawConfig(raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := app.UseI18n(credo.I18nConfig{Dir: "locales", Default: lang}); err != nil {
				t.Fatalf("load reference locales: %v", err)
			}
			app.GET("/", func(ctx *credo.Context) error {
				return ctx.Response().Text(http.StatusOK, ctx.T("not_found")+"|"+ctx.T("content_type_required"))
			})

			response := httptest.NewRecorder()
			app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			wantBody := want.notFound + "|" + want.contentTypeRequired
			if response.Code != http.StatusOK || response.Body.String() != wantBody {
				t.Fatalf("response = (%d, %q), want (%d, %q)", response.Code, response.Body.String(), http.StatusOK, wantBody)
			}
		})
	}
}

func loadReferenceConfig(t *testing.T, path, format string) referenceConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.LoadBytes(data, format, isolatedConfigOptions(t)...)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	var got referenceConfig
	if err := raw.Unmarshal("", &got); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if _, err := credo.New(credo.WithRawConfig(raw)); err != nil {
		t.Fatalf("construct app from %s: %v", path, err)
	}
	return got
}

func isolatedConfigOptions(t *testing.T) []config.Option {
	t.Helper()
	return []config.Option{
		config.WithPrefix("CREDO_REFERENCE_TEST_"),
		config.WithDotenvPath(filepath.Join(t.TempDir(), ".env")),
		config.WithDotenvOptional(),
		config.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

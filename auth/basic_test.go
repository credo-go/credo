package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/auth"
)

func TestBasicAuthenticator_Success(t *testing.T) {
	a, err := auth.NewBasicAuthenticator[string](auth.BasicConfig[string]{
		Validator: func(username, password string, r *http.Request) (string, bool, error) {
			if username == "alice" && password == "s3cret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("alice", "s3cret")

	user, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if user != "alice" {
		t.Errorf("user = %q, want alice", user)
	}
}

func TestBasicAuthenticator_MissingCredentials(t *testing.T) {
	a, err := auth.NewBasicAuthenticator[string](auth.BasicConfig[string]{
		Validator: func(username, password string, r *http.Request) (string, bool, error) {
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrBasicCredentialsMissing) {
		t.Fatalf("expected ErrBasicCredentialsMissing, got %v", err)
	}
}

func TestBasicAuthenticator_InvalidCredentials(t *testing.T) {
	a, err := auth.NewBasicAuthenticator[string](auth.BasicConfig[string]{
		Validator: func(username, password string, r *http.Request) (string, bool, error) {
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("alice", "wrong")

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrBasicCredentialsInvalid) {
		t.Fatalf("expected ErrBasicCredentialsInvalid, got %v", err)
	}
}

func TestBasicErrorHandler_AddsChallengeHeader(t *testing.T) {
	basicAuth, err := auth.NewBasicAuthenticator[string](auth.BasicConfig[string]{
		Validator: func(username, password string, r *http.Request) (string, bool, error) {
			if username == "alice" && password == "s3cret" {
				return "alice", true, nil
			}
			return "", false, nil
		},
		Realm: "Members",
	})
	if err != nil {
		t.Fatal(err)
	}

	app := mustNew(t)
	app.GET("/secure", func(ctx *credo.Context) error {
		return ctx.Response().NoContent(http.StatusOK)
	}).Middleware(auth.Middleware[string](basicAuth, auth.BasicErrorHandler(basicAuth.Realm())))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/secure", nil)
	app.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}

	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="Members"` {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, `Basic realm="Members"`)
	}
}

func TestBasicAuthenticator_Constructor_RequiresValidator(t *testing.T) {
	_, err := auth.NewBasicAuthenticator[string](auth.BasicConfig[string]{})
	if err == nil {
		t.Fatal("expected error when validator is nil")
	}
}

func TestBasicAuthenticator_Constructor_DefaultRealm(t *testing.T) {
	a, err := auth.NewBasicAuthenticator[string](auth.BasicConfig[string]{
		Validator: func(username, password string, r *http.Request) (string, bool, error) {
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := a.Realm(); got != "Restricted" {
		t.Fatalf("realm = %q, want Restricted", got)
	}
}

func TestBasicChallenge_DefaultRealm(t *testing.T) {
	got := auth.BasicChallenge("")
	want := `Basic realm="Restricted"`
	if got != want {
		t.Fatalf("challenge = %q, want %q", got, want)
	}
}

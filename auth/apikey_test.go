package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/credo-go/credo/auth"
)

func TestAPIKeyAuthenticator_HeaderSuccess(t *testing.T) {
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Validator: func(key string, r *http.Request) (string, bool, error) {
			if key == "k-123" {
				return "alice", true, nil
			}
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "k-123")

	user, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if user != "alice" {
		t.Errorf("user = %q, want alice", user)
	}
}

func TestAPIKeyAuthenticator_QueryFallback(t *testing.T) {
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Header: "",
		Query:  "api_key",
		Validator: func(key string, r *http.Request) (string, bool, error) {
			return key, key == "q-42", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/?api_key=q-42", nil)
	user, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}

	if user != "q-42" {
		t.Errorf("user = %q, want q-42", user)
	}
}

func TestAPIKeyAuthenticator_MissingKey(t *testing.T) {
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Validator: func(key string, r *http.Request) (string, bool, error) {
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrAPIKeyMissing) {
		t.Fatalf("expected ErrAPIKeyMissing, got %v", err)
	}
}

func TestAPIKeyAuthenticator_InvalidKey(t *testing.T) {
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Validator: func(key string, r *http.Request) (string, bool, error) {
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "wrong")

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrAPIKeyInvalid) {
		t.Fatalf("expected ErrAPIKeyInvalid, got %v", err)
	}
}

func TestAPIKeyAuthenticator_ValidatorError(t *testing.T) {
	validatorErr := errors.New("db unavailable")
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Validator: func(key string, r *http.Request) (string, bool, error) {
			return "", false, validatorErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "k")

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrAPIKeyInvalid) {
		t.Fatalf("expected ErrAPIKeyInvalid, got %v", err)
	}
	if !errors.Is(err, validatorErr) {
		t.Fatalf("expected wrapped validator error, got %v", err)
	}
}

func TestAPIKeyAuthenticator_Constructor_RequiresValidator(t *testing.T) {
	_, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{})
	if err == nil {
		t.Fatal("expected error when validator is nil")
	}
}

func TestAPIKeyAuthenticator_ExtractsHeaderWithPrefix(t *testing.T) {
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Header: "Authorization",
		Prefix: "ApiKey",
		Validator: func(key string, r *http.Request) (string, bool, error) {
			return key, key == "k-123", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "apikey\tk-123")

	user, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if user != "k-123" {
		t.Fatalf("user = %q, want k-123", user)
	}
}

func TestAPIKeyAuthenticator_FallsBackToQueryWhenHeaderValuesInvalid(t *testing.T) {
	a, err := auth.NewAPIKeyAuthenticator[string](auth.APIKeyConfig[string]{
		Header: "Authorization",
		Prefix: "ApiKey",
		Query:  "api_key",
		Validator: func(key string, r *http.Request) (string, bool, error) {
			return key, key == "q-42", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/?api_key=q-42", nil)
	r.Header.Add("Authorization", "Bearer abc")
	r.Header.Add("Authorization", "   ")

	user, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if user != "q-42" {
		t.Fatalf("user = %q, want q-42", user)
	}
}

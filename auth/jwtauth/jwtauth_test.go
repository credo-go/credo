package jwtauth_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/credo-go/credo/auth/jwtauth"
)

func TestAuthenticator_Success_DefaultClaims(t *testing.T) {
	secret := []byte("top-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{"sub": "42", "role": "admin"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	claims, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}

	if claims["sub"] != "42" {
		t.Errorf("sub = %v, want 42", claims["sub"])
	}
}

func TestAuthenticator_MissingToken(t *testing.T) {
	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: []byte("top-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	_, err = a.Authenticate(r)
	if !errors.Is(err, jwtauth.ErrTokenMissing) {
		t.Fatalf("expected ErrTokenMissing, got %v", err)
	}
}

func TestAuthenticator_InvalidSignature(t *testing.T) {
	signed := mustSignJWT(t, []byte("issuer-secret"), jwt.MapClaims{"sub": "42"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: []byte("different-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if !errors.Is(err, jwtauth.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestAuthenticator_QueryFallback(t *testing.T) {
	secret := []byte("query-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{"sub": "7"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
		Header:     "",
		Query:      "token",
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/?token="+signed, nil)
	claims, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}

	if claims["sub"] != "7" {
		t.Errorf("sub = %v, want 7", claims["sub"])
	}
}

func TestAuthenticator_CustomParseUser(t *testing.T) {
	type claims struct {
		Tenant string `json:"tenant"`
		jwt.RegisteredClaims
	}

	secret := []byte("tenant-secret")
	signed := mustSignJWT(t, secret, &claims{
		Tenant: "acme",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "user-1",
		},
	})

	a, err := jwtauth.NewAuthenticator[string](jwtauth.Config[string]{
		SigningKey: secret,
		Advanced: jwtauth.Advanced[string]{
			NewClaims: func() jwt.Claims {
				return &claims{}
			},
			ParseToken: func(token *jwt.Token) (string, error) {
				c, ok := token.Claims.(*claims)
				if !ok {
					return "", fmt.Errorf("unexpected claims type %T", token.Claims)
				}
				return c.Subject + "@" + c.Tenant, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	user, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}

	if user != "user-1@acme" {
		t.Errorf("user = %q, want user-1@acme", user)
	}
}

func TestAuthenticator_CookieFallback(t *testing.T) {
	secret := []byte("cookie-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{"sub": "cookie-user"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
		Header:     "",
		Cookie:     "jwt_token",
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "jwt_token", Value: signed})

	claims, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "cookie-user" {
		t.Fatalf("sub = %v, want cookie-user", claims["sub"])
	}
}

func TestAuthenticator_SigningKeys_KnownKid(t *testing.T) {
	fallback := []byte("fallback")
	k1 := []byte("kid-1-secret")
	signed := mustSignJWTWithKid(t, k1, "kid-1", jwt.MapClaims{"sub": "1"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey:  fallback,
		SigningKeys: map[string]any{"kid-1": k1},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	claims, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "1" {
		t.Fatalf("sub = %v, want 1", claims["sub"])
	}
}

func TestAuthenticator_SigningKeys_MissingKid_UsesSigningKeyFallback(t *testing.T) {
	fallback := []byte("fallback-key")
	k1 := []byte("kid-1-secret")
	signed := mustSignJWT(t, fallback, jwt.MapClaims{"sub": "fallback"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey:  fallback,
		SigningKeys: map[string]any{"kid-1": k1},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	claims, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "fallback" {
		t.Fatalf("sub = %v, want fallback", claims["sub"])
	}
}

func TestAuthenticator_SigningKeys_MissingKid_NoFallbackKey(t *testing.T) {
	k1 := []byte("kid-1-secret")
	signed := mustSignJWT(t, k1, jwt.MapClaims{"sub": "x"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKeys: map[string]any{"kid-1": k1},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if !errors.Is(err, jwtauth.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestAuthenticator_SigningKeys_UnknownKid(t *testing.T) {
	fallback := []byte("fallback-key")
	k1 := []byte("kid-1-secret")
	unknown := []byte("unknown-kid-secret")
	signed := mustSignJWTWithKid(t, unknown, "kid-x", jwt.MapClaims{"sub": "x"})

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey:  fallback,
		SigningKeys: map[string]any{"kid-1": k1},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if !errors.Is(err, jwtauth.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestAuthenticator_Constructor_RequiresSigningKeyOrKeyFunc(t *testing.T) {
	_, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{})
	want := "jwtauth: Config has no signing key; set SigningKey, SigningKeys or Advanced.KeyFunc"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestAuthenticator_Constructor_UsesProvidedKeyFunc(t *testing.T) {
	called := false
	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		Advanced: jwtauth.Advanced[jwt.MapClaims]{
			KeyFunc: func(token *jwt.Token) (any, error) {
				called = true
				return []byte("top-secret"), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	signed := mustSignJWT(t, []byte("top-secret"), jwt.MapClaims{"sub": "42"})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected provided keyfunc to be called")
	}
}

func TestAuthenticator_ParseClaims_SimplePath(t *testing.T) {
	secret := []byte("simple-secret")
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	signed := mustSignJWT(t, secret, jwt.MapClaims{
		"sub":  "user-1",
		"role": "admin",
		"exp":  exp.Unix(),
	})

	type user struct {
		ID   string
		Role string
		Exp  time.Time
	}

	a, err := jwtauth.NewAuthenticator[user](jwtauth.Config[user]{
		SigningKey: secret,
		ParseClaims: func(claims jwtauth.Claims) (user, error) {
			return user{
				ID:   claims.Subject(),
				Role: claims.GetString("role"),
				Exp:  claims.ExpiresAt(),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	u, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "user-1" || u.Role != "admin" {
		t.Errorf("user = %+v, want ID=user-1 Role=admin", u)
	}
	// ExpiresAt must surface as time.Time, not the raw float64 the JSON
	// decoder produces for exp.
	if !u.Exp.Equal(exp) {
		t.Errorf("Exp = %s, want %s", u.Exp, exp)
	}
}

func TestAuthenticator_Constructor_ParseClaimsAndParseTokenConflict(t *testing.T) {
	_, err := jwtauth.NewAuthenticator[string](jwtauth.Config[string]{
		SigningKey:  []byte("secret"),
		ParseClaims: func(jwtauth.Claims) (string, error) { return "", nil },
		Advanced: jwtauth.Advanced[string]{
			ParseToken: func(*jwt.Token) (string, error) { return "", nil },
		},
	})
	want := "jwtauth: Config sets both ParseClaims and Advanced.ParseToken; set one of them"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestAuthenticator_Issuer(t *testing.T) {
	secret := []byte("issuer-secret")

	a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
		Issuer:     "credo-idp",
	})
	if err != nil {
		t.Fatal(err)
	}

	matching := mustSignJWT(t, secret, jwt.MapClaims{"iss": "credo-idp", "sub": "1"})
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+matching)
	if _, err := a.Authenticate(r); err != nil {
		t.Fatalf("matching issuer rejected: %v", err)
	}

	wrong := mustSignJWT(t, secret, jwt.MapClaims{"iss": "evil-idp", "sub": "1"})
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+wrong)
	if _, err := a.Authenticate(r); !errors.Is(err, jwtauth.ErrTokenInvalid) {
		t.Fatalf("wrong issuer: expected ErrTokenInvalid, got %v", err)
	}
}

func TestAuthenticator_Audience_AnyOf(t *testing.T) {
	secret := []byte("aud-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{
		"sub": "1",
		"aud": []string{"api", "billing"},
	})

	cases := []struct {
		name     string
		audience []string
		wantOK   bool
	}{
		{name: "one of several matches", audience: []string{"billing", "reports"}, wantOK: true},
		{name: "exact single match", audience: []string{"api"}, wantOK: true},
		{name: "no overlap", audience: []string{"reports"}, wantOK: false},
		{name: "empty config skips check", audience: nil, wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
				SigningKey: secret,
				Audience:   tc.audience,
			})
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer "+signed)

			_, err = a.Authenticate(r)
			if tc.wantOK && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !tc.wantOK && !errors.Is(err, jwtauth.ErrTokenInvalid) {
				t.Fatalf("expected ErrTokenInvalid, got %v", err)
			}
		})
	}
}

func TestAuthenticator_Leeway(t *testing.T) {
	secret := []byte("leeway-secret")
	// Token expired 30 seconds ago.
	signed := mustSignJWT(t, secret, jwt.MapClaims{
		"sub": "1",
		"exp": time.Now().Add(-30 * time.Second).Unix(),
	})

	strict, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	if _, authErr := strict.Authenticate(r); !errors.Is(authErr, jwtauth.ErrTokenInvalid) {
		t.Fatalf("expired token without leeway: expected ErrTokenInvalid, got %v", authErr)
	}

	lenient, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
		Leeway:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	if _, authErr := lenient.Authenticate(r); authErr != nil {
		t.Fatalf("expired-within-leeway token rejected: %v", authErr)
	}
}

func TestAuthenticator_RequireExpiry(t *testing.T) {
	secret := []byte("exp-secret")
	noExp := mustSignJWT(t, secret, jwt.MapClaims{"sub": "1"})

	// Pin the golang-jwt v5 default: a token without exp is accepted.
	lax, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+noExp)
	if _, authErr := lax.Authenticate(r); authErr != nil {
		t.Fatalf("v5 default should accept exp-less token, got %v", authErr)
	}

	strict, err := jwtauth.NewAuthenticator[jwt.MapClaims](jwtauth.Config[jwt.MapClaims]{
		SigningKey:    secret,
		RequireExpiry: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+noExp)
	if _, err := strict.Authenticate(r); !errors.Is(err, jwtauth.ErrTokenInvalid) {
		t.Fatalf("RequireExpiry: expected ErrTokenInvalid for exp-less token, got %v", err)
	}
}

func TestClaims_ZeroValueAccessors(t *testing.T) {
	var claims jwtauth.Claims
	if claims.Subject() != "" || claims.Issuer() != "" || claims.GetString("x") != "" {
		t.Error("zero Claims string accessors should return empty strings")
	}
	if claims.Audience() != nil || claims.Get("x") != nil {
		t.Error("zero Claims slice/any accessors should return nil")
	}
	if !claims.ExpiresAt().IsZero() || !claims.IssuedAt().IsZero() || !claims.NotBefore().IsZero() {
		t.Error("zero Claims time accessors should return the zero time")
	}
}

func mustSignJWT(t *testing.T, key []byte, claims jwt.Claims) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}

func mustSignJWTWithKid(t *testing.T, key []byte, kid string, claims jwt.Claims) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign jwt with kid: %v", err)
	}
	return signed
}

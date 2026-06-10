package auth_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/credo-go/credo/auth"
	jwt "github.com/golang-jwt/jwt/v5"
)

func TestJWTAuthenticator_Success_DefaultClaims(t *testing.T) {
	secret := []byte("top-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{"sub": "42", "role": "admin"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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

func TestJWTAuthenticator_MissingToken(t *testing.T) {
	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey: []byte("top-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrJWTMissing) {
		t.Fatalf("expected ErrJWTMissing, got %v", err)
	}
}

func TestJWTAuthenticator_InvalidSignature(t *testing.T) {
	signed := mustSignJWT(t, []byte("issuer-secret"), jwt.MapClaims{"sub": "42"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey: []byte("different-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrJWTInvalid) {
		t.Fatalf("expected ErrJWTInvalid, got %v", err)
	}
}

func TestJWTAuthenticator_QueryFallback(t *testing.T) {
	secret := []byte("query-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{"sub": "7"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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

func TestJWTAuthenticator_CustomParseUser(t *testing.T) {
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

	a, err := auth.NewJWTAuthenticator[string](auth.JWTConfig[string]{
		SigningKey: secret,
		Advanced: auth.JWTAdvanced[string]{
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

func TestJWTAuthenticator_CookieFallback(t *testing.T) {
	secret := []byte("cookie-secret")
	signed := mustSignJWT(t, secret, jwt.MapClaims{"sub": "cookie-user"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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

func TestJWTAuthenticator_SigningKeys_KnownKid(t *testing.T) {
	fallback := []byte("fallback")
	k1 := []byte("kid-1-secret")
	signed := mustSignJWTWithKid(t, k1, "kid-1", jwt.MapClaims{"sub": "1"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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

func TestJWTAuthenticator_SigningKeys_MissingKid_UsesSigningKeyFallback(t *testing.T) {
	fallback := []byte("fallback-key")
	k1 := []byte("kid-1-secret")
	signed := mustSignJWT(t, fallback, jwt.MapClaims{"sub": "fallback"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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

func TestJWTAuthenticator_SigningKeys_MissingKid_NoFallbackKey(t *testing.T) {
	k1 := []byte("kid-1-secret")
	signed := mustSignJWT(t, k1, jwt.MapClaims{"sub": "x"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKeys: map[string]any{"kid-1": k1},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrJWTInvalid) {
		t.Fatalf("expected ErrJWTInvalid, got %v", err)
	}
}

func TestJWTAuthenticator_SigningKeys_UnknownKid(t *testing.T) {
	fallback := []byte("fallback-key")
	k1 := []byte("kid-1-secret")
	unknown := []byte("unknown-kid-secret")
	signed := mustSignJWTWithKid(t, unknown, "kid-x", jwt.MapClaims{"sub": "x"})

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey:  fallback,
		SigningKeys: map[string]any{"kid-1": k1},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = a.Authenticate(r)
	if !errors.Is(err, auth.ErrJWTInvalid) {
		t.Fatalf("expected ErrJWTInvalid, got %v", err)
	}
}

func TestJWTAuthenticator_Constructor_RequiresSigningKeyOrKeyFunc(t *testing.T) {
	_, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{})
	if err == nil {
		t.Fatal("expected error when no signing key, keys, or keyfunc are provided")
	}
}

func TestJWTAuthenticator_Constructor_UsesProvidedKeyFunc(t *testing.T) {
	called := false
	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		Advanced: auth.JWTAdvanced[jwt.MapClaims]{
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

func TestJWTAuthenticator_ParseClaims_SimplePath(t *testing.T) {
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

	a, err := auth.NewJWTAuthenticator[user](auth.JWTConfig[user]{
		SigningKey: secret,
		ParseClaims: func(claims auth.JWTClaims) (user, error) {
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

func TestJWTAuthenticator_Constructor_ParseClaimsAndParseTokenConflict(t *testing.T) {
	_, err := auth.NewJWTAuthenticator[string](auth.JWTConfig[string]{
		SigningKey:  []byte("secret"),
		ParseClaims: func(auth.JWTClaims) (string, error) { return "", nil },
		Advanced: auth.JWTAdvanced[string]{
			ParseToken: func(*jwt.Token) (string, error) { return "", nil },
		},
	})
	if err == nil {
		t.Fatal("expected constructor error when both ParseClaims and Advanced.ParseToken are set")
	}
}

func TestJWTAuthenticator_Issuer(t *testing.T) {
	secret := []byte("issuer-secret")

	a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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
	if _, err := a.Authenticate(r); !errors.Is(err, auth.ErrJWTInvalid) {
		t.Fatalf("wrong issuer: expected ErrJWTInvalid, got %v", err)
	}
}

func TestJWTAuthenticator_Audience_AnyOf(t *testing.T) {
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
			a, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
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
			if !tc.wantOK && !errors.Is(err, auth.ErrJWTInvalid) {
				t.Fatalf("expected ErrJWTInvalid, got %v", err)
			}
		})
	}
}

func TestJWTAuthenticator_Leeway(t *testing.T) {
	secret := []byte("leeway-secret")
	// Token expired 30 seconds ago.
	signed := mustSignJWT(t, secret, jwt.MapClaims{
		"sub": "1",
		"exp": time.Now().Add(-30 * time.Second).Unix(),
	})

	strict, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	if _, err := strict.Authenticate(r); !errors.Is(err, auth.ErrJWTInvalid) {
		t.Fatalf("expired token without leeway: expected ErrJWTInvalid, got %v", err)
	}

	lenient, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey: secret,
		Leeway:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	if _, err := lenient.Authenticate(r); err != nil {
		t.Fatalf("expired-within-leeway token rejected: %v", err)
	}
}

func TestJWTAuthenticator_RequireExpiry(t *testing.T) {
	secret := []byte("exp-secret")
	noExp := mustSignJWT(t, secret, jwt.MapClaims{"sub": "1"})

	// Pin the golang-jwt v5 default: a token without exp is accepted.
	lax, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+noExp)
	if _, err := lax.Authenticate(r); err != nil {
		t.Fatalf("v5 default should accept exp-less token, got %v", err)
	}

	strict, err := auth.NewJWTAuthenticator[jwt.MapClaims](auth.JWTConfig[jwt.MapClaims]{
		SigningKey:    secret,
		RequireExpiry: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+noExp)
	if _, err := strict.Authenticate(r); !errors.Is(err, auth.ErrJWTInvalid) {
		t.Fatalf("RequireExpiry: expected ErrJWTInvalid for exp-less token, got %v", err)
	}
}

func TestJWTClaims_ZeroValueAccessors(t *testing.T) {
	var claims auth.JWTClaims
	if claims.Subject() != "" || claims.Issuer() != "" || claims.GetString("x") != "" {
		t.Error("zero JWTClaims string accessors should return empty strings")
	}
	if claims.Audience() != nil || claims.Get("x") != nil {
		t.Error("zero JWTClaims slice/any accessors should return nil")
	}
	if !claims.ExpiresAt().IsZero() || !claims.IssuedAt().IsZero() || !claims.NotBefore().IsZero() {
		t.Error("zero JWTClaims time accessors should return the zero time")
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

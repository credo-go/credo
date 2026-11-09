package jwtauth_test

import (
	"net/http/httptest"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/auth"
	"github.com/credo-go/credo/auth/jwtauth"
)

// The JWT authenticator implements auth's interface from its own package.
var _ auth.Authenticator[string] = (*jwtauth.Authenticator[string])(nil)

// auth.Middleware turns the authenticator into a middleware: a valid token
// stores the user, a missing one is a 401.
func TestAuthenticator_WithAuthMiddleware(t *testing.T) {
	secret := []byte("middleware-secret")
	a, err := jwtauth.NewAuthenticator(jwtauth.Config[string]{
		SigningKey:  secret,
		ParseClaims: func(claims jwtauth.Claims) (string, error) { return claims.Subject(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := credo.New()
	if err != nil {
		t.Fatal(err)
	}
	app.GET("/me", func(c *credo.Context) error {
		user, _ := c.GetUser[string]()
		return c.Response().Text(200, user)
	}).Middleware(auth.Middleware[string](a, nil))

	r := httptest.NewRequest("GET", "/me", nil)
	r.Header.Set("Authorization", "Bearer "+mustSignJWT(t, secret, jwt.MapClaims{"sub": "user-7"}))
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "user-7" {
		t.Errorf("with a token: %d %q, want 200 \"user-7\"", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/me", nil))
	if w.Code != 401 {
		t.Errorf("without a token: status = %d, want 401", w.Code)
	}
}

// The sentinels carry the package's prefix.
func TestErrors_Text(t *testing.T) {
	if got := jwtauth.ErrTokenMissing.Error(); got != "jwtauth: token is missing" {
		t.Errorf("ErrTokenMissing = %q", got)
	}
	if got := jwtauth.ErrTokenInvalid.Error(); got != "jwtauth: token is invalid" {
		t.Errorf("ErrTokenInvalid = %q", got)
	}
}

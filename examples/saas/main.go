// Package main demonstrates a full SaaS application built with the Credo framework.
//
// Features shown:
//   - Configuration loading (config.Load with YAML/JSON + env overrides)
//   - Global middleware (built-in recover/request ID/access log, plus CORS,
//     secure headers, compression)
//   - Authentication (JWT + API key)
//   - Route groups (public, authenticated, admin)
//   - Dependency injection (Provide/Resolve with typed constructors)
//   - Validation (programmatic rules, no struct tags)
//   - Error handling (RFC 7807 Problem Details)
//   - Graceful shutdown with OnShutdown hooks
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/auth"
	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/middleware"
	"github.com/credo-go/credo/validation"
)

// ---------------------------------------------------------------------------
// Configuration types (typed config via DI)
// ---------------------------------------------------------------------------

// AppConfig holds user-defined application settings.
type AppConfig struct {
	Name        string
	Environment string
	Debug       bool
}

// DatabaseConfig holds database connection settings. Field names map to
// snake_case config keys automatically (e.g. MaxOpen → "max_open",
// SSLMode → "ssl_mode"); a credo:"..." tag is only needed when the desired
// key differs from the field's snake_case name.
type DatabaseConfig struct {
	Driver      string
	Host        string
	Port        int
	Name        string
	User        string
	Password    string
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
	SSLMode     string
}

func (c DatabaseConfig) DSN() string {
	return fmt.Sprintf("%s://%s:%s@%s:%d/%s?sslmode=%s",
		c.Driver, c.User, c.Password, c.Host, c.Port, c.Name, c.SSLMode)
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// User represents an authenticated user (stored in context via auth.SetUser).
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// CreateTenantRequest is the request body for creating a tenant.
type CreateTenantRequest struct {
	Name    string `json:"name"`
	Domain  string `json:"domain"`
	PlanID  string `json:"plan_id"`
	OwnerID string `json:"owner_id"`
}

// Validate implements validation.Validatable for auto-validation on BindBody.
func (r *CreateTenantRequest) Validate() error {
	return validation.ValidateStruct(r,
		validation.Field(&r.Name, validation.Required[string](), validation.Length(2, 100)),
		validation.Field(&r.Domain, validation.Required[string](), validation.Length(3, 253)),
		validation.Field(&r.PlanID, validation.Required[string](), validation.UUID()),
		validation.Field(&r.OwnerID, validation.Required[string](), validation.UUID()),
	)
}

// Tenant is the response type for tenant operations.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Domain    string    `json:"domain"`
	PlanID    string    `json:"plan_id"`
	OwnerID   string    `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Service layer (DI-managed)
// ---------------------------------------------------------------------------

// TenantService handles tenant business logic.
type TenantService struct {
	infra credo.Infra
	cfg   *DatabaseConfig
}

// NewTenantService is a DI constructor (Model 1: Infra as first parameter).
func NewTenantService(infra credo.Infra, cfg *DatabaseConfig) *TenantService {
	infra.Logger.Info("TenantService initialized", "db_host", cfg.Host)
	return &TenantService{infra: infra, cfg: cfg}
}

// Create creates a new tenant (stub — returns mock data).
func (s *TenantService) Create(ctx context.Context, req *CreateTenantRequest) (*Tenant, error) {
	s.infra.Logger.Info("creating tenant", "name", req.Name, "domain", req.Domain)
	return &Tenant{
		ID:        "tnnt_" + req.Domain,
		Name:      req.Name,
		Domain:    req.Domain,
		PlanID:    req.PlanID,
		OwnerID:   req.OwnerID,
		CreatedAt: time.Now(),
	}, nil
}

// List returns all tenants (stub — returns mock data).
func (s *TenantService) List(ctx context.Context) ([]Tenant, error) {
	s.infra.Logger.Info("listing tenants")
	return []Tenant{
		{ID: "tnnt_acme", Name: "Acme Corp", Domain: "acme.example.com", CreatedAt: time.Now()},
		{ID: "tnnt_globex", Name: "Globex Inc", Domain: "globex.example.com", CreatedAt: time.Now()},
	}, nil
}

// Shutdown implements credo.Shutdowner for graceful cleanup.
func (s *TenantService) Shutdown(ctx context.Context) error {
	s.infra.Logger.Info("TenantService shutting down")
	return nil
}

// ---------------------------------------------------------------------------
// JWT helpers
// ---------------------------------------------------------------------------

// jwtSigningKey is the HMAC key for this example (use RSA/ECDSA in production).
var jwtSigningKey = []byte("super-secret-key-change-in-production")

func newJWTAuthenticator() *auth.JWTAuthenticator[User] {
	a, err := auth.NewJWTAuthenticator(auth.JWTConfig[User]{
		SigningMethod: "HS256",
		SigningKey:    jwtSigningKey,
		ParseClaims: func(claims auth.JWTClaims) (User, error) {
			return User{
				ID:    claims.Subject(),
				Email: claims.GetString("email"),
				Role:  claims.GetString("role"),
			}, nil
		},
	})
	if err != nil {
		log.Fatalf("jwt authenticator: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func loginHandler(ctx *credo.Context) error {
	// In production, validate credentials against a database.
	// This stub issues a JWT for demonstration purposes.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   "usr_1234",
		"email": "admin@example.com",
		"role":  "admin",
		"exp":   time.Now().Add(24 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	})
	signed, err := token.SignedString(jwtSigningKey)
	if err != nil {
		return credo.NewHTTPError(http.StatusInternalServerError, "failed to sign token").WithInternal(err)
	}
	return ctx.Response().JSON(http.StatusOK, map[string]string{
		"token": signed,
		"type":  "Bearer",
	})
}

func meHandler(ctx *credo.Context) error {
	user, ok := auth.GetUser[User](ctx.Context())
	if !ok {
		return credo.ErrUnauthorized
	}
	return ctx.Response().JSON(http.StatusOK, user)
}

func createTenantHandler(svc *TenantService) credo.Handler {
	return func(ctx *credo.Context) error {
		var req CreateTenantRequest
		if err := ctx.Request().BindBody(&req); err != nil {
			return err // validation errors auto-converted to RFC 7807
		}

		tenant, err := svc.Create(ctx.Context(), &req)
		if err != nil {
			return credo.NewHTTPError(http.StatusInternalServerError, "failed to create tenant").WithInternal(err)
		}

		return ctx.Response().JSON(http.StatusCreated, tenant)
	}
}

func listTenantsHandler(svc *TenantService) credo.Handler {
	return func(ctx *credo.Context) error {
		tenants, err := svc.List(ctx.Context())
		if err != nil {
			return credo.NewHTTPError(http.StatusInternalServerError, "failed to list tenants").WithInternal(err)
		}
		return ctx.Response().JSON(http.StatusOK, tenants)
	}
}

func adminDashboardHandler(ctx *credo.Context) error {
	user, _ := auth.GetUser[User](ctx.Context())
	return ctx.Response().JSON(http.StatusOK, map[string]any{
		"message": "Welcome to the admin dashboard",
		"user":    user,
		"stats": map[string]int{
			"total_tenants":  42,
			"active_tenants": 38,
			"total_users":    1250,
		},
	})
}

// requireRole creates middleware that checks the user's role via route meta.
func requireRole(role string) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			user, ok := auth.GetUser[User](ctx.Context())
			if !ok {
				return credo.ErrUnauthorized
			}
			if user.Role != role {
				return credo.NewHTTPError(http.StatusForbidden,
					fmt.Sprintf("role %q required, got %q", role, user.Role))
			}
			return next(ctx)
		}
	}
}

// ---------------------------------------------------------------------------
// Application setup
// ---------------------------------------------------------------------------

func run() error {
	// 1. Load configuration (YAML/JSON file + .env + env vars)
	rawCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}

	// 2. Extract typed configs at module boundary
	var appCfg AppConfig
	if err := rawCfg.Unmarshal("app", &appCfg); err != nil {
		return fmt.Errorf("unmarshal app config: %w", err)
	}

	var dbCfg DatabaseConfig
	if err := rawCfg.Unmarshal("database", &dbCfg); err != nil {
		return fmt.Errorf("unmarshal database config: %w", err)
	}

	// 3. Configure logger
	logLevel := slog.LevelInfo
	if appCfg.Debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	// 4. Create app with config and logger
	app, err := credo.New(
		credo.WithRawConfig(rawCfg),
		credo.WithLogger(logger),
		credo.WithShutdownTimeout(15*time.Second),
	)
	if err != nil {
		return fmt.Errorf("credo.New: %w", err)
	}

	// 5. Register typed configs in DI container
	credo.MustProvideValue(app, &appCfg)
	credo.MustProvideValue(app, &dbCfg)

	// 6. Register services via DI
	credo.MustProvide[*TenantService](app, NewTenantService)

	// 7. Resolve services for handler wiring
	tenantSvc := credo.MustResolve[*TenantService](app)

	// 8. Finalize DI container (freeze + validate: catches missing deps, cycles)
	if err := credo.Finalize(app); err != nil {
		return fmt.Errorf("DI finalize: %w", err)
	}

	// 9. Global middleware you add yourself (applied to all requests,
	// including 404/405). Recover, request ID, and access log are built in.
	app.GlobalMiddleware(
		middleware.Secure(),
		middleware.CORS(middleware.CORSConfig{
			AllowOrigins:     []string{"https://app.example.com", "https://*.example.com"},
			AllowCredentials: true,
		}),
		middleware.Compress(),
	)

	// 10. Health endpoints (K8s liveness + readiness probes)
	app.UseHealth()

	// 11. Public routes (no auth required)
	app.POST("/auth/login", loginHandler).Name("auth.login")

	// 12. Authenticated routes (JWT required)
	jwtAuth := newJWTAuthenticator()
	authenticated := app.Group("/api/v1")
	authenticated.Middleware(
		auth.Middleware[User](jwtAuth, nil),
	)

	authenticated.GET("/me", meHandler).Name("user.me")
	authenticated.GET("/tenants", listTenantsHandler(tenantSvc)).Name("tenants.list")
	authenticated.POST("/tenants", createTenantHandler(tenantSvc)).Name("tenants.create")

	// 13. Admin routes (JWT + admin role required)
	admin := authenticated.Group("/admin")
	admin.Middleware(requireRole("admin"))

	admin.GET("/dashboard", adminDashboardHandler).Name("admin.dashboard")

	// 14. Custom 404 handler
	app.StatusHandler(http.StatusNotFound, func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusNotFound, map[string]string{
			"error":   "not_found",
			"message": fmt.Sprintf("No route matches %s %s", ctx.Request().Method, ctx.Request().URL.Path),
		})
	})

	// 15. Lifecycle hooks
	app.OnStart(func(ctx context.Context) error {
		logger.Info("application started", "app", appCfg.Name, "addr", app.Addr())
		return nil
	})
	app.OnShutdown(func(ctx context.Context) error {
		logger.Info("application shutting down", "app", appCfg.Name)
		return nil
	})

	// 16. Start the server. Run blocks until SIGINT/SIGTERM, then drains
	// gracefully within the configured 15s shutdown timeout. A second signal
	// during shutdown force-kills the process.
	logger.Info("starting application",
		"app", appCfg.Name,
		"env", appCfg.Environment,
	)

	return app.Run()
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("application error: %v", err)
	}
}

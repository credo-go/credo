// Package main demonstrates a full SaaS application built with the Credo framework.
//
// run() follows the documented bootstrap order, every satellite included:
//
//  1. Configuration: config.Load, typed sections, the logger and credo.New
//  2. Provide: typed config values and constructors, Infra first
//  3. Feature mounts and satellite registrations: request features, i18n,
//     health, the store, a scheduled worker, the WebSocket server and hooks
//  4. Finalize: the whole graph is validated before anything is built
//  5. Resolve, middleware and routes, built from resolved values
//  6. Run: signal-aware serving with a graceful drain
//
// Features shown:
//   - Configuration loading (config.Load with YAML + .env + env overrides)
//   - Typed config at the module boundary, injected via DI
//   - Configuration reload: OnConfigChange re-reads one section on SIGHUP
//     (systemctl reload) or app.Reload, here swapping the log level in place
//   - Framework HTTP features (recovery on by default; request ID, access
//     log and compression enabled explicitly) plus global CORS and secure
//     headers middleware
//   - Localization from a programmatic catalog (error, validation and
//     handler messages, field display names)
//   - Authentication (JWT bearer tokens)
//   - Route groups (public, authenticated, admin)
//   - Dependency injection (Provide/Resolve with typed constructors)
//   - Validation (programmatic rules and one custom rule, no struct tags)
//   - Centralized error handling (Credo JSON envelope)
//   - A data store registered with store.Register: pinged at start,
//     reported by /ready, shut down after its consumers
//   - A scheduled worker built from the container
//   - A WebSocket echo endpoint served by an ingress component
//   - Health probes (/health, /ready) for container orchestration
//   - Lifecycle components and graceful shutdown with OnStop hooks
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/auth"
	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/middleware"
	"github.com/credo-go/credo/store"
	"github.com/credo-go/credo/validation"
	"github.com/credo-go/credo/websocket"
	"github.com/credo-go/credo/worker"
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

// User represents an authenticated user (attached to the request via ctx.SetUser).
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
		validation.Field(&r.Domain,
			validation.Required[string](),
			validation.Length(3, 253),
			validation.By(notReservedDomain),
		),
		validation.Field(&r.PlanID, validation.Required[string](), validation.UUID()),
		validation.Field(&r.OwnerID, validation.Required[string](), validation.UUID()),
	)
}

// notReservedDomain is a custom rule. A client-visible failure is a
// validation.NewError; any other error a rule returns is an internal (500)
// failure whose text is only logged.
func notReservedDomain(domain string) error {
	for _, suffix := range []string{".invalid", ".localhost", ".test"} {
		if strings.HasSuffix(domain, suffix) {
			return validation.NewError("domain_reserved", "must not use a reserved domain")
		}
	}
	return nil
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
// Data store (stand-in for a *sqldb.DB)
// ---------------------------------------------------------------------------

// TenantStore stands in for a database handle such as *sqldb.DB, which an
// application builds from DatabaseConfig with sqldb.Open. It implements
// store.Lifecycle, so store.Register can name its binding: the start phase
// pings it, /ready reports its health, and the App shuts it down after the
// services that use it.
type TenantStore struct {
	infra   credo.Infra
	mu      sync.Mutex
	tenants map[string]Tenant
}

// NewTenantStore is a DI constructor. A real constructor opens the
// connection pool from cfg; it does no I/O beyond that, since the start
// phase pings the store.
func NewTenantStore(infra credo.Infra, cfg *DatabaseConfig) *TenantStore {
	infra.Logger.Debug("tenant store configured", "db_host", cfg.Host, "db_name", cfg.Name)
	return &TenantStore{infra: infra, tenants: make(map[string]Tenant)}
}

// Ping implements store.Lifecycle; the start phase calls it once.
func (s *TenantStore) Ping(context.Context) error { return nil }

// Health implements store.Lifecycle; /ready reports it under the store's name.
func (s *TenantStore) Health(context.Context) store.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.Health{Status: store.StatusUp, Details: map[string]any{"tenants": len(s.tenants)}}
}

// Shutdown implements store.Lifecycle and makes the store a component.
func (s *TenantStore) Shutdown(context.Context) error {
	s.infra.Logger.Info("tenant store closed")
	return nil
}

// Save stores a tenant.
func (s *TenantStore) Save(t Tenant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants[t.ID] = t
}

// All returns every stored tenant.
func (s *TenantStore) All() []Tenant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		out = append(out, t)
	}
	return out
}

// ---------------------------------------------------------------------------
// Service layer (DI-managed)
// ---------------------------------------------------------------------------

// TenantService handles tenant business logic.
type TenantService struct {
	infra credo.Infra
	store *TenantStore
}

// NewTenantService is a DI constructor (Infra as first parameter).
func NewTenantService(infra credo.Infra, store *TenantStore) *TenantService {
	return &TenantService{infra: infra, store: store}
}

// Create creates a new tenant.
func (s *TenantService) Create(ctx context.Context, req *CreateTenantRequest) (*Tenant, error) {
	tenant := Tenant{
		ID:        "tnnt_" + req.Domain,
		Name:      req.Name,
		Domain:    req.Domain,
		PlanID:    req.PlanID,
		OwnerID:   req.OwnerID,
		CreatedAt: time.Now(),
	}
	s.store.Save(tenant)
	s.infra.Logger.InfoContext(ctx, "tenant created", "tenant_id", tenant.ID)
	return &tenant, nil
}

// List returns all tenants.
func (s *TenantService) List(context.Context) ([]Tenant, error) {
	return s.store.All(), nil
}

// Shutdown makes TenantService a credo.Component: the App shuts it down
// after its consumers (the HTTP drain) and before the store it uses.
func (s *TenantService) Shutdown(context.Context) error {
	s.infra.Logger.Info("TenantService shutting down")
	return nil
}

// ---------------------------------------------------------------------------
// Background work (DI-managed scheduled worker)
// ---------------------------------------------------------------------------

// UsageReporter is a scheduled worker: each activation performs one run and
// returns. The container builds it in the start walk, after the store it
// depends on, and the App stops it with the HTTP drain.
type UsageReporter struct {
	infra credo.Infra
	store *TenantStore
}

// NewUsageReporter is a DI constructor.
func NewUsageReporter(infra credo.Infra, store *TenantStore) *UsageReporter {
	return &UsageReporter{infra: infra, store: store}
}

// Run implements worker.Worker.
func (r *UsageReporter) Run(ctx context.Context) error {
	r.infra.Logger.InfoContext(ctx, "usage report", "tenants", len(r.store.All()))
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
// Localization
// ---------------------------------------------------------------------------

// i18nConfig is a programmatic English catalog. Error and validation codes
// are message keys as they are, with no prefix; a code with no entry falls
// back to Credo's built-in text. Bigger applications keep the catalogs in
// locales/<lang>/messages.json and fields.json (see ../references/locales).
func i18nConfig() credo.I18nConfig {
	return credo.I18nConfig{
		Default: "en",
		Messages: credo.I18nMessages{
			"admin.welcome":        "Welcome to the admin dashboard, {{.email}}",
			"required":             "{{.field}} is required",
			"length":               "{{.field}} must be between {{.min}} and {{.max}} characters",
			"uuid":                 "{{.field}} must be a valid UUID",
			"domain_reserved":      "{{.field}} must not use a reserved domain",
			"role_required":        "Your role does not allow this action",
			"token_signing_failed": "The token could not be issued",
			"tenant_create_failed": "The tenant could not be created",
			"tenant_list_failed":   "The tenants could not be listed",
		},
		Fields: credo.I18nFields{
			"name":     "Name",
			"domain":   "Domain",
			"plan_id":  "Plan",
			"owner_id": "Owner",
		},
	}
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
		return credo.NewHTTPError(http.StatusInternalServerError, "token_signing_failed").
			WithInternal(err)
	}
	return ctx.Response().JSON(http.StatusOK, map[string]string{
		"token": signed,
		"type":  "Bearer",
	})
}

func meHandler(ctx *credo.Context) error {
	user, ok := ctx.GetUser[User]()
	if !ok {
		return credo.ErrUnauthorized
	}
	return ctx.Response().JSON(http.StatusOK, user)
}

func createTenantHandler(svc *TenantService) credo.Handler {
	return func(ctx *credo.Context) error {
		var req CreateTenantRequest
		if err := ctx.Request().BindBody(&req); err != nil {
			return err // validation errors enter the centralized error envelope
		}

		tenant, err := svc.Create(ctx.Context(), &req)
		if err != nil {
			return credo.NewHTTPError(http.StatusInternalServerError, "tenant_create_failed").
				WithInternal(err)
		}

		return ctx.Response().JSON(http.StatusCreated, tenant)
	}
}

func listTenantsHandler(svc *TenantService) credo.Handler {
	return func(ctx *credo.Context) error {
		tenants, err := svc.List(ctx.Context())
		if err != nil {
			return credo.NewHTTPError(http.StatusInternalServerError, "tenant_list_failed").
				WithInternal(err)
		}
		return ctx.Response().JSON(http.StatusOK, tenants)
	}
}

func adminDashboardHandler(ctx *credo.Context) error {
	user, _ := ctx.GetUser[User]()
	return ctx.Response().JSON(http.StatusOK, map[string]any{
		"message": ctx.T("admin.welcome", map[string]any{"email": user.Email}),
		"user":    user,
		"stats": map[string]int{
			"total_tenants":  42,
			"active_tenants": 38,
			"total_users":    1250,
		},
	})
}

// echoHandler echoes every WebSocket message back until the client closes.
func echoHandler(_ *credo.Context, conn *websocket.Conn) error {
	for {
		typ, payload, err := conn.Read(conn.Context())
		if err != nil {
			return err
		}
		if err := conn.Write(conn.Context(), typ, payload); err != nil {
			return err
		}
	}
}

// requireRole creates middleware that checks the user's role.
func requireRole(role string) credo.Middleware {
	return func(next credo.Handler) credo.Handler {
		return func(ctx *credo.Context) error {
			user, ok := ctx.GetUser[User]()
			if !ok {
				return credo.ErrUnauthorized
			}
			if user.Role != role {
				return credo.NewHTTPError(http.StatusForbidden, "role_required").
					WithDetails(map[string]string{"required_role": role})
			}
			return next(ctx)
		}
	}
}

// ---------------------------------------------------------------------------
// Application setup
// ---------------------------------------------------------------------------

func run() error {
	// 1. Configuration. Load the YAML/JSON file + .env + env vars, read the
	// application's typed sections at the module boundary, build the logger
	// and create the App. The log level lives in a slog.LevelVar so a config
	// reload (SIGHUP / app.Reload) can change it without a restart.
	rawCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}

	var appCfg AppConfig
	if err = rawCfg.Unmarshal("app", &appCfg); err != nil {
		return fmt.Errorf("unmarshal app config: %w", err)
	}
	var dbCfg DatabaseConfig
	if err = rawCfg.Unmarshal("databases.default", &dbCfg); err != nil {
		return fmt.Errorf("unmarshal database config: %w", err)
	}

	var logLevel slog.LevelVar
	if appCfg.Debug {
		logLevel.Set(slog.LevelDebug)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel}))

	app, err := credo.New(
		credo.WithRawConfig(rawCfg),
		credo.WithLogger(logger),
		credo.WithShutdownTimeout(15*time.Second),
	)
	if err != nil {
		return fmt.Errorf("credo.New: %w", err)
	}

	// 2. Provide. Typed config values and the constructors that use them.
	// Nothing is built yet; a misused registration panics at its line.
	app.ProvideValue(&appCfg)
	app.ProvideValue(&dbCfg)
	app.Provide[*TenantStore](NewTenantStore)
	app.Provide[*TenantService](NewTenantService)
	app.Provide[*UsageReporter](NewUsageReporter)

	// 3. Feature mounts and satellite registrations, in any order.
	//
	// Framework HTTP features: panic recovery is on by default; request
	// correlation, access logging and response compression are installed once
	// and run around every request, including 404/405.
	app.UseRequestID()
	app.UseAccessLog()
	app.UseCompress()
	app.UseI18n(i18nConfig())
	app.UseHealth() // /health (liveness) and /ready (readiness)

	// The store binding is named for the start-phase ping and /ready.
	store.Register[*TenantStore](app, store.WithName("tenants-db"))

	// A scheduled worker built from the container: one run per hour.
	workers := worker.Use(app)
	workers.ScheduledProvided[*UsageReporter]("usage-report", "@every 1h")

	// The WebSocket server is an ingress component: it starts with the App
	// and drains beside the HTTP drain, before the services its handlers use.
	ws := websocket.New(app.NewInfra("websocket"))
	app.Manage(ws, credo.Ingress())

	// Hooks. OnConfigChange makes the "app" section reloadable: flip
	// app.debug in the config file and `systemctl reload` (SIGHUP) or
	// app.Reload switches the log level in place. The new value is decoded
	// and published atomically before this runs; nothing else in "app" is
	// live.
	app.OnConfigChange("app", func(ctx context.Context, next AppConfig) error {
		if next.Debug {
			logLevel.Set(slog.LevelDebug)
		} else {
			logLevel.Set(slog.LevelInfo)
		}
		logger.Info("log level reloaded", "debug", next.Debug)
		return nil
	})
	app.OnStart(func(context.Context) error {
		logger.Info("application started", "app", appCfg.Name, "addr", app.Addr().String())
		return nil
	})
	app.OnStop(func(context.Context) error {
		logger.Info("application shutting down", "app", appCfg.Name)
		return nil
	})

	// 4. Finalize validates the whole graph — missing dependencies, cycles —
	// and reports every problem at once. Constructors run only after this.
	if err := app.Finalize(); err != nil {
		return fmt.Errorf("DI finalize: %w", err)
	}

	// 5. Resolve, middleware and routes, built from resolved values.
	tenantSvc := app.MustResolve[*TenantService]()

	// Global middleware you add yourself (applied to all requests,
	// including 404/405).
	app.GlobalMiddleware(
		middleware.Secure(),
		middleware.CORS(middleware.CORSConfig{
			AllowOrigins:     []string{"https://app.example.com", "https://*.example.com"},
			AllowCredentials: true,
		}),
	)

	// Public routes (no auth required).
	app.POST("/auth/login", loginHandler).Name("auth.login")
	app.GET("/ws/echo", ws.Handler(echoHandler)).Name("ws.echo")

	// Authenticated routes (JWT required).
	jwtAuth := newJWTAuthenticator()
	authenticated := app.Group("/api/v1")
	authenticated.Middleware(
		auth.Middleware[User](jwtAuth, nil),
	)

	authenticated.GET("/me", meHandler).Name("user.me")
	authenticated.GET("/tenants", listTenantsHandler(tenantSvc)).Name("tenants.list")
	authenticated.POST("/tenants", createTenantHandler(tenantSvc)).Name("tenants.create")

	// Admin routes (JWT + admin role required).
	admin := authenticated.Group("/admin")
	admin.Middleware(requireRole("admin"))

	admin.GET("/dashboard", adminDashboardHandler).Name("admin.dashboard")

	// Custom answer for the router's own 404 (StatusHandler accepts 404 and
	// 405 only).
	app.StatusHandler(http.StatusNotFound, func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusNotFound, map[string]string{
			"error":   "not_found",
			"message": fmt.Sprintf("No route matches %s %s", ctx.Request().Method, ctx.Request().URL.Path),
		})
	})

	// 6. Run. The start phase pings the store, builds and starts the
	// components — the worker and the WebSocket server among them — and runs
	// the start hooks before the App accepts requests. Run blocks until
	// SIGINT/SIGTERM, then drains gracefully within the configured 15s
	// shutdown timeout: the listener, the WebSocket server and the scheduled
	// worker first, then the services, then the store. A second signal during
	// shutdown force-kills the process.
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

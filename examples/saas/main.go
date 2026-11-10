// Package main demonstrates a full SaaS application built with the Credo framework.
//
// run() follows the documented bootstrap order, every satellite included:
//
//  1. Configuration: config.Load, typed sections and credo.New, which
//     builds the logger from the "log" section
//  2. Bindings: the opened store as a value, typed config, and the
//     constructors, Infra first
//  3. Feature mounts and satellite registrations: request features, i18n,
//     health, the store, a scheduled worker, the WebSocket server and hooks
//  4. Finalize: an optional early check of the whole graph
//  5. Resolve, middleware and routes, built from resolved values
//  6. Run: signal-aware serving with a graceful drain
//
// Features shown:
//   - Configuration loading (config.Load with YAML + .env + env overrides)
//   - Typed config at the module boundary, injected via DI
//   - The framework's "log" section: log.level and log.format select the
//     App's logger, with no logger built here
//   - Configuration reload: a SIGHUP (systemctl reload) or app.Reload applies
//     a new log.level and log.format in place, while a change to the static
//     "app" section is reported as needing a restart
//   - Framework HTTP features (recovery on by default; request ID, access
//     log and compression enabled explicitly) plus pre-routing CORS and secure
//     headers middleware
//   - Localization from a programmatic catalog (error, validation and
//     handler messages, field display names)
//   - Authentication (JWT bearer tokens)
//   - Route groups (public, authenticated, admin)
//   - Dependency injection (Provide/Resolve with typed constructors)
//   - Validation (programmatic rules and one custom rule, no struct tags)
//   - Centralized error handling (Credo JSON envelope)
//   - A data store bound as a value: one ProvideValue call pings it at start,
//     reports it in /ready and shuts it down after its consumers
//   - A scheduled worker built from the container
//   - A WebSocket echo endpoint served by a component whose type declares
//     the ingress tier
//   - Health probes (/health, /ready) for container orchestration
//   - Lifecycle components and graceful shutdown with OnStop hooks
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/credo-go/credo"
	"github.com/credo-go/credo/auth"
	"github.com/credo-go/credo/auth/jwtauth"
	"github.com/credo-go/credo/config"
	"github.com/credo-go/credo/middleware"
	"github.com/credo-go/credo/validation"
	"github.com/credo-go/credo/websocket"
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

// User represents an authenticated user (attached to the request via c.SetUser).
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
// validation.NewViolation; any other error a rule returns is an internal (500)
// failure whose text is only logged.
func notReservedDomain(domain string) error {
	for _, suffix := range []string{".invalid", ".localhost", ".test"} {
		if strings.HasSuffix(domain, suffix) {
			return validation.NewViolation("domain_reserved", "must not use a reserved domain")
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
// application opens from its configuration with sqldb.Open and binds with
// app.ProvideValue. Like *sqldb.DB it is a component: its type shows Start,
// Ready and Shutdown, so the start phase pings it before the services that
// use it start, /ready reports it, and the App shuts it down after those
// services.
type TenantStore struct {
	mu      sync.Mutex
	tenants map[string]Tenant
}

// OpenTenantStore stands in for sqldb.Open: it opens the connection pool
// from cfg and does no I/O beyond that, since the start phase pings the
// store.
func OpenTenantStore(cfg DatabaseConfig) (*TenantStore, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("tenant store: databases.default.host is empty")
	}
	return &TenantStore{tenants: make(map[string]Tenant)}, nil
}

// Start pings the store; the start phase calls it once, before the services
// that depend on it start.
func (s *TenantStore) Start(context.Context) error { return nil }

// Ready reports the store in /ready, under its component name.
func (s *TenantStore) Ready(context.Context) error { return nil }

// Shutdown closes the store and makes it a component. The App's call
// carries the component's scoped Infra, so the record names the component
// (component=tenants-db), as a framework module such as sqldb logs.
func (s *TenantStore) Shutdown(ctx context.Context) error {
	credo.InfraFrom(ctx).Logger.Info("tenant store closed")
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

// NewUsageReporter is the worker's constructor, whose parameters the
// container supplies.
func NewUsageReporter(infra credo.Infra, store *TenantStore) *UsageReporter {
	return &UsageReporter{infra: infra, store: store}
}

// Run implements credo.Worker.
func (r *UsageReporter) Run(ctx context.Context) error {
	r.infra.Logger.InfoContext(ctx, "usage report", "tenants", len(r.store.All()))
	return nil
}

// ---------------------------------------------------------------------------
// JWT helpers
// ---------------------------------------------------------------------------

// jwtSigningKey is the HMAC key for this example (use RSA/ECDSA in production).
var jwtSigningKey = []byte("super-secret-key-change-in-production")

func newJWTAuthenticator() *jwtauth.Authenticator[User] {
	a, err := jwtauth.NewAuthenticator(jwtauth.Config[User]{
		SigningMethod: "HS256",
		SigningKey:    jwtSigningKey,
		ParseClaims: func(claims jwtauth.Claims) (User, error) {
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

func loginHandler(c *credo.Context) error {
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
	return c.Response().JSON(http.StatusOK, map[string]string{
		"token": signed,
		"type":  "Bearer",
	})
}

func meHandler(c *credo.Context) error {
	user, ok := c.GetUser[User]()
	if !ok {
		return credo.ErrUnauthorized
	}
	return c.Response().JSON(http.StatusOK, user)
}

func createTenantHandler(svc *TenantService) credo.Handler {
	return func(c *credo.Context) error {
		var req CreateTenantRequest
		if err := c.Request().BindBody(&req); err != nil {
			return err // validation errors enter the centralized error envelope
		}

		tenant, err := svc.Create(c.Context(), &req)
		if err != nil {
			return credo.NewHTTPError(http.StatusInternalServerError, "tenant_create_failed").
				WithInternal(err)
		}

		return c.Response().JSON(http.StatusCreated, tenant)
	}
}

func listTenantsHandler(svc *TenantService) credo.Handler {
	return func(c *credo.Context) error {
		tenants, err := svc.List(c.Context())
		if err != nil {
			return credo.NewHTTPError(http.StatusInternalServerError, "tenant_list_failed").
				WithInternal(err)
		}
		return c.Response().JSON(http.StatusOK, tenants)
	}
}

func adminDashboardHandler(c *credo.Context) error {
	user, _ := c.GetUser[User]()
	return c.Response().JSON(http.StatusOK, map[string]any{
		"message": c.T("admin.welcome", map[string]any{"email": user.Email}),
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
		return func(c *credo.Context) error {
			user, ok := c.GetUser[User]()
			if !ok {
				return credo.ErrUnauthorized
			}
			if user.Role != role {
				return credo.NewHTTPError(http.StatusForbidden, "role_required").
					WithDetails(map[string]string{"required_role": role})
			}
			return next(c)
		}
	}
}

// ---------------------------------------------------------------------------
// Application setup
// ---------------------------------------------------------------------------

func run() error {
	// 1. Configuration. Load the YAML/JSON file + .env + env vars, read the
	// application's typed sections at the module boundary and create the
	// App. The logger is the framework's: credo.New builds it from the "log"
	// section (log.level, log.format) and holds the section live, so a
	// config reload (SIGHUP / app.Reload) applies a new level and a new
	// format without a restart. An application that needs another handler
	// passes its own logger with credo.WithLogger, and the section is then
	// not read.
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

	app, err := credo.New(
		credo.WithRawConfig(rawCfg),
		credo.WithShutdownTimeout(15*time.Second),
	)
	if err != nil {
		return fmt.Errorf("credo.New: %w", err)
	}
	logger := app.Logger()

	// 2. Bindings. The store is opened here, as sqldb.Open would open a
	// database, and bound as a value: its type plans the ping at start, the
	// /ready entry and the teardown after its consumers, and ComponentName
	// names it in logs and /ready. Then the typed config and the
	// constructors; nothing is built yet, and a misused registration panics
	// at its line.
	tenantStore, err := OpenTenantStore(dbCfg)
	if err != nil {
		return err
	}
	app.ProvideValue(tenantStore, credo.ComponentName("tenants-db"))
	app.ProvideValue(&appCfg)
	app.Provide(NewTenantService)

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

	// A scheduled worker its constructor builds over the container's
	// values: one run per hour.
	app.Workers().Scheduled("usage-report", "@every 1h", NewUsageReporter)

	// The WebSocket server's type declares the ingress tier, so Manage needs
	// no option: it starts with the App and drains beside the HTTP drain,
	// before the services its handlers use.
	ws := websocket.New()
	app.Manage(ws)

	// Hooks. Nothing here follows the "log" section: change log.level or
	// log.format in the config file and `systemctl reload` (SIGHUP) or
	// app.Reload applies it to this logger and to every logger derived from
	// it. "app" stays static, so a reload that changes app.name reports a
	// restart.
	app.OnStart(func(context.Context) error {
		logger.Info("application started", "app", appCfg.Name, "addr", app.Addr().String())
		return nil
	})
	app.OnStop(func(context.Context) error {
		logger.Info("application shutting down", "app", appCfg.Name)
		return nil
	})

	// 4. Finalize validates the whole graph — missing dependencies, cycles —
	// and reports every problem at once, before anything is built. It is
	// optional: without it, the first Resolve below finalizes the container.
	if err := app.Finalize(); err != nil {
		return fmt.Errorf("DI finalize: %w", err)
	}

	// 5. Resolve, middleware and routes, built from resolved values.
	tenantSvc := app.MustResolve[*TenantService]()

	// Pre-routing middleware you add yourself: it runs on every request,
	// the router's 404 and 405 included.
	app.PreRouting(
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
		auth.Middleware[User](jwtAuth),
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
	app.StatusHandler(http.StatusNotFound, func(c *credo.Context) error {
		return c.Response().JSON(http.StatusNotFound, map[string]string{
			"error":   "not_found",
			"message": fmt.Sprintf("No route matches %s %s", c.Request().Method, c.Request().URL.Path),
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

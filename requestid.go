// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Originally derived from github.com/go-chi/chi/middleware (MIT License).

package credo

import (
	"fmt"
	"strings"

	internalrequestid "github.com/credo-go/credo/internal/requestid"
)

// requestIDKey is the context-store key for the request ID.
const requestIDKey = internalrequestid.Key

// RequestIDConfig configures the request ID feature installed by
// [App.UseRequestID]. The zero value selects every default.
type RequestIDConfig struct {
	// Header is the HTTP header the request ID is read from and written to.
	// Default: "X-Request-Id". Credential headers (Authorization,
	// Proxy-Authorization, Cookie) are rejected at registration because the
	// value is echoed on the response and attached to log records.
	Header string

	// Generator creates a request ID when the client did not send a valid
	// one. Default: crypto/rand.Text (a 128-bit base32 string).
	Generator func() string

	// Limit is the maximum accepted length of an incoming request ID; longer
	// values are discarded and a new ID is generated, so a client cannot
	// inject arbitrarily large values. Default: 64.
	Limit int
}

// requestIDFeature is the normalized request ID configuration.
type requestIDFeature struct {
	header    string
	generator func() string
	limit     int
}

// UseRequestID installs request correlation: every request gets an ID —
// the incoming Header value when it is present, within Limit and made of
// safe header/log characters, otherwise a freshly generated one — published
// on the response header, readable through [Context.RequestID] and attached
// as the request_id attribute of the request-scoped logger the first time
// [Context.Logger] is used.
//
// The feature is off by default; without it no ID is generated or echoed and
// Context.RequestID returns "". It is independent of [App.UseAccessLog]:
// access records carry an empty request_id when only access logging is
// installed. UseRequestID accepts zero configs for the defaults or one config;
// it panics for more than one config, when called twice, or after the App was
// prepared or shut down.
func (app *App) UseRequestID(cfgs ...RequestIDConfig) {
	cfg := oneConfig("App.UseRequestID", cfgs)
	f := &requestIDFeature{
		header:    cfg.Header,
		generator: cfg.Generator,
		limit:     cfg.Limit,
	}
	if f.header == "" {
		f.header = internalrequestid.Header
	}
	switch strings.ToLower(f.header) {
	case "authorization", "proxy-authorization", "cookie":
		panic(fmt.Sprintf("credo: App.UseRequestID: Header %q is a credential header and cannot carry a request ID", f.header))
	}
	if f.generator == nil {
		f.generator = internalrequestid.Generate
	}
	if f.limit <= 0 {
		f.limit = internalrequestid.DefaultLimit
	}
	app.installFeature("App.UseRequestID", func() {
		if app.requestID != nil {
			panic("credo: App.UseRequestID called twice")
		}
		app.requestID = f
	})
}

// apply resolves the request ID once per request, before any consumer needs
// it. Logger enrichment is deferred to the first Logger() call so requests
// whose handlers never log skip the With allocation.
func (f *requestIDFeature) apply(c *Context) {
	id := internalrequestid.Resolve(c.request.Header.Get(f.header), f.limit, f.generator)
	c.Set(requestIDKey, id)
	c.response.Header().Set(f.header, id)
	c.pendingLogAttrID = id
}

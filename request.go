// Originally derived from Echo (https://github.com/labstack/echo),
// Copyright (c) 2024 LabStack, MIT licensed. Substantially modified for Credo;
// see the NOTICES file for full attribution.

package credo

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/netip"
	"net/url"

	internalproxy "github.com/credo-go/credo/internal/proxy"
	"github.com/credo-go/credo/validation"
)

// Request wraps *http.Request and provides request-side helpers:
// route parameters, query parameter shortcuts, and body/query binding.
type Request struct {
	*http.Request

	// app is a back-reference to the application, set by the context pool.
	// Used for debug-mode bind warnings. Nil for externally created Requests.
	app *App

	// paramKeys/paramValues hold URL parameters populated during dispatch
	// as parallel slices in insertion order. Typical routes carry at most a
	// handful of parameters, so a linear scan beats a map and the backing
	// arrays are retained across pool reuse — steady-state dispatch does not
	// allocate for parameters. For host-scoped routes they include both
	// path params and host params (collisions are rejected at registration).
	paramKeys   []string
	paramValues []string

	// paramsMap is the lazily materialized view served by RouteParams.
	// Rebuilt from the slices when paramsMapValid is false; backing storage
	// is retained across pool reuse.
	paramsMap      map[string]string
	paramsMapValid bool

	// cachedQuery is lazy-parsed from URL.Query() on first access.
	// Cleared on reset to avoid leaking across pooled reuse.
	cachedQuery url.Values

	// cachedScheme and cachedRealIP avoid repeated proxy-header parsing within
	// one request. Cleared on reset to avoid leaking across pooled reuse.
	cachedScheme    string
	cachedSchemeSet bool
	cachedRealIP    string
	cachedRealIPSet bool
}

// NewRequest creates a new Request wrapping the given *http.Request.
func NewRequest(r *http.Request) *Request {
	return &Request{Request: r}
}

// RouteParam returns the URL parameter value for name — a path parameter such
// as {id}, or, for host-scoped routes, a host parameter. It returns "" when
// the parameter is not present.
//
// RouteParam is the preferred accessor for single values; it never
// allocates:
//
//	id := ctx.Request().RouteParam("id")
//
// Unlike the map returned by [Request.RouteParams], the returned string is
// safe to retain after the request completes.
func (r *Request) RouteParam(name string) string {
	for i, k := range r.paramKeys {
		if k == name {
			return r.paramValues[i]
		}
	}
	return ""
}

// RouteParams returns all URL parameter key-value pairs.
// For host-scoped routes, host params and path params share the same namespace.
//
// The map is a read-only view, materialized lazily on first call: writes
// to it are not seen by [Request.RouteParam]. It is owned by the framework
// and recycled after the request completes — do not retain it or read it
// from another goroutine after the handler returns. For single values,
// prefer [Request.RouteParam].
func (r *Request) RouteParams() map[string]string {
	if !r.paramsMapValid {
		if r.paramsMap == nil {
			r.paramsMap = make(map[string]string, len(r.paramKeys))
		} else {
			clear(r.paramsMap)
		}
		for i, k := range r.paramKeys {
			r.paramsMap[k] = r.paramValues[i]
		}
		r.paramsMapValid = true
	}
	return r.paramsMap
}

// resetRouteParams clears the parameter set before a (re-)dispatch.
// Backing storage is retained.
func (r *Request) resetRouteParams() {
	r.paramKeys = r.paramKeys[:0]
	r.paramValues = r.paramValues[:0]
	r.paramsMapValid = false
}

// addRouteParam appends one parameter. Dispatch-internal; uniqueness is
// guaranteed by registration-time validation (path/host collisions panic).
func (r *Request) addRouteParam(key, value string) {
	r.paramKeys = append(r.paramKeys, key)
	r.paramValues = append(r.paramValues, value)
}

// PathValue returns the route parameter for name, falling back to the
// embedded request's [http.Request.PathValue]. Prefer [Request.RouteParam]
// in new code.
//
// This shadow exists for stdlib muscle memory: Credo's dispatcher does not
// populate the embedded *http.Request's path values (doing so would cost an
// allocation per request for data [Request.RouteParam] already serves), so
// without it ctx.Request().PathValue("id") would silently return "". The
// raw embedded request — as seen by stdlib handlers via [App.Mount] or
// middleware via [WrapStdMiddleware] — still carries no path values.
func (r *Request) PathValue(name string) string {
	for i, k := range r.paramKeys {
		if k == name {
			return r.paramValues[i]
		}
	}
	return r.Request.PathValue(name)
}

// QueryParam returns a query string parameter value by name.
// Returns "" if not present.
func (r *Request) QueryParam(name string) string {
	return r.query().Get(name)
}

// Scheme reports the scheme the original client used: "http" or "https".
//
// If the request arrived over TLS directly, Scheme returns "https". Otherwise,
// forwarded scheme headers are considered only when the immediate peer
// RemoteAddr is configured as a trusted proxy on the App. Untrusted peers cannot
// influence the result.
//
// Only "http" and "https" are returned. Invalid forwarded header values fall
// back to the underlying transport.
func (r *Request) Scheme() string {
	if r == nil {
		return "http"
	}
	if r.cachedSchemeSet {
		return r.cachedScheme
	}

	var trustedProxies []netip.Prefix
	if r.app != nil {
		trustedProxies = r.app.trustedProxies
	}
	r.cachedScheme = internalproxy.Scheme(r.Request, trustedProxies)
	r.cachedSchemeSet = true
	return r.cachedScheme
}

// RealIP returns the address of the original client.
//
// When the immediate peer RemoteAddr is trusted, RealIP walks the proxy
// chain — the RFC 7239 Forwarded header's for= parameters first, then
// X-Forwarded-For — from right to left, skipping trusted proxy hops and
// returning the first untrusted address. If neither yields a usable value,
// X-Real-IP is used. If the peer is untrusted, all forwarded headers are
// ignored.
//
// The returned value is an IP address only for parseable addresses. If
// RemoteAddr itself is unparseable, RealIP falls back to RemoteAddr verbatim.
func (r *Request) RealIP() string {
	if r == nil {
		return ""
	}
	if r.cachedRealIPSet {
		return r.cachedRealIP
	}

	var trustedProxies []netip.Prefix
	if r.app != nil {
		trustedProxies = r.app.trustedProxies
	}
	r.cachedRealIP = internalproxy.RealIP(r.Request, trustedProxies)
	r.cachedRealIPSet = true
	return r.cachedRealIP
}

// query returns the parsed query values, caching the result for reuse
// within the same request.
func (r *Request) query() url.Values {
	if r.cachedQuery == nil {
		r.cachedQuery = r.URL.Query()
	}
	return r.cachedQuery
}

// BindBody decodes the request body into target based on the Content-Type header.
// Supported content types:
//   - application/json (default when Content-Type is absent)
//   - application/xml, text/xml
//   - application/x-www-form-urlencoded (uses "form" struct tags)
//   - multipart/form-data (uses "form" struct tags, including file fields)
//
// If target implements [validation.Validatable], Validate() is called
// automatically after successful decoding ("parse, don't validate").
//
// Returns 400 Bad Request for empty body or decode errors,
// 415 Unsupported Media Type for unrecognized content types.
func (r *Request) BindBody(target any) error {
	if r.Body == nil {
		return NewHTTPError(http.StatusBadRequest, "request body is empty")
	}

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}

	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		// If parsing fails, use the raw Content-Type as-is
		mediaType = ct
	}

	switch mediaType {
	case "application/json":
		if err := json.NewDecoder(r.Body).Decode(target); err != nil {
			return bodyDecodeError("invalid JSON body", err)
		}

	case "application/xml", "text/xml":
		if err := xml.NewDecoder(r.Body).Decode(target); err != nil {
			return bodyDecodeError("invalid XML body", err)
		}

	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return bodyDecodeError("invalid form body", err)
		}
		if err := decodeValues(target, r.PostForm, "form"); err != nil {
			return err
		}

	case "multipart/form-data":
		if err := r.ParseMultipartForm(defaultMultipartMaxMemory); err != nil {
			return bodyDecodeError("invalid multipart body", err)
		}
		if err := decodeValues(target, url.Values(r.MultipartForm.Value), "form"); err != nil {
			return err
		}
		if err := bindMultipartFiles(target, r.MultipartForm.File); err != nil {
			return err
		}

	default:
		return NewHTTPError(http.StatusUnsupportedMediaType,
			"unsupported content type: "+mediaType)
	}

	return r.validateBoundTarget("BindBody", target)
}

// bodyDecodeError maps a body read/decode failure to the appropriate HTTP error:
// 413 Request Entity Too Large when the body-size limit (http.MaxBytesReader)
// was exceeded, otherwise 400 Bad Request with msg as the message key.
func bodyDecodeError(msg string, err error) *HTTPError {
	if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large").
			WithInternal(mbe)
	}
	return NewHTTPError(http.StatusBadRequest, msg).WithInternal(err)
}

// BindQuery decodes URL query parameters into target using `query:"name"` struct tags.
// If target implements [validation.Validatable], Validate() is called automatically
// after successful decoding ("parse, don't validate").
//
// In debug mode, a warning is logged when the target does not implement Validatable.
func (r *Request) BindQuery(target any) error {
	if err := decodeValues(target, r.query(), "query"); err != nil {
		return err
	}
	return r.validateBoundTarget("BindQuery", target)
}

func (r *Request) validateBoundTarget(op string, target any) error {
	if v, ok := target.(validation.Validatable); ok {
		return v.Validate()
	}
	if r.app.IsDebug() {
		r.app.logger.Warn(op+": target does not implement Validatable, skipping validation",
			"type", fmt.Sprintf("%T", target))
	}
	return nil
}

// reset prepares the Request for pool reuse.
func (r *Request) reset(hr *http.Request) {
	r.Request = hr
	r.resetRouteParams() // retains backing storage for reuse
	r.cachedQuery = nil  // drop reference so next request parses its own URL
	r.cachedScheme = ""
	r.cachedSchemeSet = false
	r.cachedRealIP = ""
	r.cachedRealIPSet = false
}

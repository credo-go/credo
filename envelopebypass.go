package credo

import "log/slog"

// MetaRawResponse is the route-meta key that silences the debug-mode
// envelope-bypass diagnostic for routes that intentionally write raw JSON
// outside [Context.Render] — webhooks, third-party-dictated shapes, and
// other deliberate raw endpoints:
//
//	app.POST("/webhooks/stripe", h).SetMeta(credo.MetaRawResponse, true)
//	app.Group("/callbacks").SetMeta(credo.MetaRawResponse, true)
//
// Groups pass the value to their routes through the usual meta parent chain;
// a route-level value overrides its group. Only the bool true silences.
//
// The diagnostic itself is opt-in twice over: it fires only when a
// [SuccessRenderer] is installed AND the application runs in debug mode
// ([WithDebug] / server.debug). It flags handlers that wrote a body-carrying
// JSON response through the raw [Response.JSON] helper, silently skipping the
// application's envelope — a leak that is legal by design (the raw helpers
// are the documented escape hatch) but usually unintentional when a house
// envelope exists. Non-JSON writers (Text, Blob, XML, streaming) are exempt
// by design: they are never envelope targets.
const MetaRawResponse = "credo.rawresponse"

// warnEnvelopeBypass emits the debug-mode diagnostic after the handler chain
// (and any error handling) completes. At most one warning per request.
func (app *App) warnEnvelopeBypass(ctx *Context) {
	if !app.debug || app.successRenderer == nil || !ctx.response.envelopeBypassed {
		return
	}
	attrs := []slog.Attr{slog.String("path", ctx.Request().URL.Path)}
	if rt := ctx.Route(); rt != nil {
		if v, ok := rt.LookupMeta(MetaRawResponse); ok {
			if b, isBool := v.(bool); isBool && b {
				return
			}
		}
		attrs = append(attrs, slog.String("route", rt.GetPattern()))
		if name := rt.GetName(); name != "" {
			attrs = append(attrs, slog.String("route_name", name))
		}
	}
	ctx.Logger().LogAttrs(ctx.Request().Context(), slog.LevelWarn,
		"credo: response bypassed the success envelope; use ctx.Render or mark the route with MetaRawResponse",
		attrs...)
}

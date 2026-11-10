package credo

import (
	"io"
	"reflect"
	"slices"
)

// SetMultipartMaxMemory lowers the in-memory threshold BindBody uses when it
// parses a multipart form on app's requests, so that a test upload of a few
// KiB is spilled to temporary files. Call it before the App serves requests.
func SetMultipartMaxMemory(app *App, n int64) {
	app.multipartMaxMemory = n
}

// SourceFieldPath exposes sourceFieldPath for testing.
var SourceFieldPath = sourceFieldPath

// SourceFieldNames exposes sourceFieldNames for testing.
var SourceFieldNames = sourceFieldNames

// SetLogSectionOutput redirects the logger New builds from the "log" section
// to w and returns the function that restores stderr. Tests of the section
// are not parallel.
func SetLogSectionOutput(w io.Writer) (restore func()) {
	prev := logSectionOutput
	logSectionOutput = w
	return func() { logSectionOutput = prev }
}

// View returns the route's read view, the [RouteInfo] that [App.GetRoute]
// returns for a named route, so a test can generate URLs from a route it did
// not name.
func (r *Route) View() RouteInfo { return r.snapshot() }

// SameRouteView reports whether a and b are the same view of one route: equal
// fields, the same resolved meta and the same route behind them.
func SameRouteView(a, b RouteInfo) bool {
	return a.Method == b.Method && slices.Equal(a.Methods, b.Methods) &&
		a.Pattern == b.Pattern && a.Host == b.Host && a.Name == b.Name &&
		a.Kind == b.Kind && a.AutoHead == b.AutoHead && a.RegisteredAt == b.RegisteredAt &&
		a.route == b.route && reflect.DeepEqual(a.meta, b.meta)
}

package credo

import "io"

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

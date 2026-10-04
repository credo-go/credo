package credo

// SetMultipartMaxMemory lowers the in-memory threshold BindBody uses when it
// parses a multipart form on app's requests, so that a test upload of a few
// KiB is spilled to temporary files. Call it before the App serves requests.
func SetMultipartMaxMemory(app *App, n int64) {
	app.multipartMaxMemory = n
}

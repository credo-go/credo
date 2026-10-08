# Hello, Credo

Run `go run .` from this directory. The example loads its local configuration and demonstrates static/parameter routes, JSON responses, and a QUERY endpoint with decoded request input. It has a separate module with an in-tree Credo replacement.

## Pre-v1 migration

**Applied through v0.24.0; no change was needed.** This example remains the minimal App profile: `credo.New`, routes and `Run`. Plain `New` retains recovery but performs no automatic request-ID generation or access logging. Do not add optional feature calls merely to preserve earlier implicit defaults here; SaaS demonstrates explicit request features. Framework errors and application logs still use the logger.

The example has no bindings, so it needs no explicit `Finalize`: `Run` calls it during preparation and keeps graceful-exit handling. The [SaaS example](../saas/README.md) shows the full documented bootstrap order, and the [example migration map](../README.md) links contracts and acceptance.

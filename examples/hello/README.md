# Hello, Credo

Run `go run .` from this directory. The example loads its local configuration and demonstrates static/parameter routes, JSON responses, and a QUERY endpoint with decoded request input. It has a separate module with an in-tree Credo replacement.

## Pre-v1 migration

**Pending implementation.** This example remains the minimal App profile. After the HTTP minor, plain New retains recovery but performs no automatic request-ID generation or access logging. Do not add optional feature calls merely to preserve the previous implicit defaults here; SaaS demonstrates explicit request features. Framework errors and application logs still use the logger.

There is no constructor-backed DI resolution to move in this sample. Run keeps implicit preparation and graceful-exit handling. The [example migration map](../README.md) links contracts and acceptance. Until implementation lands, the runnable source retains the current default-on request features.

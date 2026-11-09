package main

import (
	"log"
	"net/http"

	"github.com/credo-go/credo"
)

func main() {
	// credo.New auto-discovers config.json (then config.yaml, config.yml) in
	// the working directory and reads its "server" section — the only section
	// the framework reads for itself. The listen address comes from there, so
	// editing config.json changes where this serves. Everything else you put in
	// the file is yours: unmarshal it into a struct at startup and inject it
	// through DI, as the saas example does.
	app, err := credo.New()
	if err != nil {
		log.Fatal(err)
	}

	app.GET("/", func(c *credo.Context) error {
		return c.Response().JSON(http.StatusOK, map[string]string{
			"message": "Hello, Credo!",
		})
	})

	app.GET("/hello/{name}", func(c *credo.Context) error {
		name := c.Request().RouteParam("name")
		return c.Response().JSON(http.StatusOK, map[string]string{
			"message": "Hello, " + name + "!",
		})
	})

	// QUERY is safe and idempotent like GET, but carries a structured query in
	// the request body. Credo requires Content-Type before this handler runs.
	app.QUERY("/search", func(c *credo.Context) error {
		var query struct {
			Term string `json:"term"`
		}
		if err := c.Request().BindBody(&query); err != nil {
			return err
		}
		return c.Response().JSON(http.StatusOK, map[string]string{
			"term": query.Term,
		})
	})

	// Run returns nil on a graceful shutdown, so guard the call: log.Fatal
	// exits 1 unconditionally, which would report every clean SIGTERM stop as
	// a crash to whatever supervises the process.
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

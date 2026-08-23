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

	app.GET("/", func(ctx *credo.Context) error {
		return ctx.Response().JSON(http.StatusOK, map[string]string{
			"message": "Hello, Credo!",
		})
	})

	app.GET("/hello/{name}", func(ctx *credo.Context) error {
		name := ctx.Request().RouteParam("name")
		return ctx.Response().JSON(http.StatusOK, map[string]string{
			"message": "Hello, " + name + "!",
		})
	})

	log.Fatal(app.Run())
}

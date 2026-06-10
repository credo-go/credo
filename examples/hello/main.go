package main

import (
	"log"
	"net/http"

	"github.com/credo-go/credo"
)

func main() {
	app, err := credo.New(credo.WithAddr("", 8080))
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

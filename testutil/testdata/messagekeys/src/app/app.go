package app

import (
	"github.com/credo-go/credo"
	"github.com/credo-go/credo/validation"

	"example.com/src/keys"
)

func handler(c *credo.Context, name string) error {
	_ = c.Render(201, nil, credo.RenderMessageKey("user.created"))
	_ = credo.NewHTTPError(409).WithMessageKey(keys.TenantTaken)
	_ = credo.NewHTTPError(409).WithMessageKey("only.tr")
	_ = credo.NewHTTPError(400).WithMessageKey("app.undefined")
	_ = credo.NewHTTPError(400).WithMessageKey(credo.MessageKey(name))
	_ = c.Render(200, nil, credo.RenderMessageKey("render.undefined"))
	_ = validation.ValidationError{Field: "name", Code: "required", MessageKey: "validation.required"}
	_ = validation.ValidationError{Field: "name", Code: "required", MessageKey: "validation.undefined"}
	var info credo.ErrorInfo
	info.MessageKey = "problem.conflict"
	return nil
}

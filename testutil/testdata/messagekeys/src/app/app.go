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
	_ = validation.Violation{Field: "name", Code: "required", MessageKey: "validation.required"}
	_ = validation.Violation{Field: "name", Code: "required", MessageKey: "validation.undefined"}
	var info credo.ErrorInfo
	info.MessageKey = "problem.conflict"
	_ = c.T("user.created")
	_ = c.T("t.undefined")
	_ = c.TPlural("plural.undefined", 2)
	_ = c.T(credo.MessageKey("status." + name))
	_ = c.T(credo.MessageKey(keys.StatusPrefix + name))
	_ = c.T(credo.MessageKey(keys.StatusPrefix + "sub." + name))
	_ = c.T(credo.MessageKey("orphan." + name))
	_ = c.TPlural(credo.MessageKey(keys.OrphanPrefix+name), 2)
	_ = c.T(credo.MessageKey(name + ".title"))
	_ = c.T(credo.MessageKey("status" + name))
	_ = c.T(credo.MessageKey(keys.StatusPrefix + "active"))
	_ = c.T(credo.MessageKey(keys.StatusPrefix + "gone"))
	return nil
}

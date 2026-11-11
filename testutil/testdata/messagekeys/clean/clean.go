package clean

import "github.com/credo-go/credo"

const taken credo.MessageKey = "tenant.taken"

const statusPrefix = "status."

func handler(c *credo.Context, code string) error {
	_ = c.Render(201, nil, credo.RenderMessageKey("user.created"))
	_ = c.T("user.created")
	_ = c.T(taken)
	_ = c.TPlural("user.created", 2)
	_ = c.T(credo.MessageKey("status." + code))
	_ = c.TPlural(credo.MessageKey(statusPrefix+code), 2)
	_ = c.T(credo.MessageKey(statusPrefix + "active"))
	return credo.NewHTTPError(409).WithMessageKey(taken)
}

package clean

import "github.com/credo-go/credo"

const taken credo.MessageKey = "tenant.taken"

func handler(c *credo.Context) error {
	_ = c.Render(201, nil, credo.RenderMessageKey("user.created"))
	return credo.NewHTTPError(409).WithMessageKey(taken)
}

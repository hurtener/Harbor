package client

import (
	"context"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ControlReceipt recovers an exact input acknowledgement without enqueuing work.
func (c *client) ControlReceipt(ctx context.Context, request types.ControlReceiptRequest) (types.ControlReceiptResponse, error) {
	scope := c.scope()
	scope.Run = request.Identity.Run
	request.Identity = scope
	var out types.ControlReceiptResponse
	err := c.callMethod(ctx, methods.MethodControlReceipt, request, &out)
	return out, err
}

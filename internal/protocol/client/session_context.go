package client

import (
	"context"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// SessionsReconcileContext recovers settled evidence in this client's session.
// It never resumes an old run or repeats a tool. Unknown pending outcomes remain
// refused; a successful response contains no private execution payload.
func (c *client) SessionsReconcileContext(ctx context.Context, request types.SessionsReconcileContextRequest) (types.SessionsReconcileContextResponse, error) {
	request.Identity = c.scope()
	var out types.SessionsReconcileContextResponse
	err := c.callMethod(ctx, methods.MethodSessionsReconcileContext, request, &out)
	return out, err
}

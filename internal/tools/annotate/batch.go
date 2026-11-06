package annotate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// Batch implements protocol.Annotator. The projector calls it lazily for a
// non-empty admitted list. Approval and history are read once; OAuth status
// is memoized by runtime source only within this identity-scoped request.
func (a *Annotator) Batch(ctx context.Context, id identity.Identity) (toolsprotocol.AnnotationBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := identity.Validate(id); err != nil {
		return nil, fmt.Errorf("tools/annotate: batch: %w", err)
	}
	policies, err := a.approval.Policies(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("tools/annotate: batch approval policies: %w", err)
	}
	evs, _ := a.toolHistory(ctx, id, []events.EventType{
		tools.EventTypeToolInvoked,
		tools.EventTypeToolCompleted,
		tools.EventTypeToolFailed,
	})
	lastUsed := make(map[string]time.Time)
	for _, ev := range evs {
		if name, ok := toolNameOf(ev); ok && ev.OccurredAt.After(lastUsed[name]) {
			lastUsed[name] = ev.OccurredAt
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Internally synchronized: a batch may annotate rows concurrently, but
	// neither the memo nor the identity ever escapes into another request.
	var mu sync.Mutex
	oauth := make(map[tools.ToolSourceID]prototypes.ToolOAuthStatus)
	return func(ctx context.Context, toolID string) toolsprotocol.ToolAnnotations {
		policy, ok := policies[toolID]
		if !ok {
			policy = prototypes.ToolApprovalAuto
		}
		out := toolsprotocol.ToolAnnotations{
			ApprovalPolicy: policy,
			LastUsedAt:     lastUsed[toolID],
			OAuthStatus:    prototypes.ToolOAuthNotApplicable,
		}
		if a.oauth != nil {
			if source, ok := a.sourceOf(toolID); ok {
				mu.Lock()
				status, seen := oauth[source]
				if !seen {
					status = a.oauth.Status(ctx, id, source)
					oauth[source] = status
				}
				mu.Unlock()
				out.OAuthStatus = status
			}
		}
		return out
	}, nil
}

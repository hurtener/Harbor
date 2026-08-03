package tasks

import (
	"context"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
)

const agentReachAdmissionSchema = "control.start/reach/v1"

type agentReachAdmissionContextKey struct{}

// AgentReachAdmission is the durable, non-wire receipt proving that the
// canonical control.start edge authorized one exact effective agent for one
// identity triple. Callers cannot submit it through SpawnRequest; the engine
// captures it only from the private context seal minted below.
type AgentReachAdmission struct {
	Schema           string
	TenantID         string
	UserID           string
	SessionID        string
	EffectiveAgentID string
}

// WithAgentReachAdmission seals a successful control.start reach decision on
// ctx. It is intentionally internal to Harbor and is not re-exported by the
// public SDK facade.
func WithAgentReachAdmission(ctx context.Context, id identity.Identity, effectiveAgentID string) context.Context {
	if ctx == nil || identity.Validate(id) != nil || strings.TrimSpace(effectiveAgentID) == "" {
		return ctx
	}
	return context.WithValue(ctx, agentReachAdmissionContextKey{}, AgentReachAdmission{
		Schema: agentReachAdmissionSchema, TenantID: id.TenantID, UserID: id.UserID,
		SessionID: id.SessionID, EffectiveAgentID: effectiveAgentID,
	})
}

// CaptureAgentReachAdmission returns a detached receipt only when ctx carries
// the private seal for the exact spawn identity and any explicitly requested
// agent. An ordinary SDK Spawn therefore cannot manufacture credential
// authority merely by setting SpawnRequest.AgentID.
func CaptureAgentReachAdmission(ctx context.Context, id identity.Identity, requestedAgentID string) *AgentReachAdmission {
	if ctx == nil {
		return nil
	}
	admission, ok := ctx.Value(agentReachAdmissionContextKey{}).(AgentReachAdmission)
	if !ok || !validAgentReachAdmission(admission, id) || (requestedAgentID != "" && admission.EffectiveAgentID != requestedAgentID) {
		return nil
	}
	copyAdmission := admission
	return &copyAdmission
}

// RestoreAgentReachAdmission verifies a persisted task receipt and, on
// success, restores the private context seal used by deterministic child
// spawns. It returns admitted=false for historical, malformed, tampered, or
// cross-identity records; those tasks keep their legacy execution behavior but
// gain no signed-capability credential authority.
func RestoreAgentReachAdmission(ctx context.Context, task *Task) (restored context.Context, effectiveAgentID string, admitted bool) {
	if task == nil || task.AgentReachAdmission == nil || !validAgentReachAdmission(*task.AgentReachAdmission, task.Identity.Identity) {
		return ctx, "", false
	}
	admission := *task.AgentReachAdmission
	if task.AgentID != "" && task.AgentID != admission.EffectiveAgentID {
		return ctx, "", false
	}
	return context.WithValue(ctx, agentReachAdmissionContextKey{}, admission), admission.EffectiveAgentID, true
}

// AgentReachAdmissionsEqual compares optional durable receipts.
func AgentReachAdmissionsEqual(a, b *AgentReachAdmission) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func validAgentReachAdmission(admission AgentReachAdmission, id identity.Identity) bool {
	return admission.Schema == agentReachAdmissionSchema && identity.Validate(id) == nil &&
		admission.TenantID == id.TenantID && admission.UserID == id.UserID && admission.SessionID == id.SessionID &&
		strings.TrimSpace(admission.EffectiveAgentID) != ""
}

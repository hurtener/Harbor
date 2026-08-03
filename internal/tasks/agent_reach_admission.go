package tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
)

const agentReachAdmissionSchema = "control.start/reach/v2"

type agentReachAdmissionContextKey struct{}

// AgentReachAdmissionSealer is the narrow restart-stable authority used to
// authenticate durable control.start admission receipts. Production supplies
// the broker KEK-backed AES-GCM sealer; task persistence never receives the
// key or the ability to mint a receipt.
type AgentReachAdmissionSealer interface {
	Seal([]byte) ([]byte, error)
	Open([]byte) ([]byte, error)
}

// AgentReachAdmissionAuthority mints and verifies opaque durable admissions.
// It is deliberately constructed outside the task engine and handed only to
// the canonical control.start edge and run-start verifier.
type AgentReachAdmissionAuthority struct {
	sealer AgentReachAdmissionSealer
}

type agentReachAdmissionClaims struct {
	Schema           string `json:"schema"`
	TenantID         string `json:"tenant_id"`
	UserID           string `json:"user_id"`
	SessionID        string `json:"session_id"`
	EffectiveAgentID string `json:"effective_agent_id"`
}

type admittedAgentReach struct {
	receipt AgentReachAdmission
	claims  agentReachAdmissionClaims
}

// AgentReachAdmission is an opaque, authenticated, non-wire receipt. The
// plaintext subject is sealed under restart-stable runtime authority, so a
// coordinated edit of Task.AgentID and durable task JSON cannot forge reach.
type AgentReachAdmission struct {
	Envelope      []byte `json:"envelope"`
	BindingDigest []byte `json:"binding_digest"`
}

// NewAgentReachAdmissionAuthority constructs the bounded receipt authority.
func NewAgentReachAdmissionAuthority(sealer AgentReachAdmissionSealer) (*AgentReachAdmissionAuthority, error) {
	if sealer == nil {
		return nil, errors.New("tasks: agent reach admission sealer is required")
	}
	return &AgentReachAdmissionAuthority{sealer: sealer}, nil
}

// Admit seals a successful canonical control.start reach decision on ctx.
func (a *AgentReachAdmissionAuthority) Admit(ctx context.Context, id identity.Identity, effectiveAgentID string) (context.Context, error) {
	if a == nil || a.sealer == nil {
		return ctx, errors.New("tasks: agent reach admission authority is unavailable")
	}
	if ctx == nil {
		return nil, errors.New("tasks: agent reach admission context is nil")
	}
	if err := identity.Validate(id); err != nil {
		return ctx, fmt.Errorf("tasks: agent reach admission identity: %w", err)
	}
	if strings.TrimSpace(effectiveAgentID) == "" {
		return ctx, errors.New("tasks: agent reach admission effective agent is empty")
	}
	claims := agentReachAdmissionClaims{Schema: agentReachAdmissionSchema, TenantID: id.TenantID,
		UserID: id.UserID, SessionID: id.SessionID, EffectiveAgentID: effectiveAgentID}
	plain, err := json.Marshal(claims)
	if err != nil {
		return ctx, fmt.Errorf("tasks: marshal agent reach admission: %w", err)
	}
	envelope, err := a.sealer.Seal(plain)
	if err != nil {
		return ctx, fmt.Errorf("tasks: seal agent reach admission: %w", err)
	}
	digest := sha256.Sum256(plain)
	admitted := admittedAgentReach{receipt: AgentReachAdmission{
		Envelope: envelope, BindingDigest: append([]byte(nil), digest[:]...),
	}, claims: claims}
	return context.WithValue(ctx, agentReachAdmissionContextKey{}, admitted), nil
}

// CaptureAgentReachAdmission returns a detached opaque receipt only for the
// exact spawn subject. An ordinary SDK Spawn cannot manufacture the private
// context value merely by setting SpawnRequest.AgentID.
func CaptureAgentReachAdmission(ctx context.Context, id identity.Identity, requestedAgentID string) *AgentReachAdmission {
	if ctx == nil {
		return nil
	}
	admitted, ok := ctx.Value(agentReachAdmissionContextKey{}).(admittedAgentReach)
	if !ok || !validAgentReachClaims(admitted.claims, id) ||
		(requestedAgentID != "" && admitted.claims.EffectiveAgentID != requestedAgentID) {
		return nil
	}
	return &AgentReachAdmission{
		Envelope:      append([]byte(nil), admitted.receipt.Envelope...),
		BindingDigest: append([]byte(nil), admitted.receipt.BindingDigest...),
	}
}

// Restore verifies an authenticated persisted receipt before restoring the
// private context value used by deterministic child spawns. Historical,
// malformed, tampered, cross-identity, and coherently rewritten records gain
// no signed-capability credential authority.
func (a *AgentReachAdmissionAuthority) Restore(ctx context.Context, task *Task) (context.Context, string, bool) {
	if a == nil || a.sealer == nil || task == nil || task.AgentReachAdmission == nil || len(task.AgentReachAdmission.Envelope) == 0 {
		return ctx, "", false
	}
	plain, err := a.sealer.Open(task.AgentReachAdmission.Envelope)
	if err != nil {
		return ctx, "", false
	}
	var claims agentReachAdmissionClaims
	if err := json.Unmarshal(plain, &claims); err != nil || !validAgentReachClaims(claims, task.Identity.Identity) {
		return ctx, "", false
	}
	digest := sha256.Sum256(plain)
	if !bytes.Equal(task.AgentReachAdmission.BindingDigest, digest[:]) {
		return ctx, "", false
	}
	if task.AgentID != "" && task.AgentID != claims.EffectiveAgentID {
		return ctx, "", false
	}
	receipt := AgentReachAdmission{
		Envelope:      append([]byte(nil), task.AgentReachAdmission.Envelope...),
		BindingDigest: append([]byte(nil), task.AgentReachAdmission.BindingDigest...),
	}
	return context.WithValue(ctx, agentReachAdmissionContextKey{}, admittedAgentReach{receipt: receipt, claims: claims}), claims.EffectiveAgentID, true
}

// AgentReachAdmissionsEqual compares optional opaque durable receipts by the
// canonical subject digest, not randomized ciphertext, so an exact current
// control.start retry remains idempotent. Historical plaintext v1 receipts are
// intentionally not upgraded here: accepting them as authority would reopen
// the forgery this format closes; any compatibility migration remains explicit
// debt outside this security repair.
func AgentReachAdmissionsEqual(a, b *AgentReachAdmission) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return bytes.Equal(a.BindingDigest, b.BindingDigest)
}

func validAgentReachClaims(claims agentReachAdmissionClaims, id identity.Identity) bool {
	return claims.Schema == agentReachAdmissionSchema && identity.Validate(id) == nil &&
		claims.TenantID == id.TenantID && claims.UserID == id.UserID && claims.SessionID == id.SessionID &&
		strings.TrimSpace(claims.EffectiveAgentID) != ""
}

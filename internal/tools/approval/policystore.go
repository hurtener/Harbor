package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

// The per-tool approval-policy override store.
//
// The approval subsystem's runtime decision engine (ApprovalGate +
// ApprovalPolicy) answers "does THIS invocation need a HITL gate?" from
// tags + config. Separately, an operator can pin a tool to a durable
// auto / gated / denied posture via the Tools-page admin control
// (`tools.set_approval_policy`). That per-tool posture is runtime state
// the approval subsystem owns — NOT a Console shadow store for runtime
// entities. This
// store is its home: a StateStore-backed, identity-scoped read/write
// surface the catalog annotator reads (the operator lens) and the admin
// method writes (the persist path).
//
// # Scope
//
// The policy is keyed by the caller's full (tenant, user, session)
// triple — a session-scoped posture, isolation-safe by construction
// (session A's override never reaches session B's projection). A
// tenant-wide admin posture is a future elevation; this store never
// widens the triple.
//
// # Concurrent reuse
//
// The store holds only the immutable StateStore reference; the
// StateStore is itself safe for concurrent use, and every method's
// per-call state lives in its arguments (CLAUDE.md §5).

// policyKindPrefix namespaces the per-tool policy record in the
// StateStore. The full Kind is "tools.approval.policy.<toolID>".
const policyKindPrefix = "tools.approval.policy."

// ErrPolicyStoreMisconfigured — NewStatePolicyStore was called with a
// nil StateStore. Fails closed (CLAUDE.md §5) rather than building a
// store that would nil-panic on the first call.
var ErrPolicyStoreMisconfigured = errors.New("approval: NewStatePolicyStore requires a StateStore")

// PolicyStore is the read/write seam for a tool's durable approval
// posture. The V1 production implementation is statePolicyStore.
type PolicyStore interface {
	// Policy returns toolID's persisted approval policy for id, or
	// ToolApprovalAuto when no override was set (the honest default:
	// a tool with no pinned posture runs auto). It never fabricates a
	// non-default value.
	Policy(ctx context.Context, id identity.Identity, toolID string) (prototypes.ToolApprovalPolicy, error)
	// Policies returns the pinned policies in the complete identity scope.
	// An absent tool has the semantic default ToolApprovalAuto.
	Policies(ctx context.Context, id identity.Identity) (map[string]prototypes.ToolApprovalPolicy, error)
	// SetPolicy persists toolID's approval policy for id. The policy
	// MUST be a valid ToolApprovalPolicy.
	SetPolicy(ctx context.Context, id identity.Identity, toolID string, policy prototypes.ToolApprovalPolicy) error
}

// policyRecord is the JSON shape persisted in the StateStore. A single
// field today; a struct so future per-tool posture metadata (who set
// it, when) is additive.
type policyRecord struct {
	Policy string `json:"policy"`
}

// statePolicyStore is the V1 PolicyStore over a state.StateStore. The
// StateStore's driver pluralism (in-mem / SQLite / Postgres) supplies
// persistence parity; this store is single-concrete (§9 typed-wrapper
// pattern).
type statePolicyStore struct {
	store state.StateStore
}

// NewStatePolicyStore builds the per-tool approval-policy store over a
// StateStore. The store is mandatory. The returned PolicyStore is
// immutable after construction and safe for concurrent use.
func NewStatePolicyStore(store state.StateStore) (PolicyStore, error) {
	if store == nil {
		return nil, ErrPolicyStoreMisconfigured
	}
	return &statePolicyStore{store: store}, nil
}

func policyKind(toolID string) string {
	return policyKindPrefix + toolID
}

// Policy implements PolicyStore.Policy.
func (s *statePolicyStore) Policy(ctx context.Context, id identity.Identity, toolID string) (prototypes.ToolApprovalPolicy, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := identity.Validate(id); err != nil {
		return "", fmt.Errorf("approval: policy read: %w", err)
	}
	if strings.TrimSpace(toolID) == "" {
		return "", fmt.Errorf("approval: policy read: tool id is empty")
	}
	q := identity.Quadruple{Identity: id}
	rec, err := s.store.Load(ctx, q, policyKind(toolID))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			// No pinned posture — the honest default is auto. Never a
			// fabricated non-default value (CLAUDE.md §13).
			return prototypes.ToolApprovalAuto, nil
		}
		return "", fmt.Errorf("approval: policy load: %w", err)
	}
	return decodePolicy(rec.Bytes)
}

func decodePolicy(data []byte) (prototypes.ToolApprovalPolicy, error) {
	var pr policyRecord
	if err := json.Unmarshal(data, &pr); err != nil {
		return "", fmt.Errorf("approval: policy decode: %w", err)
	}
	policy := prototypes.ToolApprovalPolicy(pr.Policy)
	if !prototypes.IsValidToolApprovalPolicy(policy) {
		return "", fmt.Errorf("approval: policy decode: stored policy %q is not a valid approval policy", pr.Policy)
	}
	return policy, nil
}

// Policies implements PolicyStore.Policies with one identity-scoped list. A
// store failure fails the read; an individually corrupt record is skipped.
func (s *statePolicyStore) Policies(ctx context.Context, id identity.Identity) (map[string]prototypes.ToolApprovalPolicy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := identity.Validate(id); err != nil {
		return nil, fmt.Errorf("approval: policies read: %w", err)
	}
	records, err := s.store.ListKindForIdentity(ctx, identity.Quadruple{Identity: id}, policyKindPrefix)
	if err != nil {
		return nil, fmt.Errorf("approval: policies list: %w", err)
	}
	policies := make(map[string]prototypes.ToolApprovalPolicy, len(records))
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		policy, err := decodePolicy(rec.Bytes)
		if err != nil {
			// One corrupt override must not fail every list for this identity.
			// Skipping it projects the tool's default (auto), exactly what the
			// single-tool read reports for the same record; the corruption is
			// logged (kind only, never the bytes). Store errors still fail.
			slog.WarnContext(ctx, "approval: skipping corrupt approval-policy override",
				slog.String("kind", rec.Kind), slog.String("error", err.Error()))
			continue
		}
		policies[strings.TrimPrefix(rec.Kind, policyKindPrefix)] = policy
	}
	return policies, nil
}

// SetPolicy implements PolicyStore.SetPolicy.
func (s *statePolicyStore) SetPolicy(ctx context.Context, id identity.Identity, toolID string, policy prototypes.ToolApprovalPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := identity.Validate(id); err != nil {
		return fmt.Errorf("approval: policy write: %w", err)
	}
	if strings.TrimSpace(toolID) == "" {
		return fmt.Errorf("approval: policy write: tool id is empty")
	}
	if !prototypes.IsValidToolApprovalPolicy(policy) {
		return fmt.Errorf("approval: policy write: %q is not a valid approval policy", policy)
	}
	bytes, err := json.Marshal(policyRecord{Policy: string(policy)})
	if err != nil {
		return fmt.Errorf("approval: policy encode: %w", err)
	}
	q := identity.Quadruple{Identity: id}
	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: q,
		Kind:     policyKind(toolID),
		Bytes:    bytes,
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("approval: policy save: %w", err)
	}
	return nil
}

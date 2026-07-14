// Package annotate assembles the production per-tool Annotator the Tools
// catalog projector reads OAuth / approval / metrics / content-stats /
// last-used / display-mode annotations through.
//
// # The seam it fills
//
// The catalog projector (internal/tools/protocol) reads every per-tool
// annotation through an OPTIONAL Annotator seam. The projector ships
// correct; the annotations live in sibling subsystems the catalog does
// not itself own. This package is the concrete that aggregates them:
//
//   - OAuth binding status  ← tools/auth (read-time, no refresh).
//   - approval policy        ← tools/approval's per-tool policy store.
//   - last-used / metrics    ← the events stream (read-time windowed read).
//   - content-size stats     ← the events stream (MCP offload records).
//   - display modes          ← the MCP negotiation state.
//
// It also implements the two admin-mutation seams the projector delegates
// to — SetApprovalPolicy (persists through tools/approval) and RevokeOAuth
// (routes through tools/auth) — so the Tools-page admin controls persist
// through the owning runtime subsystems, never a Console shadow store for
// runtime entities.
//
// # No fabrication (CLAUDE.md §13)
//
// Every annotation is REAL runtime state or an honest representable
// absence: a source with no OAuth config reads "n/a"; a tool with no
// recorded invocations reads a zero-observation Healthy pill (an honest
// "no failures observed," never a degraded value); a policy with no
// pinned override reads "auto" (the semantic default). No method returns
// a canned test-grade value.
//
// # Concurrent reuse (CLAUDE.md §5)
//
// The Annotator is immutable after NewAnnotator: it holds only the
// catalog + sub-reader references (each internally synchronised or
// read-only). Every method's per-call state lives in its arguments and
// locals. One Annotator serves N concurrent projection goroutines
// safely; the concurrent-reuse test pins it under -race.
package annotate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// ErrMisconfigured — NewAnnotator was called without a mandatory
// dependency. Fails closed (CLAUDE.md §5) rather than building an
// annotator that would nil-panic on the first projection.
var ErrMisconfigured = errors.New("tools/annotate: NewAnnotator missing a mandatory dependency")

// OAuthReader is the read + revoke seam the annotator resolves a tool's
// OAuth binding status through. It keys on the tool's runtime Source
// (the provider that produced it), not its catalog name.
type OAuthReader interface {
	// Status returns the OAuth binding status for source under id. A
	// source with no OAuth configuration reads ToolOAuthNotApplicable.
	Status(ctx context.Context, id identity.Identity, source tools.ToolSourceID) prototypes.ToolOAuthStatus
	// Revoke revokes every OAuth binding for source under id and returns
	// the count revoked. A source with no bindings returns (0, nil).
	Revoke(ctx context.Context, id identity.Identity, source tools.ToolSourceID) (int64, error)
}

// DisplayReader is the seam the annotator reads a tool's negotiated
// MCP-Apps DisplayMode map through. Non-MCP tools read an empty map.
type DisplayReader interface {
	// Modes returns the negotiated MIME→mode map for the tool whose
	// runtime source is source, or an empty map when the tool advertises
	// no per-MIME display negotiation.
	Modes(ctx context.Context, id identity.Identity, source tools.ToolSourceID) map[string]string
}

// Deps carries the annotator's aggregation dependencies. Catalog and
// Approval are mandatory (the catalog resolves toolID→source; the policy
// store backs both the read and the admin write). Events / OAuth /
// Display are optional-but-honest: a nil Events reader yields
// zero-observation metrics; a nil OAuth reader reads "n/a"; a nil Display
// reader reads an empty mode map — each an honest representable absence,
// never a fabricated value.
type Deps struct {
	// Catalog resolves a wire toolID back to its runtime descriptor so
	// the annotator can read the tool's Source / Transport (the keys the
	// OAuth and display sub-readers need). Mandatory.
	Catalog tools.ToolCatalog
	// Approval is the per-tool approval-policy store — the read side of
	// the approval annotation AND the persist side of the admin write.
	// Mandatory.
	Approval approval.PolicyStore
	// Events is the canonical event bus the metrics / last-used /
	// content-stats reads window over (read-time, no new store). Optional.
	Events events.EventBus
	// OAuth resolves a tool's OAuth binding status + backs RevokeOAuth.
	// Optional — nil reads every tool "n/a".
	OAuth OAuthReader
	// Display resolves a tool's negotiated MCP DisplayMode map. Optional.
	Display DisplayReader
	// HeavyThresholdBytes is the configured heavy-content threshold
	// (RFC §6.5) the content-stats histogram reports + counts against.
	HeavyThresholdBytes int64
	// Clock is the annotator's notion of "now" for the metrics windows.
	// Nil ⇒ time.Now (UTC).
	Clock func() time.Time
}

// Annotator is the production Projector Annotator. Immutable after
// construction; safe for concurrent reuse.
type Annotator struct {
	catalog   tools.ToolCatalog
	approval  approval.PolicyStore
	events    events.EventBus
	oauth     OAuthReader
	display   DisplayReader
	heavy     int64
	clock     func() time.Time
	scanLimit int
}

// metricsScanLimit bounds a single per-tool windowed event read. Generous
// enough to cover a busy tool's 7-day terminal-event count without an
// unbounded scan (a window exceeding it under-counts honestly rather than
// forcing an unbounded read — the same posture as the events aggregator).
const metricsScanLimit = 4096

// NewAnnotator builds the production Annotator. Catalog + Approval are
// mandatory; the rest are optional-but-honest. The returned *Annotator is
// immutable after construction and safe for concurrent use by N
// goroutines.
func NewAnnotator(deps Deps) (*Annotator, error) {
	if deps.Catalog == nil {
		return nil, fmt.Errorf("%w: Catalog is nil", ErrMisconfigured)
	}
	if deps.Approval == nil {
		return nil, fmt.Errorf("%w: Approval policy store is nil", ErrMisconfigured)
	}
	clock := deps.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Annotator{
		catalog:   deps.Catalog,
		approval:  deps.Approval,
		events:    deps.Events,
		oauth:     deps.OAuth,
		display:   deps.Display,
		heavy:     deps.HeavyThresholdBytes,
		clock:     clock,
		scanLimit: metricsScanLimit,
	}, nil
}

// sourceOf resolves a wire toolID back to its runtime Source. The catalog
// Resolve is a name lookup; the tool's identity-scoped VISIBILITY was
// already decided by the projector (which only annotates rows it listed),
// so reading the descriptor here leaks nothing.
func (a *Annotator) sourceOf(toolID string) (tools.ToolSourceID, bool) {
	d, ok := a.catalog.Resolve(toolID)
	if !ok {
		return "", false
	}
	return d.Tool.Source, true
}

// OAuthStatus implements protocol.Annotator.
func (a *Annotator) OAuthStatus(ctx context.Context, id identity.Identity, toolID string) prototypes.ToolOAuthStatus {
	if a.oauth == nil {
		return prototypes.ToolOAuthNotApplicable
	}
	source, ok := a.sourceOf(toolID)
	if !ok {
		return prototypes.ToolOAuthNotApplicable
	}
	return a.oauth.Status(ctx, id, source)
}

// ApprovalPolicy implements protocol.Annotator. A read error degrades to
// the semantic default (auto) — an honest "no pinned posture," never a
// fabricated non-default — and is the projector's responsibility to
// surface; the annotator never lies with a gated/denied it cannot read.
func (a *Annotator) ApprovalPolicy(ctx context.Context, id identity.Identity, toolID string) prototypes.ToolApprovalPolicy {
	policy, err := a.approval.Policy(ctx, id, toolID)
	if err != nil || !prototypes.IsValidToolApprovalPolicy(policy) {
		return prototypes.ToolApprovalAuto
	}
	return policy
}

// DisplayModes implements protocol.Annotator.
func (a *Annotator) DisplayModes(ctx context.Context, id identity.Identity, toolID string) map[string]string {
	if a.display == nil {
		return map[string]string{}
	}
	source, ok := a.sourceOf(toolID)
	if !ok {
		return map[string]string{}
	}
	modes := a.display.Modes(ctx, id, source)
	if modes == nil {
		return map[string]string{}
	}
	return modes
}

// SetApprovalPolicy implements protocol.ApprovalPolicySetter — the
// persisting admin path. It routes back through the approval subsystem's
// policy store (never a Console shadow store for runtime entities). The Service emits
// the audit event on success.
func (a *Annotator) SetApprovalPolicy(ctx context.Context, id identity.Identity, toolID string, policy prototypes.ToolApprovalPolicy) error {
	if err := a.approval.SetPolicy(ctx, id, toolID, policy); err != nil {
		return fmt.Errorf("tools/annotate: set approval policy: %w", err)
	}
	return nil
}

// RevokeOAuth implements protocol.OAuthRevoker — the admin revoke path.
// It routes through tools/auth. A tool with no OAuth binding revokes
// zero (an honest count, never a fabricated success).
func (a *Annotator) RevokeOAuth(ctx context.Context, id identity.Identity, toolID string) (int64, error) {
	if a.oauth == nil {
		return 0, nil
	}
	source, ok := a.sourceOf(toolID)
	if !ok {
		return 0, nil
	}
	return a.oauth.Revoke(ctx, id, source)
}

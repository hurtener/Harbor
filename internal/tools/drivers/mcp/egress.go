package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
)

// egress.go — egress substitution on the outbound tool-call path: the
// runtime resolves an artifact id the model authored and places the
// resolved BYTES into the outbound request body, so a remote tool can
// read a large document without that document transiting the model's
// context.
//
// Three properties are load-bearing here and each is enforced by the
// ORDER of what follows rather than by a rule a contributor has to
// remember:
//
//  1. The substitution mutates ONLY the decoded argument map. The raw
//     argument JSON is never rewritten, because it is what the
//     trajectory persists, what the observation renders, what the
//     per-invocation content hash is computed over, and what the
//     durable app tool-context record replays into a browser. Writing
//     into the decoded map alone keeps the resolved value
//     dispatch-local.
//  2. The record is emitted BEFORE the wire request, fail-closed. A
//     substitution that could not be recorded does not happen. This is
//     the emit-then-act ordering the credential plane settled on after
//     an apply-then-emit path was found claiming a fail-closed posture
//     it did not have — and it is deliberately UNLIKE the best-effort
//     app-discovery emit next door, because this record is the whole
//     compensating control for a byte movement that would otherwise
//     leave no trace.
//  3. Resolution happens once per dispatched call, not once per retry
//     attempt. See the invocation closure in mcp.go.

// egressPlan is the result of one dispatched call's egress
// substitution: the decoded, substituted argument map that goes on the
// wire, and the content-free records of what was substituted.
//
// A zero egressPlan means "this tool maps no artifact parameters" — its
// nil args tell callTool to decode the raw arguments as it always has,
// so an unmapped tool's outbound frame is byte-identical to a build
// without this feature.
//
// It is per-invocation state on the goroutine stack, never a field on
// the Provider.
type egressPlan struct {
	// args is the decoded argument map carrying the substituted
	// artifactegress.Payload values. Nil when no substitution ran.
	args map[string]any
	// records is the FACT of each substitution — ids, sizes, digests,
	// never bytes.
	records []artifactegress.Record
}

// prepareEgress resolves every mapped artifact parameter for one
// dispatched call and records the fact of it, BEFORE any wire request
// is issued.
//
// It runs ahead of the reliability shell so the store is read once per
// dispatched call rather than once per attempt. It fails loud on every
// path — an absent resolver, a missing or non-string parameter, an
// unresolvable id, an oversize value, an unrecordable substitution —
// and no wire request is issued on any of them.
//
// A browser-driven MCP-App tool callback reaches this function with no
// run and therefore no seated resolver, and fails here with a typed
// error naming the tool and the reason. That is the posture, not an
// oversight: seating a second resolver on that path would have to close
// over the browser request's triple rather than a run's quadruple,
// producing a SECOND definition of what this feature can reach for one
// feature — and passing the raw id string through instead would hand
// the server "art-abc123" where it expects a document, which either
// fails in the server's own vocabulary or succeeds on garbage.
func (p *Provider) prepareEgress(ctx context.Context, name string, args json.RawMessage, mapping artifactegress.Mapping, maxBytes int) (egressPlan, error) {
	argMap := map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return egressPlan{}, fmt.Errorf("%w: decode args for artifact egress on tool %q: %w", tools.ErrToolInvalidArgs, name, err)
		}
	}
	records, err := artifactegress.Encode(ctx, argMap, mapping, name, maxBytes)
	if err != nil {
		// The wrap names the tool and keeps %w at every hop so the
		// dispatch layer's classifier still reads the resolution
		// sentinels through the chain: a cross-identity or unknown id
		// surfaces as the recoverable artifact-ref-not-found observation
		// the model can repair, while an absent resolver stays the
		// step-terminating operator-misconfiguration class.
		return egressPlan{}, fmt.Errorf("mcp: artifact egress for tool %q on server %q: %w", name, p.source, err)
	}
	if len(records) == 0 {
		return egressPlan{}, nil
	}
	// Fail-closed: the record is the compensating control, so a
	// substitution that could not be recorded does not happen.
	if err := p.publishArtifactEgressed(ctx, name, records); err != nil {
		return egressPlan{}, err
	}
	return egressPlan{args: argMap, records: records}, nil
}

// publishArtifactEgressed emits the canonical record of one call's
// substitutions on the driver's bus — the same bus, and therefore the
// same audit-redactor path, every other tool event rides.
//
// It is FAIL-CLOSED, unlike the best-effort app-discovery emit beside
// it, and the asymmetry is the point: an app-discovery event that fails
// to publish costs a renderer a mount, while an unrecorded substitution
// costs the deployment the only trace that content moved. The one real
// difference between egress substitution and an admin instructing a
// model to paste a document into a tool argument is that pasting leaves
// a trail; this record is what restores it, so it is a precondition of
// the call rather than an observation of it.
//
// A missing bus or a missing identity is likewise a refusal, not a
// skip: both mean the record cannot be written, and the caller's
// eligibility declaration bought a traceable byte movement, not an
// untraceable one.
func (p *Provider) publishArtifactEgressed(ctx context.Context, toolName string, records []artifactegress.Record) error {
	if p.cfg.Bus == nil {
		return fmt.Errorf("%w: server %q tool %q substituted %d artifact value(s) but no event bus is wired to record it",
			ErrArtifactEgressUnrecorded, p.source, toolName, len(records))
	}
	id, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("%w: server %q tool %q has no identity on the call context",
			ErrArtifactEgressUnrecorded, p.source, toolName)
	}
	q := identity.Quadruple{Identity: id}
	if quad, qok := identity.QuadrupleFrom(ctx); qok {
		q.RunID = quad.RunID
	}
	now := time.Now()
	ev := events.Event{
		Type:       EventTypeMCPArtifactEgressed,
		Identity:   q,
		OccurredAt: now,
		Payload: ArtifactEgressedPayload{
			Identity:   q,
			ServerID:   p.source,
			ToolName:   toolName,
			Records:    append([]artifactegress.Record(nil), records...),
			OccurredAt: now,
		},
	}
	if err := p.cfg.Bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("%w: server %q tool %q: %w", ErrArtifactEgressUnrecorded, p.source, toolName, err)
	}
	return nil
}

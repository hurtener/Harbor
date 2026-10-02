package artifactcontent

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/tools"
)

type invocationKey struct{}
type invocationBinding struct {
	ID, TaskID, Descriptor string
	Scope                  artifacts.ArtifactScope
}

// WithInvocation binds materialization to an already admitted runtime invocation.
// This context is installed by native dispatch, never decoded from a tool result.
func WithInvocation(ctx context.Context, id, taskID, descriptor string, scope artifacts.ArtifactScope) (context.Context, error) {
	if len(id) != 64 || taskID == "" || descriptor == "" || scope.Validate() != nil {
		return nil, ErrInvalidResult
	}
	if _, err := hex.DecodeString(id); err != nil {
		return nil, ErrInvalidResult
	}
	return context.WithValue(ctx, invocationKey{}, invocationBinding{ID: id, TaskID: taskID, Descriptor: descriptor, Scope: scope}), nil
}

// Witness is opaque runtime evidence, not a wire shape. JSON cannot construct
// it. Metadata-only tool output which resembles an artifact never creates one.
type Witness struct {
	binding invocationBinding
	refs    []tools.ArtifactContentRef
}

// Present reports whether verified materialization minted this value.
func (w Witness) Present() bool { return w.binding.ID != "" }

// References returns verified metadata only for the exact runtime invocation.
func (w Witness) References(ctx context.Context) ([]tools.ArtifactContentRef, error) {
	binding, ok := ctx.Value(invocationKey{}).(invocationBinding)
	if !ok || !w.Present() || binding != w.binding {
		return nil, ErrInvalidResult
	}
	return append([]tools.ArtifactContentRef(nil), w.refs...), nil
}

// WitnessCarrier is implemented by native drivers that materialize binary
// content before returning to dispatch. It never scans arbitrary result JSON.
type WitnessCarrier interface{ MaterializedArtifactWitness() Witness }

// MaterializeWithWitness preserves Materialize's safe projection and adds an
// opaque witness only under a native invocation binding. App/runless callers
// keep ordinary artifact behavior and receive no task-output authority.
func MaterializeWithWitness(ctx context.Context, st artifacts.ArtifactStore, scope artifacts.ArtifactScope, value any, producer string) (any, Witness, error) {
	var witness Witness
	projected, err := materialize(ctx, st, scope, value, producer, func(refs []tools.ArtifactContentRef) error {
		binding, ok := ctx.Value(invocationKey{}).(invocationBinding)
		if !ok {
			return nil
		}
		if binding.Scope != scope {
			return fmt.Errorf("%w: materialization invocation scope differs", ErrInvalidResult)
		}
		witness = Witness{binding: binding, refs: append([]tools.ArtifactContentRef(nil), refs...)}
		return nil
	})
	if err != nil {
		return nil, Witness{}, err
	}
	return projected, witness, nil
}

// ForDescriptor prevents a nested helper or resource call from inheriting the
// direct native descriptor's output authority. Artifact behavior is unchanged.
func ForDescriptor(ctx context.Context, descriptor string) context.Context {
	binding, ok := ctx.Value(invocationKey{}).(invocationBinding)
	if !ok || binding.Descriptor == descriptor {
		return ctx
	}
	return context.WithValue(ctx, invocationKey{}, struct{}{})
}

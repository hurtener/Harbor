package drafter

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// writer.go — the narrow artifact-write seam.
//
// The handler's ONLY mutation is ONE immutable caller-scoped SKILL.md
// artifact, written through this seam. The seam is deliberately
// write-only: no Get / GetRef / Exists / Delete / List surface, so a
// handler (or a future refactor) cannot read back, list, or delete
// artifacts through it. The scope is fixed at construction — a
// composition owner builds the writer from the invocation's verified
// run identity and the handler never supplies a scope.

// ArtifactNamespace is the content-addressed namespace drafts are
// stored under. The namespace participates in the ref id, so drafts
// and other artifacts of the same session never collide in the
// content-addressed id space.
const ArtifactNamespace = "skill-draft"

// ArtifactWriter is the narrow write-only seam the draft handler
// persists its one artifact through. Implementations MUST be safe for
// N concurrent goroutines on a single shared instance.
type ArtifactWriter interface {
	// Write persists data as one immutable caller-scoped artifact and
	// returns its canonical content-addressed ref. Identical
	// (scope, namespace, bytes) re-writes converge to the SAME ref —
	// content addressing makes replays idempotent.
	Write(ctx context.Context, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error)
}

// ScopedWriter adapts an ArtifactStore to the narrow seam under one
// fixed caller scope. The scope is stamped on every write; the
// handler cannot redirect it.
type ScopedWriter struct {
	scoped *artifacts.ScopedArtifacts
}

// NewScopedWriter constructs the narrow writer over store with the
// fixed scope (tenant/user/session; task is a provenance annotation
// and may be empty). An invalid scope or nil store is a wiring bug and
// fails loud.
func NewScopedWriter(store artifacts.ArtifactStore, scope artifacts.ArtifactScope) (*ScopedWriter, error) {
	if store == nil {
		return nil, fmt.Errorf("drafter: artifact store is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("drafter: invalid artifact scope: %w", err)
	}
	return &ScopedWriter{scoped: artifacts.NewScoped(store, scope)}, nil
}

// Scope returns the fixed scope the writer was constructed with
// (read-only diagnostic; the value is immutable).
func (w *ScopedWriter) Scope() artifacts.ArtifactScope {
	return w.scoped.Scope()
}

// Write persists data under the fixed caller scope. The opts'
// Namespace defaults to ArtifactNamespace when empty, so a
// composition owner cannot accidentally scatter drafts across
// namespaces.
func (w *ScopedWriter) Write(ctx context.Context, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if opts.Namespace == "" {
		opts.Namespace = ArtifactNamespace
	}
	return w.scoped.PutBytes(ctx, data, opts)
}

var _ ArtifactWriter = (*ScopedWriter)(nil)

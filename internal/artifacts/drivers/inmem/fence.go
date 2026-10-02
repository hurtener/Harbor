package inmem

import (
	"context"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// FenceScope permanently serializes erasure against every byte Put.
func (d *driver) FenceScope(ctx context.Context, s artifacts.ArtifactScope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed.Load() {
		return artifacts.ErrStoreClosed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	d.fences[s.Triple()] = true
	return nil
}

// ScopeFenced reads the durable owner tombstone without broadening scope.
func (d *driver) ScopeFenced(ctx context.Context, s artifacts.ArtifactScope) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.fences[s.Triple()], nil
}

var _ artifacts.ScopeFencer = (*driver)(nil)

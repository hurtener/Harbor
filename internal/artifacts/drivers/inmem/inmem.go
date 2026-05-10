// Package inmem is Harbor's V1 in-memory ArtifactStore driver. It is
// the floor: every deployment can fall back to it (per-process
// lifetime, no persistence), and the conformance suite drives it as
// the canonical reference for Phase 18 (SQLite-blob, Postgres-blob)
// and Phase 19 (S3-style) drivers.
//
// Internal model:
//
//   - A single map keyed on a struct key `(scope, id)` holds the ref;
//     a sibling map keyed by the same struct key holds the bytes. Two
//     maps (rather than a struct holding both) so the bytes slice can
//     be copy-on-read without copying ref fields.
//   - A single `sync.RWMutex` guards both maps. The driver does no
//     I/O so contention is bounded by Go's map throughput; a finer
//     lock structure would be premature.
//   - Bytes are deep-copied on Put and Get to defend against caller
//     mutation (mirrors `internal/state/drivers/inmem` precedent).
//   - `Close` flips an atomic flag; subsequent calls return wrapped
//     `ErrStoreClosed`. There are no driver-owned goroutines.
package inmem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
)

const (
	defaultNamespace = "default"
	defaultMimeBytes = "application/octet-stream"
	defaultMimeText  = "text/plain; charset=utf-8"
)

// New constructs an ArtifactStore directly. Exposed for tests that
// want to skip the registry; production callers go through
// `artifacts.Open`.
func New(_ config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
	return &driver{
		refs:  map[indexKey]artifacts.ArtifactRef{},
		blobs: map[indexKey][]byte{},
	}, nil
}

func init() {
	artifacts.Register("inmem", func(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
		return New(cfg)
	})
}

// indexKey is the composite primary key. Struct-typed (rather than
// string-concatenated) so tenant IDs containing delimiters can't
// collide.
type indexKey struct {
	Tenant  string
	User    string
	Session string
	Task    string
	ID      string
}

func keyFor(scope artifacts.ArtifactScope, id string) indexKey {
	return indexKey{
		Tenant:  scope.TenantID,
		User:    scope.UserID,
		Session: scope.SessionID,
		Task:    scope.TaskID,
		ID:      id,
	}
}

type driver struct {
	mu     sync.RWMutex
	refs   map[indexKey]artifacts.ArtifactRef
	blobs  map[indexKey][]byte
	closed atomic.Bool
}

// PutBytes implements artifacts.ArtifactStore. Content-addressed:
// `ID = "{namespace}_{sha256[:12]}"`. Re-Put with identical
// (scope, namespace, bytes) returns the existing ref (no duplicate).
func (d *driver) PutBytes(_ context.Context, scope artifacts.ArtifactScope, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return artifacts.ArtifactRef{}, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return artifacts.ArtifactRef{}, err
	}

	namespace := opts.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}
	mime := opts.MimeType
	if mime == "" {
		mime = defaultMimeBytes
	}

	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	id := fmt.Sprintf("%s_%s", namespace, hexDigest[:12])
	key := keyFor(scope, id)

	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.refs[key]; ok {
		// Dedup: same key already stored. Same content (sha matches by
		// construction since ID embeds the truncated hash). Return the
		// existing ref unchanged.
		return existing, nil
	}

	ref := artifacts.ArtifactRef{
		ID:        id,
		MimeType:  mime,
		SizeBytes: int64(len(data)),
		Filename:  opts.Filename,
		SHA256:    hexDigest,
		Scope:     scope,
		Namespace: namespace,
		Source:    cloneSource(opts.Source),
	}
	d.refs[key] = ref
	d.blobs[key] = cloneBytes(data)
	return ref, nil
}

// PutText implements artifacts.ArtifactStore.
func (d *driver) PutText(ctx context.Context, scope artifacts.ArtifactScope, text string, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if opts.MimeType == "" {
		opts.MimeType = defaultMimeText
	}
	return d.PutBytes(ctx, scope, []byte(text), opts)
}

// Get implements artifacts.ArtifactStore. Found-false is NOT an
// error.
func (d *driver) Get(_ context.Context, scope artifacts.ArtifactScope, id string) ([]byte, bool, error) {
	if d.closed.Load() {
		return nil, false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	blob, ok := d.blobs[keyFor(scope, id)]
	if !ok {
		return nil, false, nil
	}
	return cloneBytes(blob), true, nil
}

// GetRef implements artifacts.ArtifactStore. Found-false is NOT an
// error.
func (d *driver) GetRef(_ context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
	if d.closed.Load() {
		return nil, false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	ref, ok := d.refs[keyFor(scope, id)]
	if !ok {
		return nil, false, nil
	}
	out := ref
	out.Source = cloneSource(ref.Source)
	return &out, true, nil
}

// Exists implements artifacts.ArtifactStore.
func (d *driver) Exists(_ context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.refs[keyFor(scope, id)]
	return ok, nil
}

// Delete implements artifacts.ArtifactStore. Idempotent: returns
// `(false, nil)` for an absent key.
func (d *driver) Delete(_ context.Context, scope artifacts.ArtifactScope, id string) (bool, error) {
	if d.closed.Load() {
		return false, artifacts.ErrStoreClosed
	}
	if err := scope.Validate(); err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := keyFor(scope, id)
	_, ok := d.refs[key]
	if !ok {
		return false, nil
	}
	delete(d.refs, key)
	delete(d.blobs, key)
	return true, nil
}

// List implements artifacts.ArtifactStore. Empty fields in `filter`
// are wildcards: `ArtifactScope{TenantID: "A"}` lists every artifact
// under tenant A across users / sessions / tasks.
func (d *driver) List(_ context.Context, filter artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return nil, artifacts.ErrStoreClosed
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]artifacts.ArtifactRef, 0, len(d.refs))
	for _, ref := range d.refs {
		if !matchesFilter(ref.Scope, filter) {
			continue
		}
		copyRef := ref
		copyRef.Source = cloneSource(ref.Source)
		out = append(out, copyRef)
	}
	return out, nil
}

// Close implements artifacts.ArtifactStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	d.closed.Store(true)
	return nil
}

// matchesFilter implements the wildcard semantics: empty fields in
// `filter` match anything.
func matchesFilter(scope, filter artifacts.ArtifactScope) bool {
	if filter.TenantID != "" && scope.TenantID != filter.TenantID {
		return false
	}
	if filter.UserID != "" && scope.UserID != filter.UserID {
		return false
	}
	if filter.SessionID != "" && scope.SessionID != filter.SessionID {
		return false
	}
	if filter.TaskID != "" && scope.TaskID != filter.TaskID {
		return false
	}
	return true
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func cloneSource(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Compile-time assertion that driver satisfies artifacts.ArtifactStore.
var _ artifacts.ArtifactStore = (*driver)(nil)

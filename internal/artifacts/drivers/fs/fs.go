// Package fs is Harbor's filesystem ArtifactStore driver. It is the
// single-binary production target — durable across process restart,
// no external dependency, suitable for embedded deployments.
//
// Storage layout:
//
//	<root>/<tenant>/<user>/<session>/<task>/<namespace>/<id>
//	<root>/<tenant>/<user>/<session>/<task>/<namespace>/<id>.meta.json
//
// Empty `TaskID` becomes the literal directory `_` so the layout stays
// five levels deep (the consumer pattern is "session-scoped artifacts
// have empty task," and we want their directories distinguishable from
// a task literally named ""). The other identity components are
// mandatory and validated upstream.
//
// THE PATH CARRIES THE TASK; THE KEY DOES NOT. Reads resolve on the
// isolation triple, so the task directory cannot be reconstructed from
// the caller's scope — it is read off the STORED ref, whose
// `Scope.TaskID` is the producing task's provenance stamp. Every path
// this driver builds for an existing artifact therefore derives from
// `ref.Scope`, never from the caller's; only a first Put derives its
// directory from the caller, because only then is the caller the
// producer. That is what lets a triple-keyed read resolve ACROSS task
// directories without the layout changing, and it keeps artifacts
// written by any earlier build readable in place.
//
// Two properties follow for data already on disk. A restart's index
// rebuild can find the SAME triple + id under more than one task
// directory (an earlier build wrote per-task copies). Write order is
// not recoverable from a filesystem, so the collapse rule is stated
// rather than guessed: the LEXICOGRAPHICALLY SMALLEST `TaskID` wins,
// deterministically, regardless of walk order. The bytes are identical
// by content-addressing, so only the provenance stamp is at stake. And
// `Delete` sweeps every task directory under the triple, so a delete
// cannot report success while leaving a copy that a later Get
// resolves.
//
// Atomicity. Each Put writes blob + meta as a pair of `tmp.<id>` /
// `tmp.<id>.meta.json` files, then `os.Rename`s each in turn. On
// crash mid-write only fully-renamed pairs are visible; stray
// `tmp.*` files from a crash are best-effort cleaned by `New` at
// startup (any remaining ones are harmless — `Get` ignores them).
//
// Path safety. Per AGENTS.md §7 #5: every constructed path is run
// through `filepath.Clean` and then prefix-checked with the
// canonicalized root. The `Filename` field in `PutOpts` is metadata
// only — never used in path construction (the `id` is). The
// path-safety test exists as defense in depth.
//
// Concurrency. A single `sync.RWMutex` guards the in-memory ref
// index that mirrors disk; the index is rebuilt at `New` time by
// scanning `<root>` and re-reading `.meta.json` files. The lock
// scope spans both the index update and the disk operations to
// keep them atomic from the caller's POV. Cross-process concurrent
// access to the same root is OUT OF SCOPE for V1 (one binary, one
// root); a locking strategy for that lands when a phase needs it.
package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
)

const (
	defaultNamespace  = "default"
	defaultMimeBytes  = "application/octet-stream"
	defaultMimeText   = "text/plain; charset=utf-8"
	emptyTaskSentinel = "_"
	metaSuffix        = ".meta.json"
	tmpPrefix         = "tmp."
	dirPerm           = 0o755
	filePerm          = 0o644
)

// New constructs a filesystem ArtifactStore. `cfg.FSRoot` must be
// non-empty; the directory is created (`os.MkdirAll`) if absent.
// On startup, stray `tmp.*` files from crashed Puts are best-effort
// removed; the in-memory ref index is rebuilt by walking the tree
// and re-reading `.meta.json` files.
func New(cfg config.ArtifactsConfig) (artifacts.ArtifactStore, error) {
	if cfg.FSRoot == "" {
		return nil, fmt.Errorf("artifacts/fs: FSRoot must be set")
	}
	root, err := filepath.Abs(cfg.FSRoot)
	if err != nil {
		return nil, fmt.Errorf("artifacts/fs: resolve FSRoot %q: %w", cfg.FSRoot, err)
	}
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, fmt.Errorf("artifacts/fs: mkdir %q: %w", root, err)
	}
	d := &driver{
		root:  root,
		index: map[indexKey]artifacts.ArtifactRef{},
	}
	if err := d.cleanupTmp(); err != nil {
		return nil, fmt.Errorf("artifacts/fs: cleanup stale tmp: %w", err)
	}
	if err := d.rebuildIndex(); err != nil {
		return nil, fmt.Errorf("artifacts/fs: rebuild index: %w", err)
	}
	return d, nil
}

func init() {
	artifacts.Register("fs", New)
}

// indexKey is the composite primary key: the isolation triple plus the
// content-addressed id (mirrors the InMem driver's shape; struct-typed
// so identity components carrying delimiters can't collide).
//
// `TaskID` is deliberately ABSENT — it is a provenance annotation. The
// on-disk path still carries it; the stored ref is what maps a key back
// to its directory.
type indexKey struct {
	Tenant  string
	User    string
	Session string
	ID      string
}

func keyFor(scope artifacts.ArtifactScope, id string) indexKey {
	return indexKey{
		Tenant:  scope.TenantID,
		User:    scope.UserID,
		Session: scope.SessionID,
		ID:      id,
	}
}

type driver struct {
	root   string
	mu     sync.RWMutex
	index  map[indexKey]artifacts.ArtifactRef
	closed atomic.Bool
}

// PutBytes implements artifacts.ArtifactStore.
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

	if existing, ok := d.index[key]; ok {
		// Dedup: id already on disk under this triple — possibly in a
		// different task directory. First-writer-wins: return the stored
		// ref (carrying the first producer's stamp) and write nothing.
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

	dir, err := d.dirFor(scope, namespace)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/fs: mkdir %q: %w", dir, err)
	}

	blobPath, err := d.safeJoin(dir, id)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	metaPath, err := d.safeJoin(dir, id+metaSuffix)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}

	// Marshal meta first — if Source contains non-encodable values we
	// fail loudly here rather than after the blob is on disk.
	metaBytes, err := json.Marshal(ref)
	if err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/fs: marshal meta: %w", err)
	}

	tmpBlob, err := d.safeJoin(dir, tmpPrefix+id)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}
	tmpMeta, err := d.safeJoin(dir, tmpPrefix+id+metaSuffix)
	if err != nil {
		return artifacts.ArtifactRef{}, err
	}

	// Write blob to tmp + rename. On error, best-effort remove the tmp.
	if err := writeFileAtomic(tmpBlob, blobPath, data); err != nil {
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/fs: write blob: %w", err)
	}
	if err := writeFileAtomic(tmpMeta, metaPath, metaBytes); err != nil {
		// Best-effort: remove the orphan blob so the next Put with
		// these bytes can succeed cleanly. Ignore remove errors.
		_ = os.Remove(blobPath)
		return artifacts.ArtifactRef{}, fmt.Errorf("artifacts/fs: write meta: %w", err)
	}

	d.index[key] = ref
	return ref, nil
}

// PutText implements artifacts.ArtifactStore.
func (d *driver) PutText(ctx context.Context, scope artifacts.ArtifactScope, text string, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if opts.MimeType == "" {
		opts.MimeType = defaultMimeText
	}
	return d.PutBytes(ctx, scope, []byte(text), opts)
}

// Get implements artifacts.ArtifactStore. Resolves on the isolation
// triple; `scope.TaskID` is ignored and the blob path is derived from
// the stored ref's task. Found-false is NOT an error.
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
	ref, ok := d.index[keyFor(scope, id)]
	d.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	// The directory comes from the STORED ref, not the caller's scope:
	// the artifact lives under the producing task's directory and the
	// caller's task (if any) is not part of the read key.
	dir, err := d.dirFor(ref.Scope, ref.Namespace)
	if err != nil {
		return nil, false, err
	}
	blobPath, err := d.safeJoin(dir, id)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(blobPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Index/disk drift — surface loudly. (Reserved
			// `ErrNotFound` for actual error contexts; this is one.)
			return nil, false, fmt.Errorf("%w: blob missing for indexed ref %q",
				artifacts.ErrNotFound, id)
		}
		return nil, false, fmt.Errorf("artifacts/fs: read blob: %w", err)
	}
	return data, true, nil
}

// GetRef implements artifacts.ArtifactStore. Resolves on the isolation
// triple; the returned ref carries the stored provenance stamp.
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
	ref, ok := d.index[keyFor(scope, id)]
	if !ok {
		return nil, false, nil
	}
	out := ref
	out.Source = cloneSource(ref.Source)
	return &out, true, nil
}

// Exists implements artifacts.ArtifactStore. Resolves on the isolation
// triple; `scope.TaskID` is ignored.
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
	_, ok := d.index[keyFor(scope, id)]
	return ok, nil
}

// Delete implements artifacts.ArtifactStore. Resolves on the isolation
// triple and removes EVERY on-disk copy under it — the indexed one plus
// any sibling left in another task directory by an earlier build.
// Removing only the indexed copy would let a later Get resolve a
// leftover, so the delete would have reported a success it did not
// perform (CLAUDE.md §13). Idempotent.
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
	ref, ok := d.index[key]
	if !ok {
		return false, nil
	}
	if err := d.removeEveryCopy(ref.Scope, ref.Namespace, id); err != nil {
		return false, err
	}
	delete(d.index, key)
	return true, nil
}

// removeEveryCopy deletes `<task>/<namespace>/<id>` and its meta sibling
// under every task directory of the artifact's session, so no copy
// survives a Delete on the triple. Caller holds the write lock.
func (d *driver) removeEveryCopy(scope artifacts.ArtifactScope, namespace, id string) error {
	tasks, err := d.taskDirs(scope)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		copyScope := scope
		copyScope.TaskID = task
		dir, derr := d.dirFor(copyScope, namespace)
		if derr != nil {
			return derr
		}
		blobPath, jerr := d.safeJoin(dir, id)
		if jerr != nil {
			return jerr
		}
		metaPath, jerr := d.safeJoin(dir, id+metaSuffix)
		if jerr != nil {
			return jerr
		}
		if rerr := os.Remove(blobPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("artifacts/fs: remove blob: %w", rerr)
		}
		if rerr := os.Remove(metaPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("artifacts/fs: remove meta: %w", rerr)
		}
	}
	return nil
}

// taskDirs returns the `TaskID` values whose directories exist under the
// scope's session, with the `_` sentinel decoded back to the empty
// string. A missing session directory yields an empty slice rather than
// an error — nothing to sweep is not a failure.
func (d *driver) taskDirs(scope artifacts.ArtifactScope) ([]string, error) {
	sessionDir, err := d.safeJoin(d.root, scope.TenantID, scope.UserID, scope.SessionID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts/fs: read session dir %q: %w", sessionDir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == emptyTaskSentinel {
			out = append(out, "")
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// List implements artifacts.ArtifactStore. `filter.TenantID` is
// required; every other empty field is a wildcard within that tenant.
func (d *driver) List(_ context.Context, filter artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	if d.closed.Load() {
		return nil, artifacts.ErrStoreClosed
	}
	if err := filter.ValidateFilter(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]artifacts.ArtifactRef, 0, len(d.index))
	for _, ref := range d.index {
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

// dirFor returns the directory for `(scope, namespace)`. Path-safety
// guarded: the constructed path is `filepath.Clean`ed and prefix-
// checked against the driver's canonicalized root.
func (d *driver) dirFor(scope artifacts.ArtifactScope, namespace string) (string, error) {
	task := scope.TaskID
	if task == "" {
		task = emptyTaskSentinel
	}
	return d.safeJoin(d.root, scope.TenantID, scope.UserID, scope.SessionID, task, namespace)
}

// safeJoin joins parts to base and verifies the result lives under
// base. Defends against `..` traversal in any caller-supplied
// component (path-traversal guard per AGENTS.md §7 #5).
func (d *driver) safeJoin(base string, parts ...string) (string, error) {
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("artifacts/fs: empty path component")
		}
	}
	full := filepath.Clean(filepath.Join(append([]string{base}, parts...)...))
	canonical := filepath.Clean(d.root)
	if full != canonical && !strings.HasPrefix(full, canonical+string(filepath.Separator)) {
		return "", fmt.Errorf("artifacts/fs: path traversal rejected: %q outside %q", full, canonical)
	}
	return full, nil
}

// rebuildIndex walks `<root>` and re-reads every `.meta.json` to
// rebuild the in-memory index after restart. Stray `tmp.*` files
// (cleaned in cleanupTmp before this runs) are absent. Files that
// don't parse are logged via the returned error chain — callers
// surface the failure at New time.
//
// Collision rule. The index key is the isolation triple plus the id, so
// two meta files for the same key can appear when an earlier build
// wrote the same bytes under two task directories. Write order is not
// recoverable from a filesystem, so the survivor is chosen by an
// explicit, deterministic rule — the smallest `TaskID` — rather than by
// whichever entry the walk happened to reach first. The bytes are
// identical by content-addressing; only the provenance stamp differs.
func (d *driver) rebuildIndex() error {
	return filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), metaSuffix) {
			return nil
		}
		// Ignore stray tmp meta (cleanupTmp already removed them, but
		// be defensive in case a future cleanup misses them).
		if strings.HasPrefix(entry.Name(), tmpPrefix) {
			return nil
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G122: path comes from WalkDir over d.root, the operator-configured artifact dir (not attacker-controlled); entries are filtered to the meta suffix
		if err != nil {
			return fmt.Errorf("artifacts/fs: read meta %q: %w", path, err)
		}
		var ref artifacts.ArtifactRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return fmt.Errorf("artifacts/fs: parse meta %q: %w", path, err)
		}
		key := keyFor(ref.Scope, ref.ID)
		if existing, ok := d.index[key]; ok && existing.Scope.TaskID <= ref.Scope.TaskID {
			return nil
		}
		d.index[key] = ref
		return nil
	})
}

// cleanupTmp walks `<root>` and removes any `tmp.*` files (best
// effort; remove errors are ignored).
func (d *driver) cleanupTmp() error {
	return filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), tmpPrefix) {
			_ = os.Remove(path) //nolint:gosec // G122: path comes from WalkDir over d.root, the operator-configured artifact dir (not attacker-controlled); only tmp-prefixed entries are removed
		}
		return nil
	})
}

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

// writeFileAtomic writes data to tmp, then renames tmp → final. On
// rename failure, removes tmp. Caller is responsible for ensuring
// the parent directory exists.
func writeFileAtomic(tmpPath, finalPath string, data []byte) error {
	if err := os.WriteFile(tmpPath, data, filePerm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
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

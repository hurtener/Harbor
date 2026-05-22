package devdraft

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hurtener/Harbor/cmd/harbor/scaffold"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrIdentityMissing — a Store method was invoked without a
	// complete (tenant, user, session) triple in ctx. Fails closed
	// per CLAUDE.md §6 rule 9.
	ErrIdentityMissing = errors.New("devdraft: identity required")
	// ErrNotFound — the requested draft does not exist under the
	// caller's identity-scoped path.
	ErrNotFound = errors.New("devdraft: draft not found")
	// ErrUnsafePath — a path component (PATCH file path or Save
	// output dir) escaped its allowed root per CLAUDE.md §7 rule 5.
	ErrUnsafePath = errors.New("devdraft: unsafe path")
	// ErrUnknownTemplate — Create was invoked with a template name
	// not registered in the embedded scaffold template set.
	ErrUnknownTemplate = errors.New("devdraft: unknown template")
	// ErrInvalidName — Create or Save was invoked with a project
	// name that fails the scaffold engine's validation.
	ErrInvalidName = errors.New("devdraft: invalid project name")
	// ErrOutputDirExists — Save was invoked with an OutputDir that
	// already exists. Mirrors `scaffold.ErrOutputDirExists`'s no-
	// overwrite posture.
	ErrOutputDirExists = errors.New("devdraft: output directory already exists")
	// ErrValidationFailed — Save's pre-promotion validation pass
	// (config.Load + Validate against the rendered harbor.yaml)
	// failed. The wrapped error names the offending field.
	ErrValidationFailed = errors.New("devdraft: rendered config failed validation")
	// ErrIO — a filesystem operation failed during a Store method.
	// The wrapped error carries the offending path + operation.
	ErrIO = errors.New("devdraft: filesystem operation failed")
)

// defaultDraftFileMaxBytes is the largest file the PATCH endpoint
// accepts. 1 MiB is generous for source code; larger files signal an
// operator confusion (binaries do not belong in a draft tree).
const defaultDraftFileMaxBytes = 1 << 20

// projectNameRE mirrors the scaffold engine's name shape (lowercase
// alphanumeric / dash / underscore, 1–64 chars, leading alnum). The
// scaffold engine's `validateName` is unexported so we duplicate the
// regex here; the scaffold test suite + this package's tests both
// pin the shape, so divergence is caught at PR time.
var projectNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Store is the per-binary, per-operator-root draft scratchpad.
// Construction binds the on-disk Root + the bus + the logger; the
// returned Store is a compiled artifact (D-025) and safe to share
// across N concurrent goroutines.
type Store struct {
	root       string
	bus        events.EventBus
	logger     *slog.Logger
	maxFileLen int

	// entropyMu guards the monotonic-ULID entropy reader. The
	// ulid.MonotonicEntropy reader is NOT goroutine-safe per its
	// godoc; we serialise mint calls so concurrent Create requests
	// don't race.
	entropyMu sync.Mutex
	entropy   *ulid.MonotonicEntropy
}

// Options configures NewStore.
type Options struct {
	// Root is the directory the Store materialises draft trees
	// under. The Store appends `<tenant>/<user>/<session>/<draft_id>`
	// to this root. Typical operator value: `<cwd>/.harbor/drafts`.
	Root string
	// Bus is the events.EventBus the Store publishes lifecycle
	// events onto. Mandatory — a Store with no bus would silently
	// drop the observability surface, which violates CLAUDE.md §13
	// "fail loudly" + the Wave 11.5 §17.6 F1 lesson (test fixture
	// vs production divergence on bus wiring).
	Bus events.EventBus
	// Logger is the slog.Logger the Store writes lifecycle lines to.
	// Optional — nil falls back to slog.Default.
	Logger *slog.Logger
	// MaxFileBytes overrides the per-file size cap PATCH enforces.
	// Zero falls back to defaultDraftFileMaxBytes (1 MiB).
	MaxFileBytes int
}

// NewStore builds a Store. Both `Options.Root` and `Options.Bus` are
// mandatory; missing either is an immediate error (CLAUDE.md §13
// fail-loud on operator-facing seam).
func NewStore(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("devdraft: NewStore requires Options.Root")
	}
	if opts.Bus == nil {
		return nil, fmt.Errorf("devdraft: NewStore requires Options.Bus (events.EventBus)")
	}
	absRoot, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("devdraft: NewStore: resolve root %q: %w", opts.Root, err)
	}
	maxLen := opts.MaxFileBytes
	if maxLen <= 0 {
		maxLen = defaultDraftFileMaxBytes
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		root:       absRoot,
		bus:        opts.Bus,
		logger:     logger,
		maxFileLen: maxLen,
		entropy:    ulid.Monotonic(rand.Reader, 0),
	}, nil
}

// Draft is the in-memory snapshot Store.Get returns to a caller. It
// is a value type — the caller owns the returned slice references.
type Draft struct {
	// ID is the opaque ULID Store.Create minted.
	ID string `json:"id"`
	// Template is the template the draft was seeded from. Empty
	// when read via Get (V1 does not persist the template name —
	// see the Get godoc).
	Template string `json:"template,omitempty"`
	// CreatedAt is the wall-clock time the draft was materialised
	// (the draft root's ModTime).
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the most-recent on-disk mtime across the draft
	// tree. Useful for the Console's "last edit" column.
	UpdatedAt time.Time `json:"updated_at"`
	// Files is the deterministic-order list of rel paths under the
	// draft root, lexicographically sorted.
	Files []DraftFile `json:"files"`
}

// DraftFile is the (rel-path, content, size) tuple Store.Get returns
// per file. Content is the on-disk byte stream; callers that want
// metadata-only can read Size and skip Content.
type DraftFile struct {
	Path    string `json:"path"`
	Size    int    `json:"size"`
	Content []byte `json:"content"`
}

// CreateOptions configures Store.Create. Name is the seed-time
// project name (used by the scaffold engine as `harbor.yaml`'s
// service name + the Go package name). Template selects the embedded
// scaffold template; empty defaults to `scaffold.DefaultTemplate`.
type CreateOptions struct {
	Name     string
	Template string
}

// Create seeds a fresh draft under the caller's identity-scoped
// path using the chosen template. Returns the seeded Draft
// (including the minted DraftID + the list of seeded files).
//
// Identity is mandatory — a ctx missing the triple is rejected with
// ErrIdentityMissing (CLAUDE.md §6 rule 9). The on-disk path is
// `<root>/<tenant>/<user>/<session>/<draft_id>/` so concurrent
// operators (multiple `harbor dev` clients against the same
// `.harbor/drafts/` root) cannot collide.
//
// On success a `dev.draft.created` event lands on the bus carrying
// (DraftID, Template, FileCount).
func (s *Store) Create(ctx context.Context, opts CreateOptions) (*Draft, error) {
	id, err := s.mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateProjectName(opts.Name); err != nil {
		return nil, err
	}
	tmpl := opts.Template
	if tmpl == "" {
		tmpl = scaffold.DefaultTemplate
	}
	if !templateRegistered(tmpl) {
		return nil, fmt.Errorf("%w: %q (known: %s)", ErrUnknownTemplate, tmpl, strings.Join(scaffold.Templates(), ","))
	}

	draftID, err := s.newDraftID()
	if err != nil {
		return nil, fmt.Errorf("devdraft: mint id: %w", err)
	}
	identityRoot, err := s.identityRoot(id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(identityRoot, 0o755); err != nil {
		return nil, fmt.Errorf("%w: mkdir identity root %q: %w", ErrIO, identityRoot, err)
	}
	draftRoot := filepath.Join(identityRoot, draftID)
	if _, statErr := os.Stat(draftRoot); statErr == nil {
		return nil, fmt.Errorf("%w: draft root %q already exists", ErrIO, draftRoot)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: stat draft root %q: %w", ErrIO, draftRoot, statErr)
	}

	// Render via the SAME engine `harbor scaffold` + Store.Save
	// invoke — keeping the seed shape aligned with the promoted
	// output by construction.
	res, err := scaffold.Scaffold(scaffold.Options{
		Name:      opts.Name,
		Template:  tmpl,
		OutputDir: draftRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("devdraft: seed draft: %w", err)
	}

	now := time.Now().UTC()
	draft := &Draft{
		ID:        draftID,
		Template:  tmpl,
		CreatedAt: now,
		UpdatedAt: now,
		Files:     make([]DraftFile, 0, len(res.Files)),
	}
	for _, f := range res.Files {
		path := filepath.Join(draftRoot, f)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("%w: read seeded file %q: %w", ErrIO, f, readErr)
		}
		draft.Files = append(draft.Files, DraftFile{
			Path:    filepath.ToSlash(f),
			Size:    len(raw),
			Content: raw,
		})
	}

	s.publish(ctx, id, EventTypeDraftCreated, DraftCreatedPayload{
		DraftID:   draftID,
		Template:  tmpl,
		FileCount: len(draft.Files),
	})

	s.logger.InfoContext(ctx, "devdraft: created",
		slog.String("draft_id", draftID),
		slog.String("template", tmpl),
		slog.Int("files", len(draft.Files)),
	)
	return draft, nil
}

// Get returns a Draft snapshot for the named draft ID under the
// caller's identity. Returns ErrNotFound when the draft does not
// exist for the caller. Cross-identity reads are impossible — the
// on-disk path is composed from the identity before any file open.
//
// V1 does not persist the template name; the returned Draft.Template
// is empty. A future phase that adds a `.harbor/drafts/<id>/.meta`
// sidecar will flip this to the recorded template.
func (s *Store) Get(ctx context.Context, draftID string) (*Draft, error) {
	id, err := s.mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateDraftID(draftID); err != nil {
		return nil, err
	}
	draftRoot, err := s.draftRoot(id, draftID)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(draftRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, draftID)
		}
		return nil, fmt.Errorf("%w: stat draft %q: %w", ErrIO, draftID, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: draft root %q is not a directory", ErrIO, draftRoot)
	}

	files, latest, walkErr := walkDraft(draftRoot, s.maxFileLen)
	if walkErr != nil {
		return nil, walkErr
	}
	return &Draft{
		ID:        draftID,
		CreatedAt: st.ModTime().UTC(),
		UpdatedAt: latest,
		Files:     files,
	}, nil
}

// WriteFile updates a single file in the named draft. The relPath is
// path-traversal-checked under the draft root; oversize writes are
// rejected with a wrapped ErrIO. Parent directories are created on
// demand. On success a `dev.draft.updated` event lands on the bus.
func (s *Store) WriteFile(ctx context.Context, draftID, relPath string, content []byte) error {
	id, err := s.mustIdentity(ctx)
	if err != nil {
		return err
	}
	if err := validateDraftID(draftID); err != nil {
		return err
	}
	if len(content) > s.maxFileLen {
		return fmt.Errorf("%w: file %q exceeds per-file cap (%d bytes > %d)", ErrIO, relPath, len(content), s.maxFileLen)
	}
	draftRoot, err := s.draftRoot(id, draftID)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(draftRoot); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, draftID)
		}
		return fmt.Errorf("%w: stat draft %q: %w", ErrIO, draftID, statErr)
	}

	dest, err := resolveSafe(draftRoot, filepath.FromSlash(relPath))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("%w: mkdir parent of %q: %w", ErrIO, relPath, err)
	}
	//nolint:gosec // draft project source files are intended to be world-readable (0o644); they carry no secrets
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		return fmt.Errorf("%w: write %q: %w", ErrIO, relPath, err)
	}

	s.publish(ctx, id, EventTypeDraftUpdated, DraftUpdatedPayload{
		DraftID: draftID,
		Path:    filepath.ToSlash(relPath),
		Size:    len(content),
	})

	s.logger.InfoContext(ctx, "devdraft: updated",
		slog.String("draft_id", draftID),
		slog.String("path", relPath),
		slog.Int("size", len(content)),
	)
	return nil
}

// PreviewResult reports the outcome of Store.Preview. OK is true when
// the rendered `harbor.yaml` parses + validates via the in-process
// `internal/config` loader; the Errors slice carries the human-
// readable reasons when OK is false. The Phase 66 preview surface is
// a config-validation pass; a future phase upgrades this to a real
// dry-run that boots the draft against a sandboxed runtime.
type PreviewResult struct {
	OK     bool     `json:"ok"`
	Errors []string `json:"errors,omitempty"`
}

// Preview runs a validation-only dry-run against the named draft.
// V1: parse + Validate the rendered `harbor.yaml` via `internal/
// config.Load`. The seed harbor.yaml passes validation by
// construction; a draft that mutates the yaml to an invalid shape
// is caught here BEFORE the operator tries Save. On every preview a
// `dev.draft.previewed` event lands on the bus.
func (s *Store) Preview(ctx context.Context, draftID string) (*PreviewResult, error) {
	id, err := s.mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateDraftID(draftID); err != nil {
		return nil, err
	}
	draftRoot, err := s.draftRoot(id, draftID)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(draftRoot); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, draftID)
		}
		return nil, fmt.Errorf("%w: stat draft %q: %w", ErrIO, draftID, statErr)
	}
	yamlPath := filepath.Join(draftRoot, "harbor.yaml")
	if _, statErr := os.Stat(yamlPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			res := &PreviewResult{OK: false, Errors: []string{
				"draft is missing harbor.yaml — preview cannot validate the config",
			}}
			s.publish(ctx, id, EventTypeDraftPreviewed, DraftPreviewedPayload{DraftID: draftID, OK: false})
			return res, nil
		}
		return nil, fmt.Errorf("%w: stat harbor.yaml: %w", ErrIO, statErr)
	}
	_, loadErr := config.Load(ctx, yamlPath)
	res := &PreviewResult{OK: loadErr == nil}
	if loadErr != nil {
		res.Errors = []string{loadErr.Error()}
	}
	s.publish(ctx, id, EventTypeDraftPreviewed, DraftPreviewedPayload{DraftID: draftID, OK: res.OK})

	s.logger.InfoContext(ctx, "devdraft: previewed",
		slog.String("draft_id", draftID),
		slog.Bool("ok", res.OK),
	)
	return res, nil
}

// SaveOptions configures Store.Save. Name is the project name to
// label the promoted output (mirrors `harbor scaffold --name`).
// OutputDir is the operator-supplied output dir — the engine refuses
// to overwrite an existing dir.
type SaveOptions struct {
	Name      string
	OutputDir string
}

// SaveResult reports the promoted scaffold output.
type SaveResult struct {
	Name      string   `json:"name"`
	OutputDir string   `json:"output_dir"`
	Files     []string `json:"files"`
}

// Save promotes the named draft to a `harbor scaffold`-emitted
// layout under opts.OutputDir.
//
// Promotion order (every step fails loud; no partial writes):
//
//  1. Resolve identity from ctx.
//  2. Validate the project name + output dir shape.
//  3. Stat the draft root (ErrNotFound when absent).
//  4. Pre-promotion validation: `internal/config.Load + Validate`
//     against the draft's `harbor.yaml`. Refuses promotion with
//     ErrValidationFailed on any error — closes the seam at the
//     boundary, not at the operator's next `harbor validate`.
//  5. Canonicalise the operator-supplied output dir, assert it does
//     not exist (ErrOutputDirExists).
//  6. Copy the draft tree byte-for-byte to OutputDir. The draft IS
//     the rendered output — the seed shape came from the scaffold
//     engine and mutations the operator made via PATCH are load-
//     bearing. On any copy error the partial output is removed.
//
// On success a `dev.draft.saved` event lands on the bus.
func (s *Store) Save(ctx context.Context, draftID string, opts SaveOptions) (*SaveResult, error) {
	id, err := s.mustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateDraftID(draftID); err != nil {
		return nil, err
	}
	if err := validateProjectName(opts.Name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.OutputDir) == "" {
		return nil, fmt.Errorf("%w: SaveOptions.OutputDir is empty", ErrIO)
	}

	draftRoot, err := s.draftRoot(id, draftID)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(draftRoot); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, draftID)
		}
		return nil, fmt.Errorf("%w: stat draft %q: %w", ErrIO, draftID, statErr)
	}

	yamlPath := filepath.Join(draftRoot, "harbor.yaml")
	if _, statErr := os.Stat(yamlPath); statErr != nil {
		return nil, fmt.Errorf("%w: draft is missing harbor.yaml: %w", ErrValidationFailed, statErr)
	}
	if _, loadErr := config.Load(ctx, yamlPath); loadErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrValidationFailed, loadErr)
	}

	absOut, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve output dir %q: %w", ErrIO, opts.OutputDir, err)
	}
	if _, statErr := os.Stat(absOut); statErr == nil {
		return nil, fmt.Errorf("%w: %s", ErrOutputDirExists, absOut)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: stat output dir %q: %w", ErrIO, absOut, statErr)
	}

	files, copyErr := copyTree(draftRoot, absOut, s.maxFileLen)
	if copyErr != nil {
		_ = os.RemoveAll(absOut)
		return nil, fmt.Errorf("%w: copy draft → output: %w", ErrIO, copyErr)
	}

	s.publish(ctx, id, EventTypeDraftSaved, DraftSavedPayload{
		DraftID:   draftID,
		OutputDir: absOut,
		FileCount: len(files),
	})

	s.logger.InfoContext(ctx, "devdraft: saved",
		slog.String("draft_id", draftID),
		slog.String("output_dir", absOut),
		slog.Int("files", len(files)),
	)
	return &SaveResult{
		Name:      opts.Name,
		OutputDir: absOut,
		Files:     files,
	}, nil
}

// Discard removes the named draft from disk. Idempotent — discarding
// a draft that does not exist still emits the terminal event so
// observers see a clean lifecycle. On success a `dev.draft.discarded`
// event lands on the bus.
func (s *Store) Discard(ctx context.Context, draftID string) error {
	id, err := s.mustIdentity(ctx)
	if err != nil {
		return err
	}
	if err := validateDraftID(draftID); err != nil {
		return err
	}
	draftRoot, err := s.draftRoot(id, draftID)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(draftRoot); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			s.publish(ctx, id, EventTypeDraftDiscarded, DraftDiscardedPayload{DraftID: draftID})
			return nil
		}
		return fmt.Errorf("%w: stat draft %q: %w", ErrIO, draftID, statErr)
	}
	if err := os.RemoveAll(draftRoot); err != nil {
		return fmt.Errorf("%w: remove draft %q: %w", ErrIO, draftID, err)
	}
	s.publish(ctx, id, EventTypeDraftDiscarded, DraftDiscardedPayload{DraftID: draftID})

	s.logger.InfoContext(ctx, "devdraft: discarded",
		slog.String("draft_id", draftID),
	)
	return nil
}

// Root returns the absolute on-disk root the Store materialises
// drafts under. Exposed for tests + the devstack helper's debug-
// only inspection; production code should NOT path-join into this.
func (s *Store) Root() string { return s.root }

// ---- internal helpers --------------------------------------------

func (s *Store) mustIdentity(ctx context.Context) (identity.Identity, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return identity.Identity{}, ErrIdentityMissing
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("%w: %w", ErrIdentityMissing, err)
	}
	return id, nil
}

// identityRoot composes `<root>/<tenant>/<user>/<session>`. The
// components are validated by identity.Validate before reaching this
// helper; we still strip path separators here as defence-in-depth
// (CLAUDE.md §7 rule 5).
func (s *Store) identityRoot(id identity.Identity) (string, error) {
	for name, v := range map[string]string{
		"tenant_id":  id.TenantID,
		"user_id":    id.UserID,
		"session_id": id.SessionID,
	} {
		if v == "" || strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
			return "", fmt.Errorf("%w: identity component %s contains path separators or parent tokens: %q", ErrUnsafePath, name, v)
		}
	}
	identityRoot := filepath.Join(s.root, id.TenantID, id.UserID, id.SessionID)
	if !pathHasPrefix(identityRoot, s.root) {
		return "", fmt.Errorf("%w: %q escapes %q", ErrUnsafePath, identityRoot, s.root)
	}
	return identityRoot, nil
}

func (s *Store) draftRoot(id identity.Identity, draftID string) (string, error) {
	if err := validateDraftID(draftID); err != nil {
		return "", err
	}
	identityRoot, err := s.identityRoot(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(identityRoot, draftID), nil
}

// newDraftID mints a ULID. Wrapped in a mutex because
// `ulid.MonotonicEntropy` is NOT goroutine-safe per its godoc.
func (s *Store) newDraftID() (string, error) {
	s.entropyMu.Lock()
	defer s.entropyMu.Unlock()
	id, err := ulid.New(ulid.Timestamp(time.Now()), s.entropy)
	if err != nil {
		return "", fmt.Errorf("ulid: %w", err)
	}
	return id.String(), nil
}

// publish fans a SafePayload onto the bus under the caller's
// identity. Publish errors are logged but NOT propagated — the
// underlying file operation has already succeeded by the time
// publish runs; a bus rejection is an operator-visible warning, not
// a 5xx surface. The error IS still logged so an operator sees it.
func (s *Store) publish(ctx context.Context, id identity.Identity, t events.EventType, payload events.EventPayload) {
	if s.bus == nil {
		return
	}
	ev := events.Event{
		Type: t,
		Identity: identity.Quadruple{
			Identity: id,
		},
		Payload: payload,
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "devdraft: bus publish failed",
			slog.String("event_type", string(t)),
			slog.String("error", err.Error()),
		)
	}
}

// validateProjectName mirrors scaffold.validateName's contract. The
// scaffold engine's validateName is unexported; we duplicate the
// regex here. Tests on both sides keep the shapes aligned.
func validateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: project name must not be empty", ErrInvalidName)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%w: project name must not contain path separators (got %q)", ErrInvalidName, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: project name must not contain parent-directory tokens (got %q)", ErrInvalidName, name)
	}
	if !projectNameRE.MatchString(name) {
		return fmt.Errorf("%w: project name must match %s (got %q)", ErrInvalidName, projectNameRE.String(), name)
	}
	return nil
}

// validateDraftID rejects obviously-malformed draft IDs at the API
// boundary. The Store mints ULIDs (26-char Crockford base32); we
// accept any 1–64 char ASCII identifier that does NOT contain path
// separators or parent-dir tokens. The defence-in-depth here is
// path safety, not identity correctness — the disk layout is
// already identity-scoped.
func validateDraftID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: draft id is empty", ErrNotFound)
	}
	if len(id) > 64 {
		return fmt.Errorf("%w: draft id too long (max 64)", ErrNotFound)
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("%w: draft id contains path separators or parent tokens: %q", ErrUnsafePath, id)
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '-' || r == '_') {
			return fmt.Errorf("%w: draft id contains illegal character %q", ErrUnsafePath, r)
		}
	}
	return nil
}

// templateRegistered reports whether name is in scaffold.Templates().
func templateRegistered(name string) bool {
	for _, t := range scaffold.Templates() {
		if t == name {
			return true
		}
	}
	return false
}

// walkDraft walks a draft root and returns the deterministic-order
// list of files. Each file's content is loaded into memory; the
// per-file cap prevents pathological allocations.
func walkDraft(draftRoot string, maxFileLen int) ([]DraftFile, time.Time, error) {
	var files []DraftFile
	latest := time.Time{}
	walkErr := filepath.WalkDir(draftRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("info %s: %w", path, err)
		}
		if int(info.Size()) > maxFileLen {
			return fmt.Errorf("%w: file %q exceeds per-file cap (%d bytes > %d)", ErrIO, path, info.Size(), maxFileLen)
		}
		if mt := info.ModTime().UTC(); mt.After(latest) {
			latest = mt
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(draftRoot, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		files = append(files, DraftFile{
			Path:    filepath.ToSlash(rel),
			Size:    len(raw),
			Content: raw,
		})
		return nil
	})
	if walkErr != nil {
		return nil, time.Time{}, fmt.Errorf("%w: walk draft: %w", ErrIO, walkErr)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, latest, nil
}

// copyTree copies srcRoot's tree to dstRoot, preserving file
// contents but flattening modes to 0755 (dirs) and 0644 (files) —
// the same modes the scaffold engine uses. Returns the
// deterministic-order list of rel paths written.
func copyTree(srcRoot, dstRoot string, maxFileLen int) ([]string, error) {
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dstRoot, err)
	}
	var written []string
	walkErr := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		if rel == "." {
			return nil
		}
		dest := filepath.Join(dstRoot, rel)
		if !pathHasPrefix(dest, dstRoot) {
			return fmt.Errorf("dest %s escapes %s", dest, dstRoot)
		}
		if d.IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dest, err)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("info %s: %w", path, err)
		}
		if int(info.Size()) > maxFileLen {
			return fmt.Errorf("file %q exceeds per-file cap (%d bytes > %d)", path, info.Size(), maxFileLen)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", filepath.Dir(dest), err)
		}
		//nolint:gosec // promoted project source files are intended to be world-readable (0o644); they carry no secrets
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		written = append(written, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(written)
	return written, nil
}

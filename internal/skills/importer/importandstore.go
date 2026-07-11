// importandstore.go — the exported one-call ingest path.
// The Skills.md importer shipped a complete parse pipeline and
// then no shipped path invoked it: the only non-test consumers were
// devdraft's path-safety reuse ("a Harbor-defining
// feature that is unreachable is not shipped"). ImportAndStore
// composes the existing Import pipeline with the SkillStore upsert +
// the settled conflict policy, and is the ONE implementation the
// `harbor skill import` CLI verb thinly wraps — the verb is a caller,
// never a second implementation.

package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// ErrDuplicateSkillName — ImportAndStore found an existing skill with
// the same name under the calling identity and the caller did not
// pass WithOverwrite. Duplicate-name rejection is LOUD by default
// (RFC §6.7 conflict policy; CLAUDE.md §5 — no silent overwrite of
// operator-visible state).
var ErrDuplicateSkillName = errors.New("importer: skill name already exists (pass overwrite to replace)")

// ImportReport is the honest outcome summary ImportAndStore returns.
// The CLI verb renders it verbatim (human or --json); Go callers read
// it programmatically.
type ImportReport struct {
	// Name / Title identify the imported skill.
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	// Scope is the stamped visibility scope (frontmatter `scope:`,
	// default `project`).
	Scope skills.Scope `json:"scope"`
	// Steps / Attachments count what the parser found.
	Steps       int `json:"steps"`
	Attachments int `json:"attachments"`
	// ContentHash is the canonical sha256 of the skill body.
	ContentHash string `json:"content_hash"`
	// Overwrote is true when an existing same-name row was replaced
	// (only possible under WithOverwrite).
	Overwrote bool `json:"overwrote"`
}

// importStoreOptions carries the optional knobs.
type importStoreOptions struct {
	overwrite bool
}

// ImportStoreOption configures ImportAndStore.
type ImportStoreOption func(*importStoreOptions)

// importStoreMu serialises the duplicate-name gate + upsert window of
// ImportAndStore (see the function's concurrency note). Process-local
// by design: the §5 package-level-state rule's spirit is per-run
// state on artifacts — a package-level sync primitive guarding a
// CLI/embedder ingest path carries no identity and no run state.
var importStoreMu sync.Mutex

// WithOverwrite permits replacing an existing same-name skill. The
// store's own conflict policy still applies underneath — an existing
// `Origin=pack` row can only be replaced by another pack import (the
// generator may never overwrite it), per RFC §6.7.
func WithOverwrite() ImportStoreOption {
	return func(o *importStoreOptions) { o.overwrite = true }
}

// ImportAndStore reads the Skills.md file at `path`, runs the
// Import pipeline (frontmatter scan, validation, path-safe attachment
// resolution rooted at the file's directory), and upserts the
// resulting `Origin=pack` skill into `store` under `id`.
//
// Conflict policy (fail-loud, RFC §6.7):
//
//   - An existing skill with the same name under the calling identity
//     rejects with wrapped ErrDuplicateSkillName unless WithOverwrite
//     is passed.
//   - Under WithOverwrite the store's own policy still gates the
//     write (pack rows are protected from non-pack input; this path
//     always writes Origin=pack, so pack→pack replace succeeds).
//
// Attachments (inline `![alt](rel/path)` references) resolve against
// the file's directory via the path-safety helper (§7 rule
// 5) and upload through `deps.Store` scoped to the calling identity
// (TaskID = "skill-import" for provenance).
//
// Identity is mandatory: the triple must be fully populated or the
// store rejects the upsert with `skills.ErrIdentityRequired` (and
// emits `skill.identity_rejected`). Validation errors from the parser
// surface verbatim — `ErrMissingFrontmatter`, `ErrMalformedYAML`,
// `ErrMissingTrigger`, `ErrEmptySteps`, `ErrUnknownSection`,
// `ErrAttachmentOutsideRoot` — so callers (and the CLI verb) can
// print the validator's own message.
//
// Concurrency note: the
// duplicate-name gate is check-then-act — the V1 SkillStore interface
// has no conditional insert, so the Get→Upsert window is serialised
// behind a PROCESS-LOCAL mutex here. That fully closes the race for
// this surface's actual callers (the one-shot `harbor skill import`
// verb and single-process headless embedders); two imports of the
// same name from SEPARATE processes against a shared store can still
// both pass the gate (last write wins — benign for identical pack
// content, which converges). A store-level conditional create is the
// recorded follow-up if a multi-process ingestion path ever lands.
func ImportAndStore(
	ctx context.Context,
	id identity.Identity,
	store skills.SkillStore,
	deps Deps,
	path string,
	opts ...ImportStoreOption,
) (ImportReport, error) {
	if store == nil {
		return ImportReport{}, errors.New("importer: ImportAndStore called with nil SkillStore")
	}
	var o importStoreOptions
	for _, opt := range opts {
		opt(&o)
	}

	imp, err := New(deps)
	if err != nil {
		return ImportReport{}, fmt.Errorf("importer: %w", err)
	}
	defer func() { _ = imp.Close(ctx) }() // Close is a documented no-op flag flip; nothing to surface

	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ImportReport{}, fmt.Errorf("importer: resolve path %q: %w", path, err)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ImportReport{}, fmt.Errorf("importer: read %q: %w", path, err)
	}

	q := identity.Quadruple{Identity: id}
	skill, arts, err := imp.Import(ctx, ImportSource{
		Bytes:    data,
		PathHint: absPath,
		// Attachments resolve relative to the Skills.md file's own
		// directory — the operator-trusted root for a file they are
		// explicitly importing. path_safety.go blocks escapes.
		AllowedRoot: filepath.Dir(absPath),
		Scope: artifacts.ArtifactScope{
			TenantID:  id.TenantID,
			UserID:    id.UserID,
			SessionID: id.SessionID,
			TaskID:    "skill-import",
		},
	})
	if err != nil {
		return ImportReport{}, err
	}

	// Duplicate-name gate. Get under the same identity; ErrSkillNotFound
	// is the happy path. The gate+upsert window is serialised behind
	// the process-local importStoreMu (see the godoc's concurrency
	// note): without it, two concurrent same-name imports could both
	// observe not-found and silently last-write-win past the loud
	// ErrDuplicateSkillName posture.
	importStoreMu.Lock()
	defer importStoreMu.Unlock()
	overwrote := false
	if _, getErr := store.Get(ctx, q, skill.Name); getErr == nil {
		if !o.overwrite {
			return ImportReport{}, fmt.Errorf("%w: %q", ErrDuplicateSkillName, skill.Name)
		}
		overwrote = true
	} else if !errors.Is(getErr, skills.ErrSkillNotFound) {
		return ImportReport{}, fmt.Errorf("importer: pre-upsert lookup %q: %w", skill.Name, getErr)
	}

	if err := store.Upsert(ctx, q, skill); err != nil {
		return ImportReport{}, fmt.Errorf("importer: store %q: %w", skill.Name, err)
	}

	return ImportReport{
		Name:        skill.Name,
		Title:       skill.Title,
		Scope:       skill.Scope,
		Steps:       len(skill.Steps),
		Attachments: len(arts.PathToRef),
		ContentHash: skill.ContentHash,
		Overwrote:   overwrote,
	}, nil
}

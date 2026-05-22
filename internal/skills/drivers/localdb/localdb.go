// Package localdb is Harbor's SQLite-backed `skills.SkillStore`
// driver (Phase 37). It is the first leg of the skills persistence
// triad (LocalDB now, Portico post-V1) defined by RFC §6.7 + brief
// 04 §4.3.
//
// The driver is built on `modernc.org/sqlite` — a CGo-free SQLite
// engine (D-013, AGENTS.md §5). Builds remain `CGO_ENABLED=0`.
//
// Operating model:
//
//   - Database opened against `cfg.DSN`. Bare file paths and the
//     special `:memory:` sentinel are supported. URI-form DSNs
//     (`file:foo.db?...`) pass through with `_pragma` + `_txlock`
//     query params layered on top so per-connection PRAGMAs survive
//     `database/sql`'s connection lifecycle.
//   - WAL journal mode is pinned at open. `busy_timeout=5000`
//     absorbs `SQLITE_BUSY` retries. `db.SetMaxOpenConns(1)` pins
//     the pool to a single connection — matches Phase 15's
//     StateStore + Phase 25's MemoryStore for the same reason
//     (BEGIN IMMEDIATE doesn't honor busy_timeout across pool
//     connections; pinning serialises writers at the Go layer).
//   - The schema is applied via embedded `migrations/*.sql` files
//     (forward-only, AGENTS.md §13). The migration runner is
//     idempotent.
//   - FTS5 availability is detected at open by attempting to
//     execute a probe query against the `skills_fts` virtual table.
//     If FTS5 is unavailable, the driver still serves Search via
//     the regex/exact fallback ladder (brief 04 §4.4); read paths
//     never fail because of missing FTS5.
//
// Skill state lives in this driver's OWN `skills` + `skills_fts`
// tables — the LocalDB driver does NOT piggyback on the SQLite
// StateStore. The injected `events.EventBus` dep IS used (for the
// identity-rejection emit path AND the four `skill.*` audit
// events). The skills `Deps` struct does NOT carry a `StateStore`
// (D-034 analog).
//
// The driver self-registers under `"localdb"` from its `init()`.
// The production binary picks it up via blank import in
// `cmd/harbor/main.go`; tests may call `New` directly to skip the
// registry.
//
// Concurrency contract (D-025):
//
//   - The driver struct holds a `*sql.DB` (an internally-
//     synchronized connection pool, pinned to one connection), an
//     `atomic.Bool` close flag, and an immutable `ftsAvailable`
//     boolean computed at open. All safe for N concurrent goroutines
//     without external locking.
//   - Per-call state lives on the call stack / supplied `ctx`.
//     Nothing mutable on the driver ever crosses run boundaries.
package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	// modernc.org/sqlite registers the "sqlite" driver name with
	// database/sql via its own init().
	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

const (
	driverName    = "localdb"
	sqliteDriver  = "sqlite"
	busyTimeoutMs = 5000

	defaultListLimit  = 100
	maxListLimit      = 1000
	defaultSearchN    = 20
	maxSearchN        = 200
	queryHashHexChars = 16
)

// New constructs a SQLite-backed `skills.SkillStore` against
// `cfg.DSN`. Production callers go through `skills.Open`; tests may
// call `New` directly to skip the registry.
func New(cfg skills.ConfigSnapshot, deps skills.Deps) (skills.SkillStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("skills/localdb: deps.Bus is required")
	}
	if cfg.DSN == "" {
		return nil, errors.New(`skills/localdb: empty DSN; expected file path or "sqlite:" URI`)
	}

	dsn, err := augmentDSNForPragmas(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: augment DSN: %w", err)
	}

	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: sql.Open(%q): %w", cfg.DSN, err)
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := verifyJournalMode(ctx, db, cfg.DSN); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("skills/localdb: migrate: %w", err)
	}

	ftsAvail := detectFTS5(ctx, db)

	return &driver{
		db:           db,
		bus:          deps.Bus,
		ftsAvailable: ftsAvail,
	}, nil
}

func init() {
	skills.Register(driverName, New)
}

// driver is the SQLite-backed SkillStore. Safe for concurrent use by
// N goroutines.
type driver struct {
	db           *sql.DB
	bus          events.EventBus
	ftsAvailable bool

	// closed flips exactly once via CompareAndSwap in Close — the CAS
	// is the once-only guard, so no mutex is needed around teardown.
	closed atomic.Bool
}

// Compile-time assertion.
var _ skills.SkillStore = (*driver)(nil)

// Upsert implements skills.SkillStore.
func (d *driver) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	if d.closed.Load() {
		return skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.EmitIdentityRejected(ctx, d.bus, id, "Upsert")
	}
	if err := skill.Validate(); err != nil {
		return err
	}
	// Caller-supplied ContentHash is honored only when non-empty;
	// otherwise compute the canonical value. This lets the
	// importer + generator stamp their own hash early without the
	// driver recomputing.
	if skill.ContentHash == "" {
		skill.ContentHash = skills.CanonicalContentHash(skill)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("skills/localdb: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the original error is the surfaced one
		}
	}()

	var existingOrigin sql.NullString
	var existingHash sql.NullString
	err = tx.QueryRowContext(ctx, `
        SELECT origin, content_hash FROM skills
        WHERE tenant = ? AND user = ? AND session = ? AND scope = ? AND name = ?`,
		id.TenantID, id.UserID, id.SessionID, string(skill.Scope), skill.Name,
	).Scan(&existingOrigin, &existingHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// New row — fall through to insert.
	case err != nil:
		return fmt.Errorf("skills/localdb: probe existing: %w", err)
	default:
		if existingOrigin.String == string(skills.OriginPack) &&
			skill.Origin != skills.OriginPack {
			// Conflict policy: pack rows survive non-pack overwrites.
			payload := skills.SkillPackOverwriteRefusedPayload{
				Name:           skill.Name,
				ExistingOrigin: skills.OriginPack,
				IncomingOrigin: skill.Origin,
			}
			if pubErr := d.bus.Publish(ctx, events.Event{
				Type:       skills.EventTypeSkillPackOverwriteRefused,
				Identity:   id,
				OccurredAt: time.Now(),
				Payload:    payload,
			}); pubErr != nil {
				return fmt.Errorf("%w: emit pack_overwrite_refused: %w",
					skills.ErrPackOverwriteRefused, pubErr)
			}
			return fmt.Errorf("%w: name=%q existing_origin=pack incoming=%s",
				skills.ErrPackOverwriteRefused, skill.Name, skill.Origin)
		}
		if existingHash.String == skill.ContentHash {
			// Idempotent — emit and bail.
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("skills/localdb: commit idempotent probe: %w", err)
			}
			committed = true
			return d.emitUpserted(ctx, id, skill, true)
		}
	}

	now := time.Now().UTC()
	if skill.CreatedAt.IsZero() {
		skill.CreatedAt = now
	}
	skill.UpdatedAt = now

	tagsJSON, err := marshalStrings(skill.Tags)
	if err != nil {
		return err
	}
	stepsJSON, err := marshalStrings(skill.Steps)
	if err != nil {
		return err
	}
	preJSON, err := marshalStrings(skill.Preconditions)
	if err != nil {
		return err
	}
	failJSON, err := marshalStrings(skill.FailureModes)
	if err != nil {
		return err
	}
	rtJSON, err := marshalStrings(skill.RequiredTools)
	if err != nil {
		return err
	}
	rnsJSON, err := marshalStrings(skill.RequiredNS)
	if err != nil {
		return err
	}
	rtgJSON, err := marshalStrings(skill.RequiredTags)
	if err != nil {
		return err
	}
	extraJSON, err := marshalExtra(skill.Extra)
	if err != nil {
		return err
	}
	tagsText := strings.Join(skill.Tags, " ")

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO skills
            (tenant, user, session, scope, name, title, description, trigger,
             task_type, tags_json, tags_text, steps_json, preconditions_json,
             failure_modes_json, required_tools_json, required_ns_json,
             required_tags_json, origin, origin_ref, scope_tenant, scope_project,
             content_hash, created_at, updated_at, last_used, use_count, extra_json)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(tenant, user, session, scope, name) DO UPDATE SET
            title              = excluded.title,
            description        = excluded.description,
            trigger            = excluded.trigger,
            task_type          = excluded.task_type,
            tags_json          = excluded.tags_json,
            tags_text          = excluded.tags_text,
            steps_json         = excluded.steps_json,
            preconditions_json = excluded.preconditions_json,
            failure_modes_json = excluded.failure_modes_json,
            required_tools_json = excluded.required_tools_json,
            required_ns_json    = excluded.required_ns_json,
            required_tags_json  = excluded.required_tags_json,
            origin             = excluded.origin,
            origin_ref         = excluded.origin_ref,
            scope_tenant       = excluded.scope_tenant,
            scope_project      = excluded.scope_project,
            content_hash       = excluded.content_hash,
            updated_at         = excluded.updated_at,
            extra_json         = excluded.extra_json`,
		id.TenantID, id.UserID, id.SessionID, string(skill.Scope), skill.Name,
		skill.Title, skill.Description, skill.Trigger, skill.TaskType,
		tagsJSON, tagsText, stepsJSON, preJSON, failJSON,
		rtJSON, rnsJSON, rtgJSON,
		string(skill.Origin), skill.OriginRef,
		skill.ScopeTenantID, skill.ScopeProjectID,
		skill.ContentHash, skill.CreatedAt, skill.UpdatedAt, skill.LastUsed,
		skill.UseCount, extraJSON,
	); err != nil {
		return fmt.Errorf("skills/localdb: upsert exec: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("skills/localdb: commit upsert: %w", err)
	}
	committed = true
	return d.emitUpserted(ctx, id, skill, false)
}

func (d *driver) emitUpserted(ctx context.Context, id identity.Quadruple, s skills.Skill, idempotent bool) error {
	payload := skills.SkillUpsertedPayload{
		Name:        s.Name,
		Origin:      s.Origin,
		Scope:       s.Scope,
		ContentHash: s.ContentHash,
		Idempotent:  idempotent,
	}
	if err := d.bus.Publish(ctx, events.Event{
		Type:       skills.EventTypeSkillUpserted,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload:    payload,
	}); err != nil {
		return fmt.Errorf("skills/localdb: emit skill.upserted: %w", err)
	}
	return nil
}

// Get implements skills.SkillStore.
func (d *driver) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if d.closed.Load() {
		return skills.Skill{}, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.Skill{}, skills.EmitIdentityRejected(ctx, d.bus, id, "Get")
	}
	row := d.db.QueryRowContext(ctx, selectSkillsSQL+`
        WHERE tenant = ? AND user = ? AND session = ? AND name = ?
        LIMIT 1`,
		id.TenantID, id.UserID, id.SessionID, name)
	got, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skills.Skill{}, fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
		}
		return skills.Skill{}, fmt.Errorf("skills/localdb: Get scan: %w", err)
	}
	return got, nil
}

// List implements skills.SkillStore.
func (d *driver) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if d.closed.Load() {
		return nil, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return nil, skills.EmitIdentityRejected(ctx, d.bus, id, "List")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var sb strings.Builder
	// 3 identity args + limit + offset are always present; optional
	// scope / task_type / per-tag filters grow the slice past this.
	args := make([]any, 0, 5+len(filter.Tags))
	sb.WriteString(selectSkillsSQL)
	sb.WriteString(` WHERE tenant = ? AND user = ? AND session = ?`)
	args = append(args, id.TenantID, id.UserID, id.SessionID)
	if filter.Scope != "" {
		sb.WriteString(` AND scope = ?`)
		args = append(args, string(filter.Scope))
	}
	if filter.TaskType != "" {
		sb.WriteString(` AND task_type = ?`)
		args = append(args, filter.TaskType)
	}
	// Tag any-of filter: implemented by JSON contains via LIKE on
	// tags_text. For Phase 37 the corpus is small; a proper index
	// can land later if hot.
	for _, tag := range filter.Tags {
		sb.WriteString(` AND tags_text LIKE ?`)
		args = append(args, "%"+tag+"%")
	}
	sb.WriteString(` ORDER BY updated_at DESC, name ASC LIMIT ? OFFSET ?`)
	args = append(args, limit, filter.Offset)

	rows, err := d.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: List query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]skills.Skill, 0, limit)
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: List scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: List iterate: %w", err)
	}
	return out, nil
}

// Search implements skills.SkillStore.
func (d *driver) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	if d.closed.Load() {
		return nil, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return nil, skills.EmitIdentityRejected(ctx, d.bus, id, "Search")
	}
	if limit <= 0 {
		limit = defaultSearchN
	}
	if limit > maxSearchN {
		limit = maxSearchN
	}

	results, path, err := d.search(ctx, id, query, limit)
	if err != nil {
		return nil, err
	}
	// Emit audit event. QueryHash hides the raw text from the
	// audit pipeline (RFC §6.7: PII redaction at injection time
	// applies to the search corpus output; the search input is
	// hashed to keep correlation possible without leaking).
	if emitErr := d.bus.Publish(ctx, events.Event{
		Type:       skills.EventTypeSkillSearchExecuted,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload: skills.SkillSearchExecutedPayload{
			QueryHash: hashQuery(query),
			Path:      path,
			Limit:     limit,
			ResultN:   len(results),
		},
	}); emitErr != nil {
		return nil, fmt.Errorf("skills/localdb: emit skill.search_executed: %w", emitErr)
	}
	return results, nil
}

// Delete implements skills.SkillStore.
func (d *driver) Delete(ctx context.Context, id identity.Quadruple, name string) error {
	if d.closed.Load() {
		return skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.EmitIdentityRejected(ctx, d.bus, id, "Delete")
	}
	res, err := d.db.ExecContext(ctx, `
        DELETE FROM skills
        WHERE tenant = ? AND user = ? AND session = ? AND name = ?`,
		id.TenantID, id.UserID, id.SessionID, name)
	if err != nil {
		return fmt.Errorf("skills/localdb: Delete exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("skills/localdb: Delete rowcount: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
	}
	if err := d.bus.Publish(ctx, events.Event{
		Type:       skills.EventTypeSkillDeleted,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload:    skills.SkillDeletedPayload{Name: name},
	}); err != nil {
		return fmt.Errorf("skills/localdb: emit skill.deleted: %w", err)
	}
	return nil
}

// Close implements skills.SkillStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	// CompareAndSwap is the once-only guard: the goroutine that flips
	// closed false→true owns the db.Close(); every other concurrent
	// caller loses the swap and returns nil. Close is idempotent.
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("skills/localdb: close: %w", err)
	}
	return nil
}

// augmentDSNForPragmas mirrors memory/sqlite + state/sqlite.
func augmentDSNForPragmas(dsn string) (string, error) {
	if dsn == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	pragmas := []string{
		"busy_timeout(" + fmt.Sprint(busyTimeoutMs) + ")",
		"journal_mode(WAL)",
	}
	if strings.HasPrefix(dsn, "file:") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse file: URI: %w", err)
		}
		q := u.Query()
		for _, p := range pragmas {
			q.Add("_pragma", p)
		}
		if q.Get("_txlock") == "" {
			q.Set("_txlock", "immediate")
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	sep := "?"
	if strings.ContainsRune(dsn, '?') {
		sep = "&"
	}
	parts := make([]string, 0, len(pragmas)+1)
	for _, p := range pragmas {
		parts = append(parts, "_pragma="+url.QueryEscape(p))
	}
	parts = append(parts, "_txlock=immediate")
	return dsn + sep + strings.Join(parts, "&"), nil
}

func verifyJournalMode(ctx context.Context, db *sql.DB, originalDSN string) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("skills/localdb: read journal_mode: %w", err)
	}
	mode = strings.ToLower(mode)
	if isMemoryDSN(originalDSN) {
		return nil
	}
	if mode != "wal" {
		return fmt.Errorf("skills/localdb: journal_mode=%q after open; expected \"wal\" (DSN=%q)",
			mode, originalDSN)
	}
	return nil
}

func isMemoryDSN(dsn string) bool {
	if dsn == ":memory:" {
		return true
	}
	if strings.HasPrefix(dsn, "file:") && strings.Contains(dsn, ":memory:") {
		return true
	}
	return false
}

// detectFTS5 probes whether the SQLite build supports FTS5 by
// running a no-op query against the `skills_fts` virtual table.
// `modernc.org/sqlite` compiles FTS5 in by default, but this is the
// guard brief 04 §4.4 requires: read paths never fail because of
// missing FTS5 — they fall back through the ranking ladder.
func detectFTS5(ctx context.Context, db *sql.DB) bool {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM skills_fts WHERE skills_fts MATCH ?`, "__fts_probe__").Scan(&n)
	return err == nil
}

// marshalStrings serializes a string slice to JSON for storage.
// nil/empty round-trip as `[]`.
func marshalStrings(in []string) (string, error) {
	if in == nil {
		return "[]", nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("skills/localdb: marshal strings: %w", err)
	}
	return string(b), nil
}

func marshalExtra(extra map[string]any) (string, error) {
	if len(extra) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("skills/localdb: marshal extra: %w", err)
	}
	return string(b), nil
}

// unmarshalStrings reverses marshalStrings. Empty/null JSON returns
// a nil slice.
func unmarshalStrings(s string) []string {
	if s == "" || s == "[]" || s == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func unmarshalExtra(s string) map[string]any {
	if s == "" || s == "{}" || s == "null" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// skillCols is the comma-separated column list `scanSkill` consumes.
// Kept as a bare list (no `SELECT` / no `FROM skills`) so callers can
// compose it with extra columns (e.g. `, rowid`) and pick their FROM.
const skillCols = `name, title, description, trigger, task_type,
       tags_json, steps_json, preconditions_json, failure_modes_json,
       required_tools_json, required_ns_json, required_tags_json,
       origin, origin_ref, scope, scope_tenant, scope_project,
       content_hash, created_at, updated_at, last_used, use_count, extra_json`

// selectSkillsSQL is the canonical `SELECT ... FROM skills` prefix
// shared by Get / List / Search row-fetch paths.
const selectSkillsSQL = `SELECT ` + skillCols + ` FROM skills`

// scannable is the minimal interface both *sql.Row and *sql.Rows
// satisfy so scanSkill can serve both code paths.
type scannable interface {
	Scan(dest ...any) error
}

func scanSkill(r scannable) (skills.Skill, error) {
	var (
		s         skills.Skill
		tagsJSON  string
		stepsJSON string
		preJSON   string
		failJSON  string
		rtJSON    string
		rnsJSON   string
		rtgJSON   string
		origin    string
		scope     string
		extraJSON string
	)
	if err := r.Scan(
		&s.Name, &s.Title, &s.Description, &s.Trigger, &s.TaskType,
		&tagsJSON, &stepsJSON, &preJSON, &failJSON,
		&rtJSON, &rnsJSON, &rtgJSON,
		&origin, &s.OriginRef, &scope, &s.ScopeTenantID, &s.ScopeProjectID,
		&s.ContentHash, &s.CreatedAt, &s.UpdatedAt, &s.LastUsed, &s.UseCount, &extraJSON,
	); err != nil {
		return skills.Skill{}, err
	}
	s.Origin = skills.Origin(origin)
	s.Scope = skills.Scope(scope)
	s.Tags = unmarshalStrings(tagsJSON)
	s.Steps = unmarshalStrings(stepsJSON)
	s.Preconditions = unmarshalStrings(preJSON)
	s.FailureModes = unmarshalStrings(failJSON)
	s.RequiredTools = unmarshalStrings(rtJSON)
	s.RequiredNS = unmarshalStrings(rnsJSON)
	s.RequiredTags = unmarshalStrings(rtgJSON)
	s.Extra = unmarshalExtra(extraJSON)
	return s, nil
}

// Package postgres is Harbor's Postgres-backed `skills.SkillStore`
// driver. It brings the skills subsystem to the three-driver
// persistence parity every other persistence-shaped subsystem already
// has (in-memory / SQLite / Postgres) defined by RFC §6.7 + §9: skills
// persist durably in shared Postgres for multi-instance deployments
// instead of only a per-instance SQLite file.
//
// The driver uses `pgx/v5/stdlib` so the rest of Harbor sees a
// `database/sql.DB`. Parametric queries everywhere; no string
// concatenation into SQL (AGENTS.md §9). An advisory lock serialises
// the migration runner so multi-replica boots are race-free.
//
// Behavioural parity with the SQLite (`localdb`) driver is proven by
// the shared `internal/skills/conformancetest` suite, which this
// driver passes unchanged — no interface change, no `Supports*`
// capability ceremony (AGENTS.md §4.4). The conflict policy
// (pack-overwrite refusal, generated-to-generated idempotency, LWW),
// identity-scoped `WHERE` filters, and the search ranking ladder all
// match the SQLite driver.
//
// # Search
//
// Search feeds the same backend-agnostic ranking ladder the SQLite
// driver uses: full-text first, then a regex fallback, then an exact
// lowercase-equality fallback. The full-text tier rides a STORED
// generated `tsvector` column + a GIN index (Postgres' analogue of
// SQLite's FTS5 virtual table); the regex + exact tiers are pure-Go /
// parameterised-SQL over the identity-scoped rows, byte-for-byte the
// same scoring constants as the SQLite driver. Scores are normalised
// to 0..1 so ranking is backend-independent. The opt-in semantic
// retrieval mode ranks by embedding similarity over the identity-
// scoped catalog, mirroring the SQLite driver.
//
// Skill state lives in this driver's OWN `skills` table — the driver
// does NOT piggyback on the Postgres StateStore. The injected
// `events.EventBus` dep IS used (for the identity-rejection emit path
// AND the four `skill.*` audit events). The skills `Deps` struct does
// NOT carry a `StateStore`.
//
// The driver self-registers under `"postgres"` from its `init()`. The
// production binary picks it up via the `internal/drivers/prod`
// aggregator's blank import; tests may call `New` directly to skip the
// registry.
//
// Concurrency contract (AGENTS.md §5 "Concurrent reuse"):
//
//   - The driver struct holds a `*sql.DB` (an internally-synchronised
//     connection pool), an `atomic.Bool` close flag, and immutable
//     retrieval config computed at open. All safe for N concurrent
//     goroutines without external locking.
//   - Per-call state lives on the call stack / supplied `ctx`. Nothing
//     mutable on the driver ever crosses run boundaries.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// driverName is the name under which this driver self-registers with
// `skills.Register`.
const driverName = "postgres"

// pgxDriverName is the database/sql driver name registered by the pgx
// stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Values mirror the StateStore + MemoryStore
// Postgres drivers for consistency; tuning lives in a future config
// knob, not here.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 5 * time.Minute
)

// Search-limit bounds mirror the SQLite driver so the two backends cap
// identically.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
	defaultSearchN   = 20
	maxSearchN       = 200
)

// New constructs a Postgres-backed `skills.SkillStore` against
// `cfg.DSN`. Production callers go through `skills.Open`; tests may
// call `New` directly to skip the registry.
//
// `deps.Bus` is required. A misconfigured DSN fails loudly at boot via
// an eager ping — never on the first Upsert. Semantic retrieval
// (opt-in) requires the injected embedder; a nil embedder under
// `RetrievalSemantic` fails loudly at construction, never a stub
// fallback (AGENTS.md §13).
func New(cfg skills.ConfigSnapshot, deps skills.Deps) (skills.SkillStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("skills/postgres: deps.Bus is required")
	}
	if cfg.DSN == "" {
		return nil, errors.New("skills/postgres: cfg.DSN is required")
	}

	switch cfg.Retrieval {
	case skills.RetrievalDefault:
	case skills.RetrievalSemantic:
		if deps.Embedder == nil {
			return nil, fmt.Errorf("skills/postgres: deps.Embedder is required for retrieval mode %q (no stub fallback)", skills.RetrievalSemantic)
		}
	default:
		return nil, fmt.Errorf("skills/postgres: unknown retrieval mode %q (expected \"\" or %q)", cfg.Retrieval, skills.RetrievalSemantic)
	}

	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	// Probe the connection eagerly. A misconfigured DSN should fail
	// loudly at boot, not on the first Upsert.
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("skills/postgres: ping: %w", err)
	}

	if err := applyMigrations(pingCtx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &driver{
		db:        db,
		bus:       deps.Bus,
		retrieval: cfg.Retrieval,
		embedder:  deps.Embedder,
	}, nil
}

func init() {
	skills.Register(driverName, New)
}

// driver is the Postgres-backed SkillStore. Safe for concurrent use by
// N goroutines.
type driver struct {
	db  *sql.DB
	bus events.EventBus
	// retrieval / embedder configure the opt-in semantic Search path.
	// Read-only after construction (the concurrent-reuse contract); a
	// nil embedder is only legal under the default retrieval mode.
	retrieval skills.RetrievalMode
	embedder  skills.Embedder

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
	// otherwise compute the canonical value. This lets the importer +
	// generator stamp their own hash early without the driver
	// recomputing.
	if skill.ContentHash == "" {
		skill.ContentHash = skills.CanonicalContentHash(skill)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("skills/postgres: begin tx: %w", err)
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
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND scope = $4 AND name = $5`,
		id.TenantID, id.UserID, id.SessionID, string(skill.Scope), skill.Name,
	).Scan(&existingOrigin, &existingHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// New row — fall through to insert.
	case err != nil:
		return fmt.Errorf("skills/postgres: probe existing: %w", err)
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
				return fmt.Errorf("skills/postgres: commit idempotent probe: %w", err)
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
            (tenant_id, user_id, session_id, scope, name, title, description, trigger_text,
             task_type, tags_json, tags_text, steps_json, preconditions_json,
             failure_modes_json, required_tools_json, required_ns_json,
             required_tags_json, origin, origin_ref, scope_tenant, scope_project,
             content_hash, created_at, updated_at, last_used, use_count, extra_json)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
                $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)
        ON CONFLICT (tenant_id, user_id, session_id, scope, name) DO UPDATE SET
            title               = excluded.title,
            description         = excluded.description,
            trigger_text        = excluded.trigger_text,
            task_type           = excluded.task_type,
            tags_json           = excluded.tags_json,
            tags_text           = excluded.tags_text,
            steps_json          = excluded.steps_json,
            preconditions_json  = excluded.preconditions_json,
            failure_modes_json  = excluded.failure_modes_json,
            required_tools_json = excluded.required_tools_json,
            required_ns_json    = excluded.required_ns_json,
            required_tags_json  = excluded.required_tags_json,
            origin              = excluded.origin,
            origin_ref          = excluded.origin_ref,
            scope_tenant        = excluded.scope_tenant,
            scope_project       = excluded.scope_project,
            content_hash        = excluded.content_hash,
            updated_at          = excluded.updated_at,
            extra_json          = excluded.extra_json`,
		id.TenantID, id.UserID, id.SessionID, string(skill.Scope), skill.Name,
		skill.Title, skill.Description, skill.Trigger, skill.TaskType,
		tagsJSON, tagsText, stepsJSON, preJSON, failJSON,
		rtJSON, rnsJSON, rtgJSON,
		string(skill.Origin), skill.OriginRef,
		skill.ScopeTenantID, skill.ScopeProjectID,
		skill.ContentHash, skill.CreatedAt, skill.UpdatedAt, skill.LastUsed,
		skill.UseCount, extraJSON,
	); err != nil {
		return fmt.Errorf("skills/postgres: upsert exec: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("skills/postgres: commit upsert: %w", err)
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
		return fmt.Errorf("skills/postgres: emit skill.upserted: %w", err)
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
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND name = $4
        LIMIT 1`,
		id.TenantID, id.UserID, id.SessionID, name)
	got, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return skills.Skill{}, fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
		}
		return skills.Skill{}, fmt.Errorf("skills/postgres: Get scan: %w", err)
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
	// 3 identity args are always present; optional scope / task_type /
	// per-tag filters + limit + offset grow the slice past this.
	args := make([]any, 0, 5+len(filter.Tags))
	sb.WriteString(selectSkillsSQL)
	sb.WriteString(` WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3`)
	args = append(args, id.TenantID, id.UserID, id.SessionID)
	n := 3
	if filter.Scope != "" {
		n++
		fmt.Fprintf(&sb, ` AND scope = $%d`, n)
		args = append(args, string(filter.Scope))
	}
	if filter.TaskType != "" {
		n++
		fmt.Fprintf(&sb, ` AND task_type = $%d`, n)
		args = append(args, filter.TaskType)
	}
	// Tag any-of filter: JSON contains via LIKE on tags_text. The
	// corpus is small; a proper index can land later if hot.
	for _, tag := range filter.Tags {
		n++
		fmt.Fprintf(&sb, ` AND tags_text LIKE $%d`, n)
		args = append(args, "%"+tag+"%")
	}
	fmt.Fprintf(&sb, ` ORDER BY updated_at DESC, name ASC LIMIT $%d OFFSET $%d`, n+1, n+2)
	args = append(args, limit, filter.Offset)

	rows, err := d.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("skills/postgres: List query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]skills.Skill, 0, limit)
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("skills/postgres: List scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/postgres: List iterate: %w", err)
	}
	return out, nil
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
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND name = $4`,
		id.TenantID, id.UserID, id.SessionID, name)
	if err != nil {
		return fmt.Errorf("skills/postgres: Delete exec: %w", err)
	}
	nRows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("skills/postgres: Delete rowcount: %w", err)
	}
	if nRows == 0 {
		return fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
	}
	if err := d.bus.Publish(ctx, events.Event{
		Type:       skills.EventTypeSkillDeleted,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload:    skills.SkillDeletedPayload{Name: name},
	}); err != nil {
		return fmt.Errorf("skills/postgres: emit skill.deleted: %w", err)
	}
	return nil
}

// Close implements skills.SkillStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	// CompareAndSwap is the once-only guard: the goroutine that flips
	// closed false->true owns the db.Close(); every other concurrent
	// caller loses the swap and returns nil. Close is idempotent.
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("skills/postgres: close: %w", err)
	}
	return nil
}

// marshalStrings serializes a string slice to JSON for storage.
// nil/empty round-trip as `[]`. Byte-identical to the SQLite driver so
// cross-driver round-trips are stable.
func marshalStrings(in []string) (string, error) {
	if in == nil {
		return "[]", nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("skills/postgres: marshal strings: %w", err)
	}
	return string(b), nil
}

func marshalExtra(extra map[string]any) (string, error) {
	if len(extra) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("skills/postgres: marshal extra: %w", err)
	}
	return string(b), nil
}

// unmarshalStrings reverses marshalStrings. Empty/null JSON returns a
// nil slice.
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
// compose it and pick their FROM.
const skillCols = `name, title, description, trigger_text, task_type,
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
	// Postgres returns TIMESTAMPTZ as location-aware time.Time; the
	// SQLite driver round-trips UTC. Normalise to UTC so cross-driver
	// comparisons and the List ordering are backend-independent.
	s.CreatedAt = s.CreatedAt.UTC()
	s.UpdatedAt = s.UpdatedAt.UTC()
	s.LastUsed = s.LastUsed.UTC()
	return s, nil
}

package localdb

// installed_package.go — the complete installed-package contract on the
// SQLite driver (Phase 243 / D-422, the mandatory store seam).
//
// Five mandatory methods (D-422; every SkillStore implements all of
// them, no `Supports*` ceremony):
//
//   - GetInstalledPackage / ResolveSupport — the atomic read surface of
//     the durable installed unit;
//   - PutInstalledPackage — the conditional compare-and-swap write with
//     explicit replace + origin precedence + idempotent exact replay;
//   - DeleteInstalledPackage / RestoreInstalledPackage — exact-receipt
//     conditional compensation that NEVER touches another proposal's
//     winner.
//
// Storage model. The atomic unit spans three tables, all mutated as ONE
// transaction per package:
//
//   - `installed_packages` — the unit envelope: session-zeroed
//     (tenant, user, effective-agent, name) key, winner origin, versioned
//     PackageHash, package Version, the canonical package serialization
//     (identity-bearing manifest, no materialized bytes), and the
//     canonical stored skill as JSON;
//   - `installed_support` — every support file of the installed unit:
//     canonical path, MIME, exact size, digest, bounded immutable BLOB
//     bytes (the FK pins the rows to their envelope);
//   - `skills` — the legacy membership row at the session-zeroed
//     ScopeUser rung with the effective agent bound, written through the
//     SAME single writer as the legacy surface so GetScopeAgent / List /
//     FTS reads reflect the installed unit atomically.
//
// A reader never sees the body without every support byte and never sees
// a partial replacement: the driver pins the pool to one connection and
// every operation on the unit runs inside a transaction, so a writer's
// commit is the only point where the unit becomes visible.
//
// Concurrency: one instance is safe for N concurrent goroutines (the
// concurrent-reuse contract). All values are deep-copied at the boundary
// — mutating a returned unit or a caller-supplied unit never mutates
// store state (storage is JSON/BLOB serialized, so reconstruction is a
// fresh copy by construction).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// querier is the minimal surface both *sql.DB and *sql.Tx satisfy, so
// the read helpers below serve autocommit and transactional callers
// alike.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// installedSkillWire is the stable JSON storage form of the canonical
// stored skill inside `installed_packages.skill_json`. It is written and
// read only by this driver, and is versioned implicitly by the envelope
// (a future envelope change bumps the storage migration, not this form).
type installedSkillWire struct {
	Name           string         `json:"name"`
	AgentID        string         `json:"agent_id"`
	Title          string         `json:"title"`
	Description    string         `json:"description"`
	Trigger        string         `json:"trigger"`
	TaskType       string         `json:"task_type"`
	Tags           []string       `json:"tags,omitempty"`
	Steps          []string       `json:"steps"`
	Preconditions  []string       `json:"preconditions,omitempty"`
	FailureModes   []string       `json:"failure_modes,omitempty"`
	RequiredTools  []string       `json:"required_tools,omitempty"`
	RequiredNS     []string       `json:"required_ns,omitempty"`
	RequiredTags   []string       `json:"required_tags,omitempty"`
	Origin         string         `json:"origin"`
	OriginRef      string         `json:"origin_ref"`
	Scope          string         `json:"scope"`
	ScopeTenantID  string         `json:"scope_tenant"`
	ScopeProjectID string         `json:"scope_project"`
	ContentHash    string         `json:"content_hash"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	LastUsed       time.Time      `json:"last_used"`
	UseCount       int            `json:"use_count"`
	Extra          map[string]any `json:"extra,omitempty"`
}

// marshalInstalledSkill serializes the canonical stored skill to the
// stable JSON storage form.
func marshalInstalledSkill(s skills.Skill) (string, error) {
	b, err := json.Marshal(installedSkillWire{
		Name:           s.Name,
		AgentID:        s.AgentID,
		Title:          s.Title,
		Description:    s.Description,
		Trigger:        s.Trigger,
		TaskType:       s.TaskType,
		Tags:           s.Tags,
		Steps:          s.Steps,
		Preconditions:  s.Preconditions,
		FailureModes:   s.FailureModes,
		RequiredTools:  s.RequiredTools,
		RequiredNS:     s.RequiredNS,
		RequiredTags:   s.RequiredTags,
		Origin:         string(s.Origin),
		OriginRef:      s.OriginRef,
		Scope:          string(s.Scope),
		ScopeTenantID:  s.ScopeTenantID,
		ScopeProjectID: s.ScopeProjectID,
		ContentHash:    s.ContentHash,
		CreatedAt:      s.CreatedAt,
		UpdatedAt:      s.UpdatedAt,
		LastUsed:       s.LastUsed,
		UseCount:       s.UseCount,
		Extra:          s.Extra,
	})
	if err != nil {
		return "", fmt.Errorf("skills/localdb: marshal installed skill: %w", err)
	}
	return string(b), nil
}

// unmarshalInstalledSkill reverses marshalInstalledSkill. The returned
// skill is a fresh deep copy (fresh slices / maps by construction).
func unmarshalInstalledSkill(s string) (skills.Skill, error) {
	var w installedSkillWire
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return skills.Skill{}, fmt.Errorf("skills/localdb: unmarshal installed skill: %w", err)
	}
	return skills.Skill{
		Name:           w.Name,
		AgentID:        w.AgentID,
		Title:          w.Title,
		Description:    w.Description,
		Trigger:        w.Trigger,
		TaskType:       w.TaskType,
		Tags:           w.Tags,
		Steps:          w.Steps,
		Preconditions:  w.Preconditions,
		FailureModes:   w.FailureModes,
		RequiredTools:  w.RequiredTools,
		RequiredNS:     w.RequiredNS,
		RequiredTags:   w.RequiredTags,
		Origin:         skills.Origin(w.Origin),
		OriginRef:      w.OriginRef,
		Scope:          skills.Scope(w.Scope),
		ScopeTenantID:  w.ScopeTenantID,
		ScopeProjectID: w.ScopeProjectID,
		ContentHash:    w.ContentHash,
		CreatedAt:      w.CreatedAt,
		UpdatedAt:      w.UpdatedAt,
		LastUsed:       w.LastUsed,
		UseCount:       w.UseCount,
		Extra:          w.Extra,
	}, nil
}

// installedRow is the identity-bearing envelope of the installed unit as
// stored in `installed_packages`.
type installedRow struct {
	origin         string
	packageHash    string
	packageVersion string
	packageJSON    string
	skillJSON      string
}

// readInstalledRow loads the installed-unit envelope at the session-zeroed
// (tenant, user, effective-agent, name) key. Absent → wrapped
// `ErrInstalledPackageNotFound`; the caller may not learn anything about
// a key it cannot see (closed typed error, no names or hashes leaked
// across the authority boundary).
func readInstalledRow(ctx context.Context, q querier, id identity.Quadruple, agentID, name string) (*installedRow, error) {
	var row installedRow
	err := q.QueryRowContext(ctx, `
        SELECT origin, package_hash, package_version, package_json, skill_json
        FROM installed_packages
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&row.origin, &row.packageHash, &row.packageVersion, &row.packageJSON, &row.skillJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: name=%q agent_id=%q", skills.ErrInstalledPackageNotFound, name, agentID)
	}
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: read installed package: %w", err)
	}
	return &row, nil
}

// installedUnitFromRow reconstructs the atomic unit from its envelope and
// ordered support rows: the canonical package form carries the manifest;
// the support rows carry the bounded immutable bytes, attached in
// canonical path order. A support row missing for a manifest entry is a
// torn stored unit and fails loudly (it cannot happen through this
// driver's one-transaction writes).
func installedUnitFromRow(row *installedRow, supports []skills.SupportFile) (skills.InstalledPackage, error) {
	pkg, err := skills.PackageFromCanonicalBytes([]byte(row.packageJSON))
	if err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/localdb: decode stored package: %w", err)
	}
	byPath := make(map[string]skills.SupportFile, len(supports))
	for _, f := range supports {
		byPath[f.Path] = f
	}
	for i := range pkg.Supports {
		f, ok := byPath[pkg.Supports[i].Path]
		if !ok {
			return skills.InstalledPackage{}, fmt.Errorf(
				"skills/localdb: stored package %q support %q has no bytes (torn unit)",
				row.packageHash, pkg.Supports[i].Path)
		}
		pkg.Supports[i].Data = f.Data
	}
	skill, err := unmarshalInstalledSkill(row.skillJSON)
	if err != nil {
		return skills.InstalledPackage{}, err
	}
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: row.packageHash}, nil
}

// writeInstalledUnit commits the atomic unit (envelope + support rows +
// legacy membership row) inside the caller's transaction. The support
// manifest of the new winner fully replaces the old winner's rows; the
// membership row is upserted through the same single writer as the legacy
// surface. created_at is preserved across replaces; updated_at moves.
func writeInstalledUnit(ctx context.Context, tx *sql.Tx, id identity.Quadruple, agentID string, pkg skills.InstalledPackage) error {
	name := pkg.Package.Name
	now := time.Now().UTC()

	if err := upsertSkillRow(ctx, tx, id, pkg.Skill); err != nil {
		return fmt.Errorf("skills/localdb: write installed membership row: %w", err)
	}

	canonical, err := skills.CanonicalPackageBytes(pkg.Package)
	if err != nil {
		return fmt.Errorf("skills/localdb: canonicalize installed package: %w", err)
	}
	skillJSON, err := marshalInstalledSkill(pkg.Skill)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO installed_packages
            (tenant, user, agent_id, name, origin, package_hash, package_version,
             package_json, skill_json, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(tenant, user, agent_id, name) DO UPDATE SET
            origin          = excluded.origin,
            package_hash    = excluded.package_hash,
            package_version = excluded.package_version,
            package_json    = excluded.package_json,
            skill_json      = excluded.skill_json,
            updated_at      = excluded.updated_at`,
		id.TenantID, id.UserID, agentID, name,
		string(pkg.Skill.Origin), pkg.PackageHash, pkg.Package.Version,
		string(canonical), skillJSON, now, now,
	); err != nil {
		return fmt.Errorf("skills/localdb: upsert installed package: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
        DELETE FROM installed_support
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name); err != nil {
		return fmt.Errorf("skills/localdb: clear installed support: %w", err)
	}
	for _, f := range pkg.Package.Supports {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO installed_support (tenant, user, agent_id, name, path, mime, size, digest, data)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id.TenantID, id.UserID, agentID, name, f.Path, f.Mime, f.Size, f.Digest, f.Data); err != nil {
			return fmt.Errorf("skills/localdb: insert installed support %q: %w", f.Path, err)
		}
	}
	return nil
}

// installedPackageExists reports whether an installed package is present
// at the session-zeroed (tenant, user, effective-agent, name) key. It is
// the shared fail-loud fence probe: every legacy mutation whose target
// key could collide with an installed unit checks it BEFORE any state is
// touched.
func (d *driver) installedPackageExists(ctx context.Context, q querier, id identity.Quadruple, agentID, name string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `
        SELECT count(*) FROM installed_packages
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name).Scan(&n); err != nil {
		return false, fmt.Errorf("skills/localdb: probe installed package: %w", err)
	}
	return n > 0, nil
}

// errInstalledPackageReadOnly builds the typed fail-loud refusal shared
// by every legacy mutation path that targets an installed-package key.
func errInstalledPackageReadOnly(name, agentID string, scope skills.Scope) error {
	return fmt.Errorf("%w: name=%q agent_id=%q scope=%q (legacy mutation refused; the installed unit is read-only from the legacy surface)",
		skills.ErrInstalledPackageReadOnly, name, agentID, scope)
}

// GetInstalledPackage implements skills.SkillStore.
func (d *driver) GetInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string) (skills.InstalledPackage, error) {
	if d.closed.Load() {
		return skills.InstalledPackage{}, skills.ErrStoreClosed
	}
	if err := skills.ValidateIdentity(id); err != nil {
		return skills.InstalledPackage{}, skills.EmitIdentityRejected(ctx, d.bus, id, "GetInstalledPackage")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/localdb: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort; the surfaced error is the original one

	row, err := readInstalledRow(ctx, tx, id, agentID, name)
	if err != nil {
		return skills.InstalledPackage{}, err
	}
	supports, err := readInstalledSupport(ctx, tx, id, agentID, name)
	if err != nil {
		return skills.InstalledPackage{}, err
	}
	unit, err := installedUnitFromRow(row, supports)
	if err != nil {
		return skills.InstalledPackage{}, err
	}
	if err := tx.Commit(); err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/localdb: commit installed package read: %w", err)
	}
	return unit, nil
}

// readInstalledSupport loads the ordered support manifest (canonical path
// order) of the installed unit. The scan produces fresh byte slices per
// call — the returned entries are deep copies.
func readInstalledSupport(ctx context.Context, q querier, id identity.Quadruple, agentID, name string) ([]skills.SupportFile, error) {
	rows, err := q.QueryContext(ctx, `
        SELECT path, mime, size, digest, data FROM installed_support
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?
        ORDER BY path`,
		id.TenantID, id.UserID, agentID, name)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: query installed support: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]skills.SupportFile, 0, 8)
	for rows.Next() {
		var f skills.SupportFile
		if err := rows.Scan(&f.Path, &f.Mime, &f.Size, &f.Digest, &f.Data); err != nil {
			return nil, fmt.Errorf("skills/localdb: scan installed support: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: iterate installed support: %w", err)
	}
	return out, nil
}

// ResolveSupport implements skills.SkillStore: the exact immutable
// `skillpkg://<PackageHash>/<encoded-canonical-path>` reference is the
// only address. A foreign-hash or dangling-path URI is refused with
// `ErrSupportNotFound` (never resolved against a different package,
// never guessed); an absent package names `ErrInstalledPackageNotFound`.
func (d *driver) ResolveSupport(ctx context.Context, id identity.Quadruple, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	if d.closed.Load() {
		return skills.SupportFile{}, skills.ErrStoreClosed
	}
	if err := skills.ValidateIdentity(id); err != nil {
		return skills.SupportFile{}, skills.EmitIdentityRejected(ctx, d.bus, id, "ResolveSupport")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/localdb: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is best-effort; the surfaced error is the original one

	row, err := readInstalledRow(ctx, tx, id, agentID, name)
	if err != nil {
		return skills.SupportFile{}, err
	}
	if uri.Hash != row.packageHash {
		return skills.SupportFile{}, fmt.Errorf("%w: uri hash %q is foreign to installed package %q",
			skills.ErrSupportNotFound, uri.Hash, row.packageHash)
	}
	var f skills.SupportFile
	err = tx.QueryRowContext(ctx, `
        SELECT path, mime, size, digest, data FROM installed_support
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ? AND path = ?`,
		id.TenantID, id.UserID, agentID, name, uri.Path,
	).Scan(&f.Path, &f.Mime, &f.Size, &f.Digest, &f.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return skills.SupportFile{}, fmt.Errorf("%w: %q is not in the installed package manifest",
			skills.ErrSupportNotFound, uri.Path)
	}
	if err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/localdb: resolve support: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/localdb: commit support read: %w", err)
	}
	return f, nil
}

// PutInstalledPackage implements skills.SkillStore: the conditional
// compare-and-swap write of the atomic unit. One transaction prevents any
// partial visibility; the condition, explicit-replace requirement, and
// origin-precedence gate are all evaluated inside that transaction, and a
// refused put leaves the exact prior winner untouched.
func (d *driver) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	if d.closed.Load() {
		return skills.InstalledPackageReceipt{}, skills.ErrStoreClosed
	}
	if err := skills.ValidateIdentity(id); err != nil {
		return skills.InstalledPackageReceipt{}, skills.EmitIdentityRejected(ctx, d.bus, id, "PutInstalledPackage")
	}
	if err := skills.ValidateInstalledPackageCondition(cond); err != nil {
		return skills.InstalledPackageReceipt{}, err
	}
	if strings.TrimSpace(agentID) == "" {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: agentID (effective agent) must be non-empty", skills.ErrInstalledPackageInvalid)
	}
	if err := skills.ValidateInstalledPackage(pkg); err != nil {
		return skills.InstalledPackageReceipt{}, err
	}
	if pkg.Skill.AgentID != agentID {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: Skill.AgentID %q != effective agent %q",
			skills.ErrInstalledPackageInvalid, pkg.Skill.AgentID, agentID)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/localdb: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original one
		}
	}()

	name := pkg.Package.Name
	var (
		winnerHash, winnerVersion, winnerOrigin string
		present                                 bool
	)
	err = tx.QueryRowContext(ctx, `
        SELECT origin, package_hash, package_version FROM installed_packages
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&winnerOrigin, &winnerHash, &winnerVersion)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		present = false
	case err != nil:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/localdb: probe installed package: %w", err)
	default:
		present = true
	}

	priorHash, priorVersion := "", ""
	if present {
		priorHash, priorVersion = winnerHash, winnerVersion
	}

	// Idempotent exact replay: the winner is already the incoming
	// package. A response-loss retry converges on the same terminal
	// state, and the receipt names the installed version as written.
	if present && winnerHash == pkg.PackageHash {
		if err := tx.Commit(); err != nil {
			return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/localdb: commit replay: %w", err)
		}
		committed = true
		return skills.InstalledPackageReceipt{
			TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: name,
			WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
			PriorHash: "", PriorVersion: "",
		}, nil
	}

	switch {
	case !present:
		if !cond.ExpectedAbsent {
			return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected prior hash %q but the key is absent",
				skills.ErrInstalledPackageConditionFailed, cond.ExpectedHash)
		}
	case cond.ExpectedAbsent:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageExists, name)
	case winnerHash != cond.ExpectedHash:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected hash %q, winner has %q",
			skills.ErrInstalledPackageConditionFailed, cond.ExpectedHash, winnerHash)
	case cond.ExpectedVersion != "" && winnerVersion != cond.ExpectedVersion:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected version %q, winner has %q",
			skills.ErrInstalledPackageConditionFailed, cond.ExpectedVersion, winnerVersion)
	case !replace:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageReplaceRequired, name)
	case winnerOrigin == string(skills.OriginPack) && pkg.Skill.Origin != skills.OriginPack:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q existing_origin=pack incoming=%s",
			skills.ErrPackOverwriteRefused, name, pkg.Skill.Origin)
	}

	if err := writeInstalledUnit(ctx, tx, id, agentID, pkg); err != nil {
		return skills.InstalledPackageReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/localdb: commit installed package: %w", err)
	}
	committed = true
	return skills.InstalledPackageReceipt{
		TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: name,
		WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
		PriorHash: priorHash, PriorVersion: priorVersion,
	}, nil
}

// DeleteInstalledPackage implements skills.SkillStore: exact-receipt
// conditional erasure. The receipt's written version must still be the
// winner; a different winner or an absent key is a normal concurrent
// outcome ((false, nil)) — a receipt NEVER deletes another proposal's
// winner. Erasure removes the whole atomic unit in one transaction:
// envelope, support rows, and the legacy membership row.
func (d *driver) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	if d.closed.Load() {
		return false, skills.ErrStoreClosed
	}
	if err := skills.ValidateIdentity(id); err != nil {
		return false, skills.EmitIdentityRejected(ctx, d.bus, id, "DeleteInstalledPackage")
	}
	if err := skills.ValidateInstalledPackageReceipt(receipt, id, agentID, name); err != nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("skills/localdb: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original one
		}
	}()

	winnerHash, present, err := readInstalledHash(ctx, tx, id, agentID, name)
	if err != nil {
		return false, err
	}
	if !present || winnerHash != receipt.WrittenHash {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("skills/localdb: commit compensation read: %w", err)
		}
		committed = true
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `
        DELETE FROM installed_support
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name); err != nil {
		return false, fmt.Errorf("skills/localdb: erase installed support: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM installed_packages
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name); err != nil {
		return false, fmt.Errorf("skills/localdb: erase installed package: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM skills
        WHERE tenant = ? AND user = ? AND session = ? AND scope = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, skills.UserScopeStorageSession, string(skills.ScopeUser), agentID, name); err != nil {
		return false, fmt.Errorf("skills/localdb: erase installed membership row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("skills/localdb: commit installed package erasure: %w", err)
	}
	committed = true
	return true, nil
}

// RestoreInstalledPackage implements skills.SkillStore: exact-receipt
// conditional restore. `prior` (the exact package the receipt's write
// displaced) is restored ONLY when the receipt's written version is still
// the winner — a receipt NEVER replaces another proposal's winner, and an
// absent key is a no-op. The restore does not re-apply the origin-
// precedence gate: it can only ever replace the version the receipt
// itself wrote. `prior` must validate and match the receipt's recorded
// prior hash; both fail loudly.
func (d *driver) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	if d.closed.Load() {
		return false, skills.ErrStoreClosed
	}
	if err := skills.ValidateIdentity(id); err != nil {
		return false, skills.EmitIdentityRejected(ctx, d.bus, id, "RestoreInstalledPackage")
	}
	if err := skills.ValidateInstalledPackageReceipt(receipt, id, agentID, name); err != nil {
		return false, err
	}
	if err := skills.ValidateInstalledPackage(prior); err != nil {
		return false, err
	}
	if prior.Skill.AgentID != agentID {
		return false, fmt.Errorf("%w: prior Skill.AgentID %q != effective agent %q",
			skills.ErrInstalledPackageInvalid, prior.Skill.AgentID, agentID)
	}
	if receipt.PriorHash == "" {
		return false, fmt.Errorf("%w: the receipt records an absent prior; compensate with DeleteInstalledPackage",
			skills.ErrInstalledPackageInvalid)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("skills/localdb: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original one
		}
	}()

	winnerHash, present, err := readInstalledHash(ctx, tx, id, agentID, name)
	if err != nil {
		return false, err
	}
	if !present || winnerHash != receipt.WrittenHash {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("skills/localdb: commit compensation read: %w", err)
		}
		committed = true
		return false, nil
	}
	if prior.PackageHash != receipt.PriorHash {
		return false, fmt.Errorf("%w: prior hash %q does not match the receipt's recorded prior %q",
			skills.ErrInstalledPackageConditionFailed, prior.PackageHash, receipt.PriorHash)
	}

	if err := writeInstalledUnit(ctx, tx, id, agentID, prior); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("skills/localdb: commit installed package restore: %w", err)
	}
	committed = true
	return true, nil
}

// readInstalledHash loads only the winner's PackageHash at the target
// key, reporting absence distinctly from a read failure. Used by the two
// exact-receipt compensation primitives.
func readInstalledHash(ctx context.Context, q querier, id identity.Quadruple, agentID, name string) (hash string, present bool, err error) {
	err = q.QueryRowContext(ctx, `
        SELECT package_hash FROM installed_packages
        WHERE tenant = ? AND user = ? AND agent_id = ? AND name = ?`,
		id.TenantID, id.UserID, agentID, name).Scan(&hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("skills/localdb: read installed package hash: %w", err)
	default:
		return hash, true, nil
	}
}

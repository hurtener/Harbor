package postgres

// installed_package.go — the complete installed-package surface of the
// Postgres SkillStore driver.
//
// The installed unit is the atomic durable form of a complete skill
// package: the canonical stored semantic skill PLUS the versioned
// PackageHash PLUS the ordered normalized support manifest with
// bounded immutable support bytes. It is keyed at the session-zeroed
// (tenant, user, effective-agent, name) target on the ScopeUser rung
// and is self-contained: reads never dereference the source/staging
// artifacts the package was validated from.
//
// Atomicity and concurrency:
//
//   - Every mutation (Put / Delete / Restore and the legacy-mutation
//     fence probes) runs in ONE transaction and takes a per-key
//     Postgres advisory xact lock derived from the exact
//     (tenant, user, agent, name) target. Writers on the same key
//     serialize; the conditional probe therefore always observes the
//     latest committed winner, so a compare-and-swap never races
//     another writer into a torn state.
//   - Every read (Get / Resolve) runs in ONE transaction at
//     REPEATABLE READ isolation: both SELECTs share one snapshot, so
//     a reader never observes the skill body without every support
//     byte and never observes a half-replaced unit (a package row
//     from one version with support bytes from another).
//   - The package row, every support row, and the `skills` membership
//     row land or disappear together in the same transaction.
//
// The legacy mutation surface (Upsert / Delete / DeleteAgent) is
// fenced off the installed key: when a legacy target would land on an
// installed package's membership row, the driver refuses with
// `ErrInstalledPackageReadOnly` before any state is touched, so the
// unit can never be torn (row deleted, package left) or silently
// overwritten (row replaced, package stale). The dedicated package-
// aware methods below are the only mutation path for an installed
// key.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// storedSupportRow is one stored support-manifest entry as read back
// from `installed_package_supports`.
type storedSupportRow struct {
	path   string
	mime   string
	size   int64
	digest string
	data   []byte
}

// packageLockKey derives the Postgres advisory-lock key for the exact
// package target (tenant, user, effective-agent, name). FNV-64a over a
// NUL-separated key is collision-safe for LOCKING purposes (a rare
// collision only serialises two unrelated keys — never incorrect, and
// all writers take the lock in the same order so no deadlock is
// possible). The bit reinterpretation matches the migration runner's
// advisory-lock convention.
func packageLockKey(tenantID, userID, agentID, name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenantID + "\x00" + userID + "\x00" + agentID + "\x00" + name))
	//nolint:gosec // intentional bit reinterpretation; pg_advisory_xact_lock takes bigint
	return int64(h.Sum64())
}

// lockPackageKey takes the per-key advisory xact lock inside `tx`. The
// lock is transaction-scoped and auto-released on commit/rollback. It
// is the concurrency-safe serialization point for every mutation that
// can touch the (tenant, user, agent, name) target: the package-aware
// methods AND the legacy-mutation fence probes that must not interleave
// with a concurrent install.
func lockPackageKey(ctx context.Context, tx *sql.Tx, tenantID, userID, agentID, name string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
		packageLockKey(tenantID, userID, agentID, name)); err != nil {
		return fmt.Errorf("skills/postgres: package advisory lock: %w", err)
	}
	return nil
}

// installedPackageColumns are the columns `GetInstalledPackage`
// consumes from `installed_packages`.
const installedPackageColumns = `package_hash, skill_json, canonical`

// GetInstalledPackage implements skills.SkillStore. It returns the
// atomic installed unit at the session-zeroed (tenant, user,
// effective-agent, name) key. Missing → ErrInstalledPackageNotFound.
// The returned value is a deep copy (fully decoded from storage).
func (d *driver) GetInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string) (skills.InstalledPackage, error) {
	if d.closed.Load() {
		return skills.InstalledPackage{}, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.InstalledPackage{}, skills.EmitIdentityRejected(ctx, d.bus, id, "GetInstalledPackage")
	}

	// REPEATABLE READ: both SELECTs share one snapshot so the package
	// row and its support bytes always come from the same committed
	// state — a reader never sees a partial unit.
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: begin read tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // read-only; the surfaced error is the original

	var hash, skillJSON, canonical string
	err = tx.QueryRowContext(ctx, `SELECT `+installedPackageColumns+`
        FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&hash, &skillJSON, &canonical)
	if errors.Is(err, sql.ErrNoRows) {
		return skills.InstalledPackage{}, fmt.Errorf("%w: name=%q agent_id=%q", skills.ErrInstalledPackageNotFound, name, agentID)
	}
	if err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: probe installed package: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
        SELECT path, mime, size, digest, data
        FROM installed_package_supports
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name)
	if err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: read support manifest: %w", err)
	}
	var supportRows []storedSupportRow
	for rows.Next() {
		var r storedSupportRow
		if err := rows.Scan(&r.path, &r.mime, &r.size, &r.digest, &r.data); err != nil {
			_ = rows.Close()
			return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: scan support row: %w", err)
		}
		supportRows = append(supportRows, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: iterate support manifest: %w", err)
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: commit read tx: %w", err)
	}

	return reassembleInstalledUnit(hash, skillJSON, canonical, supportRows)
}

// reassembleInstalledUnit reconstructs the atomic unit from its stored
// parts: the canonical skill JSON, the canonical package serialization
// (manifest identity, no bytes), and the ordered support rows. The
// manifest order and identity come from the canonical bytes; the
// bounded immutable bytes are attached by exact path. Any inconsistency
// (a manifest entry without bytes, a size/digest lie, a hash that does
// not match the canonical bytes) fails loudly — storage corruption
// never surfaces as silently-wrong bytes.
func reassembleInstalledUnit(hash, skillJSON, canonical string, supportRows []storedSupportRow) (skills.InstalledPackage, error) {
	skill, err := unmarshalInstalledSkill(skillJSON)
	if err != nil {
		return skills.InstalledPackage{}, err
	}
	pkg, err := skills.PackageFromCanonicalBytes([]byte(canonical))
	if err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: decode canonical package: %w", err)
	}
	byPath := make(map[string][]byte, len(supportRows))
	for _, r := range supportRows {
		byPath[r.path] = r.data
	}
	for i := range pkg.Supports {
		f := &pkg.Supports[i]
		data, ok := byPath[f.Path]
		if !ok {
			return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: installed package %q manifest entry %q has no stored bytes (torn storage)", pkg.Name, f.Path)
		}
		if int64(len(data)) != f.Size {
			return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: installed package %q support %q size %d != stored %d (corrupt storage)", pkg.Name, f.Path, f.Size, len(data))
		}
		if want := hexDigestBytes(data); want != f.Digest {
			return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: installed package %q support %q digest mismatch (corrupt storage)", pkg.Name, f.Path)
		}
		f.Data = data
	}
	unit := skills.InstalledPackage{
		Skill:       skill,
		Package:     pkg,
		PackageHash: hash,
	}
	// Re-validate the closed shape so a corrupt row fails loudly at
	// the read boundary (the stored unit must be self-consistent: hash
	// truth, canonical ContentHash, non-empty manifest with bytes).
	if err := skills.ValidateInstalledPackage(unit); err != nil {
		return skills.InstalledPackage{}, fmt.Errorf("skills/postgres: stored installed package failed re-validation: %w", err)
	}
	return unit, nil
}

// ResolveSupport implements skills.SkillStore. It resolves ONE support
// file of the installed package by its exact immutable
// `skillpkg://<PackageHash>/<encoded-canonical-path>` reference. The
// URI's hash MUST equal the installed package's PackageHash and its
// canonical path MUST name a manifest entry; a foreign-hash or
// dangling-path URI fails with ErrSupportNotFound (never resolved
// against a different package, never guessed). The returned entry
// carries the bounded immutable support bytes; the value is a deep
// copy.
func (d *driver) ResolveSupport(ctx context.Context, id identity.Quadruple, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	if d.closed.Load() {
		return skills.SupportFile{}, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.SupportFile{}, skills.EmitIdentityRejected(ctx, d.bus, id, "ResolveSupport")
	}

	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/postgres: begin resolve tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // read-only; the surfaced error is the original

	var winnerHash string
	err = tx.QueryRowContext(ctx, `SELECT package_hash FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&winnerHash)
	if errors.Is(err, sql.ErrNoRows) {
		return skills.SupportFile{}, fmt.Errorf("%w: name=%q agent_id=%q", skills.ErrInstalledPackageNotFound, name, agentID)
	}
	if err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/postgres: probe installed package: %w", err)
	}
	// The URI's hash must be the exact installed package's hash — a
	// foreign-hash reference is refused, never guessed against another
	// package.
	if uri.Hash != winnerHash {
		return skills.SupportFile{}, fmt.Errorf("%w: uri hash %q is foreign to installed package %q", skills.ErrSupportNotFound, uri.Hash, winnerHash)
	}

	var f skills.SupportFile
	err = tx.QueryRowContext(ctx, `
        SELECT path, mime, size, digest, data
        FROM installed_package_supports
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4 AND path = $5`,
		id.TenantID, id.UserID, agentID, name, uri.Path,
	).Scan(&f.Path, &f.Mime, &f.Size, &f.Digest, &f.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return skills.SupportFile{}, fmt.Errorf("%w: %q is not in the installed package manifest", skills.ErrSupportNotFound, uri.Path)
	}
	if err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/postgres: resolve support row: %w", err)
	}
	// The bytes must satisfy the stored digest — storage corruption
	// never surfaces as silently-wrong bytes.
	if want := hexDigestBytes(f.Data); want != f.Digest {
		return skills.SupportFile{}, fmt.Errorf("skills/postgres: resolved support %q digest mismatch (corrupt storage)", f.Path)
	}
	if err := tx.Commit(); err != nil {
		return skills.SupportFile{}, fmt.Errorf("skills/postgres: commit resolve tx: %w", err)
	}
	return f, nil
}

// PutInstalledPackage implements skills.SkillStore. It conditionally
// installs or replaces the atomic package at the session-zeroed
// (tenant, user, effective-agent, name) key. The write is a
// compare-and-swap over the exact prior absence/PackageHash/version,
// requires explicit replace, applies origin precedence (generated
// never overwrites a pack winner), and treats an exact same-hash
// replay as an idempotent no-op. A successful write returns the exact
// receipt sufficient for conditional compensation.
func (d *driver) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	if d.closed.Load() {
		return skills.InstalledPackageReceipt{}, skills.ErrStoreClosed
	}
	if skills.ValidateIdentity(id) != nil {
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
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: Skill.AgentID %q != effective agent %q", skills.ErrInstalledPackageInvalid, pkg.Skill.AgentID, agentID)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/postgres: begin put tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the original error is the surfaced one
		}
	}()

	if err := lockPackageKey(ctx, tx, id.TenantID, id.UserID, agentID, pkg.Package.Name); err != nil {
		return skills.InstalledPackageReceipt{}, err
	}

	// Probe the current winner under the key lock: the observed state
	// is the latest committed winner, so the CAS below never races.
	var curHash, curVersion, curOrigin string
	err = tx.QueryRowContext(ctx, `SELECT package_hash, package_version, origin FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, pkg.Package.Name,
	).Scan(&curHash, &curVersion, &curOrigin)
	present := true
	if errors.Is(err, sql.ErrNoRows) {
		present = false
	} else if err != nil {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/postgres: probe installed package: %w", err)
	}

	priorHash, priorVersion := "", ""
	if present {
		priorHash, priorVersion = curHash, curVersion
	}

	// Idempotent exact replay precedes the condition check: a
	// response-loss retry of the EXACT same package converges on the
	// same terminal state even when the original condition is now
	// stale. The receipt names the installed version as written.
	if present && curHash == pkg.PackageHash {
		if err := tx.Commit(); err != nil {
			return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/postgres: commit replay: %w", err)
		}
		committed = true
		return skills.InstalledPackageReceipt{
			TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: pkg.Package.Name,
			WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
			PriorHash: "", PriorVersion: "",
		}, nil
	}

	switch {
	case !present:
		if !cond.ExpectedAbsent {
			return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected prior hash %q but the key is absent", skills.ErrInstalledPackageConditionFailed, cond.ExpectedHash)
		}
	case cond.ExpectedAbsent:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageExists, pkg.Package.Name)
	case curHash != cond.ExpectedHash:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected hash %q, winner has %q", skills.ErrInstalledPackageConditionFailed, cond.ExpectedHash, curHash)
	case cond.ExpectedVersion != "" && curVersion != cond.ExpectedVersion:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected version %q, winner has %q", skills.ErrInstalledPackageConditionFailed, cond.ExpectedVersion, curVersion)
	case !replace:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageReplaceRequired, pkg.Package.Name)
	case curOrigin == string(skills.OriginPack) && pkg.Skill.Origin != skills.OriginPack:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q existing_origin=pack incoming=%s",
			skills.ErrPackOverwriteRefused, pkg.Package.Name, pkg.Skill.Origin)
	}

	// ONE transaction per package: the package row, every support row,
	// and the `skills` membership row land together. A reader never
	// observes the body without every support byte.
	if err := d.writeInstalledUnit(ctx, tx, id, agentID, pkg); err != nil {
		return skills.InstalledPackageReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return skills.InstalledPackageReceipt{}, fmt.Errorf("skills/postgres: commit put: %w", err)
	}
	committed = true
	return skills.InstalledPackageReceipt{
		TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: pkg.Package.Name,
		WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
		PriorHash: priorHash, PriorVersion: priorVersion,
	}, nil
}

// writeInstalledUnit writes the atomic unit inside `tx`: the
// installed_packages row, the full support manifest (DELETE + INSERT
// so a replacement never leaves orphaned entries), and the session-
// zeroed ScopeUser agent-bound `skills` membership row. Callers hold
// the package key lock and have validated the conditional write.
func (d *driver) writeInstalledUnit(ctx context.Context, tx *sql.Tx, id identity.Quadruple, agentID string, pkg skills.InstalledPackage) error {
	skillJSON, err := marshalInstalledSkill(pkg.Skill)
	if err != nil {
		return err
	}
	canonical, err := skills.CanonicalPackageBytes(pkg.Package)
	if err != nil {
		return fmt.Errorf("skills/postgres: canonical package: %w", err)
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO installed_packages
            (tenant_id, user_id, agent_id, name, package_hash, package_version, origin,
             skill_json, canonical, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
        ON CONFLICT (tenant_id, user_id, agent_id, name) DO UPDATE SET
            package_hash    = excluded.package_hash,
            package_version = excluded.package_version,
            origin          = excluded.origin,
            skill_json      = excluded.skill_json,
            canonical       = excluded.canonical,
            updated_at      = excluded.updated_at`,
		id.TenantID, id.UserID, agentID, pkg.Package.Name,
		pkg.PackageHash, pkg.Package.Version, string(pkg.Skill.Origin),
		skillJSON, string(canonical), now, now,
	); err != nil {
		return fmt.Errorf("skills/postgres: write installed package: %w", err)
	}

	// Replace the support manifest wholesale: a replacement never
	// leaves an entry of a displaced version behind.
	if _, err := tx.ExecContext(ctx, `
        DELETE FROM installed_package_supports
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, pkg.Package.Name,
	); err != nil {
		return fmt.Errorf("skills/postgres: clear support manifest: %w", err)
	}
	for _, f := range pkg.Package.Supports {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO installed_package_supports
                (tenant_id, user_id, agent_id, name, path, mime, size, digest, data)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id.TenantID, id.UserID, agentID, pkg.Package.Name,
			f.Path, f.Mime, f.Size, f.Digest, f.Data,
		); err != nil {
			return fmt.Errorf("skills/postgres: write support %q: %w", f.Path, err)
		}
	}

	// The membership row: an ordinary session-zeroed ScopeUser
	// agent-bound `skills` row, so the legacy read surface reflects the
	// installed unit. It is written in the SAME transaction as the
	// package — never one without the other.
	return d.writeSkillRow(ctx, tx, id.TenantID, id.UserID, skills.UserScopeStorageSession, pkg.Skill)
}

// DeleteInstalledPackage implements skills.SkillStore. It is the exact
// conditional-delete compensation primitive: it deletes the atomic
// installed package ONLY when the receipt's written version is still
// the winner. Returns (true, nil) when the receipt's version was
// current and has been deleted; (false, nil) when the winner is a
// DIFFERENT version or the key is absent — a receipt NEVER deletes
// another proposal's winner.
func (d *driver) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	if d.closed.Load() {
		return false, skills.ErrStoreClosed
	}
	if err := skills.ValidateInstalledPackageReceipt(receipt, id, agentID, name); err != nil {
		return false, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("skills/postgres: begin delete tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the original error is the surfaced one
		}
	}()

	if err := lockPackageKey(ctx, tx, id.TenantID, id.UserID, agentID, name); err != nil {
		return false, err
	}

	var winnerHash string
	err = tx.QueryRowContext(ctx, `SELECT package_hash FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&winnerHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Already erased — a normal concurrent outcome, never an error.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("skills/postgres: probe installed package: %w", err)
	case winnerHash != receipt.WrittenHash:
		// A different proposal's winner — never deleted.
		return false, nil
	}

	// Erase the whole atomic unit: package row, support bytes, and the
	// membership row — one transaction, never a partial tear.
	if _, err := tx.ExecContext(ctx, `DELETE FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	); err != nil {
		return false, fmt.Errorf("skills/postgres: delete installed package: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM installed_package_supports
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	); err != nil {
		return false, fmt.Errorf("skills/postgres: delete support manifest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skills
        WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND scope = $4 AND agent_id = $5 AND name = $6`,
		id.TenantID, id.UserID, skills.UserScopeStorageSession, string(skills.ScopeUser), agentID, name,
	); err != nil {
		return false, fmt.Errorf("skills/postgres: delete membership row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("skills/postgres: commit delete: %w", err)
	}
	committed = true
	return true, nil
}

// RestoreInstalledPackage implements skills.SkillStore. It is the
// exact conditional-restore compensation primitive: it restores `prior`
// over the current winner ONLY when the receipt's written version is
// still the winner. Returns (true, nil) when the receipt's version was
// current and has been replaced by `prior`; (false, nil) when the
// winner is a DIFFERENT version or the key is absent. The restore is
// exact-receipt compensation: it does NOT re-apply the origin-
// precedence gate, because it can only ever replace the version the
// receipt itself wrote.
func (d *driver) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	if d.closed.Load() {
		return false, skills.ErrStoreClosed
	}
	if err := skills.ValidateInstalledPackageReceipt(receipt, id, agentID, name); err != nil {
		return false, err
	}
	if err := skills.ValidateInstalledPackage(prior); err != nil {
		return false, err
	}
	if prior.Skill.AgentID != agentID {
		return false, fmt.Errorf("%w: prior Skill.AgentID %q != effective agent %q", skills.ErrInstalledPackageInvalid, prior.Skill.AgentID, agentID)
	}
	if receipt.PriorHash == "" {
		return false, fmt.Errorf("%w: the receipt records an absent prior; compensate with DeleteInstalledPackage", skills.ErrInstalledPackageInvalid)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("skills/postgres: begin restore tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // best-effort; the original error is the surfaced one
		}
	}()

	if err := lockPackageKey(ctx, tx, id.TenantID, id.UserID, agentID, name); err != nil {
		return false, err
	}

	var winnerHash string
	err = tx.QueryRowContext(ctx, `SELECT package_hash FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&winnerHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The receipt's write is no longer the winner (erased key) —
		// never replaced.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("skills/postgres: probe installed package: %w", err)
	case winnerHash != receipt.WrittenHash:
		// A different proposal's winner — never replaced.
		return false, nil
	}

	// The prior must be the EXACT package the receipt displaced;
	// a wrong prior fails loudly and mutates nothing.
	if prior.PackageHash != receipt.PriorHash {
		return false, fmt.Errorf("%w: prior hash %q does not match the receipt's recorded prior %q", skills.ErrInstalledPackageConditionFailed, prior.PackageHash, receipt.PriorHash)
	}

	if err := d.writeInstalledUnit(ctx, tx, id, agentID, prior); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("skills/postgres: commit restore: %w", err)
	}
	committed = true
	return true, nil
}

// fenceLegacyMutation implements the fail-loud legacy-mutation fence:
// when a legacy mutation target (Upsert / Delete / DeleteAgent) derived
// from `(scope, agentID, name)` would land on an installed package's
// membership row, it is refused with `ErrInstalledPackageReadOnly`
// BEFORE any row or package state is touched. The check runs under the
// same per-key advisory xact lock the package-aware mutations use, so
// a legacy mutation can never interleave with a concurrent install and
// silently overwrite (or idempotently echo) the installed key. Keys
// without an installed package return nil — the exact legacy behavior
// is preserved.
func (d *driver) fenceLegacyMutation(ctx context.Context, tx *sql.Tx, id identity.Quadruple, scope skills.Scope, agentID, name string) error {
	if !skills.LegacyMutationTargetsInstalledKey(scope, agentID, name) {
		return nil
	}
	if err := lockPackageKey(ctx, tx, id.TenantID, id.UserID, agentID, name); err != nil {
		return err
	}
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM installed_packages
        WHERE tenant_id = $1 AND user_id = $2 AND agent_id = $3 AND name = $4`,
		id.TenantID, id.UserID, agentID, name,
	).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("skills/postgres: fence probe: %w", err)
	default:
		return fmt.Errorf("%w: name=%q agent_id=%q scope=%q (legacy mutation refused; the installed unit is read-only from the legacy surface)",
			skills.ErrInstalledPackageReadOnly, name, agentID, scope)
	}
}

// marshalInstalledSkill serializes the canonical stored semantic skill
// to its driver JSON form (the `installed_packages.skill_json`
// column). The store holds no reference to caller memory.
func marshalInstalledSkill(s skills.Skill) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("skills/postgres: marshal installed skill: %w", err)
	}
	return string(b), nil
}

// unmarshalInstalledSkill reverses marshalInstalledSkill and
// normalizes timestamps to UTC exactly like the legacy `scanSkill`
// path, so cross-driver comparisons stay backend-independent.
func unmarshalInstalledSkill(s string) (skills.Skill, error) {
	var out skills.Skill
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return skills.Skill{}, fmt.Errorf("skills/postgres: unmarshal installed skill: %w", err)
	}
	out.CreatedAt = out.CreatedAt.UTC()
	out.UpdatedAt = out.UpdatedAt.UTC()
	out.LastUsed = out.LastUsed.UTC()
	return out, nil
}

// hexDigestBytes returns the lowercase hex sha256 of b — the digest
// form the support manifest records.
func hexDigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

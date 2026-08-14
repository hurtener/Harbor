package conformancetest

// reference_store_test.go — the compliant in-memory reference
// implementation of the full `skills.SkillStore` surface, used by the
// harness self-tests to prove the installed-package suite passes a
// driver that honors the contract and that every suite scenario is
// sound. It is test-only (`_test.go`) and deliberately mirrors the
// exact contract semantics the real drivers must implement:
//
//   - the atomic installed unit (semantic skill row + PackageHash +
//     ordered manifest with bounded immutable support bytes) is stored
//     and read as ONE unit under a mutex — a reader never sees the body
//     without every support byte, and a failed conditional put leaves
//     no partial state;
//   - the target key is exactly (tenant, user, session-zeroed,
//     effective-agent, name) on the ScopeUser rung;
//   - conditional put/replace compares the exact prior
//     absence/PackageHash/version, requires explicit replace, applies
//     origin precedence, and treats an exact same-hash replay as a
//     no-op success;
//   - receipts are sufficient for exact conditional compensation:
//     Delete/Restore bind to WrittenHash and never touch another
//     proposal's winner;
//   - the legacy mutation surface is fenced off the installed key:
//     Upsert / Delete / DeleteAgent refuse with
//     ErrInstalledPackageReadOnly (before any state is touched) when
//     their target would collide with an installed package's
//     membership row, so the unit can never be torn or silently
//     overwritten by a legacy path;
//   - every value is deep-copied at the boundary (concurrent-reuse
//     contract).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// referenceStore is a mutex-guarded in-memory SkillStore. It holds no
// per-call state on itself beyond the maps (both guarded), so one
// instance is safe for N concurrent goroutines under `-race`.
type referenceStore struct {
	mu     sync.Mutex
	closed bool
	rows   map[string]skills.Skill            // legacy skill rows
	pkgs   map[string]skills.InstalledPackage // installed atomic units
}

func newReferenceStore() *referenceStore {
	return &referenceStore{
		rows: map[string]skills.Skill{},
		pkgs: map[string]skills.InstalledPackage{},
	}
}

func refRowKey(q identity.Quadruple, session, scope string, agentID, name string) string {
	return q.TenantID + "|" + q.UserID + "|" + session + "|" + scope + "|" + agentID + "|" + name
}

func refPkgKey(q identity.Quadruple, agentID, name string) string {
	return q.TenantID + "|" + q.UserID + "|" + agentID + "|" + name
}

func refDeepCopySkill(s skills.Skill) skills.Skill {
	out := s
	out.Tags = append([]string(nil), s.Tags...)
	out.Steps = append([]string(nil), s.Steps...)
	out.Preconditions = append([]string(nil), s.Preconditions...)
	out.FailureModes = append([]string(nil), s.FailureModes...)
	out.RequiredTools = append([]string(nil), s.RequiredTools...)
	out.RequiredNS = append([]string(nil), s.RequiredNS...)
	out.RequiredTags = append([]string(nil), s.RequiredTags...)
	if len(s.Extra) > 0 {
		out.Extra = make(map[string]any, len(s.Extra))
		for k, v := range s.Extra {
			out.Extra[k] = v
		}
	}
	return out
}

func refDeepCopyPackage(p skills.Package) skills.Package {
	out := p
	out.Skill = skills.PackageSkill{
		Name:          p.Skill.Name,
		Title:         p.Skill.Title,
		Description:   p.Skill.Description,
		Trigger:       p.Skill.Trigger,
		TaskType:      p.Skill.TaskType,
		Tags:          append([]string(nil), p.Skill.Tags...),
		Steps:         append([]string(nil), p.Skill.Steps...),
		Preconditions: append([]string(nil), p.Skill.Preconditions...),
		FailureModes:  append([]string(nil), p.Skill.FailureModes...),
		RequiredTools: append([]string(nil), p.Skill.RequiredTools...),
		RequiredNS:    append([]string(nil), p.Skill.RequiredNS...),
		RequiredTags:  append([]string(nil), p.Skill.RequiredTags...),
	}
	if len(p.Supports) > 0 {
		out.Supports = make([]skills.SupportFile, len(p.Supports))
		for i, f := range p.Supports {
			out.Supports[i] = f
			if f.Data != nil {
				out.Supports[i].Data = append([]byte(nil), f.Data...)
			}
		}
	}
	return out
}

func refDeepCopyUnit(u skills.InstalledPackage) skills.InstalledPackage {
	out := u
	out.Skill = refDeepCopySkill(u.Skill)
	out.Package = refDeepCopyPackage(u.Package)
	return out
}

func (s *referenceStore) errIfClosed(ctx context.Context) error {
	if s.closed {
		return skills.ErrStoreClosed
	}
	return refCheckCtx(ctx)
}

// refCheckCtx honors ctx cancellation exactly as the real drivers do:
// a canceled ctx fails the operation with context.Canceled (the suite
// pins this for both the legacy and the installed-package surfaces).
func refCheckCtx(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// errIfInstalledKeyFenced returns the typed fail-loud refusal when a
// legacy mutation path (Upsert / Delete / DeleteAgent) whose target row
// key is derived from `(scope, agentID, name)` would collide with an
// installed package's membership row at the same (tenant, user,
// effective-agent, name) key. The check runs UNDER s.mu, so the refusal
// and any subsequent mutation are atomic: the installed unit can never
// be torn (row deleted, package left) or silently overwritten (row
// replaced, package stale) by a legacy path. Keys with no installed
// package return nil — the exact legacy behavior is preserved.
func (s *referenceStore) errIfInstalledKeyFenced(id identity.Quadruple, scope skills.Scope, agentID, name string) error {
	if !skills.LegacyMutationTargetsInstalledKey(scope, agentID, name) {
		return nil
	}
	if _, ok := s.pkgs[refPkgKey(id, agentID, name)]; !ok {
		return nil
	}
	return fmt.Errorf("%w: name=%q agent_id=%q scope=%q (legacy mutation refused; the installed unit is read-only from the legacy surface)",
		skills.ErrInstalledPackageReadOnly, name, agentID, scope)
}

// ---- Legacy surface (implemented so the reference store satisfies the
// full SkillStore interface; the installed-package suite relies on the
// Upsert / Get / GetScopeAgent / Delete / DeleteSessionScope behavior
// below). ----

func (s *referenceStore) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	if err := s.errIfClosed(ctx); err != nil {
		return err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.ErrIdentityRequired
	}
	if err := skill.Validate(); err != nil {
		return err
	}
	if skill.ContentHash == "" {
		skill.ContentHash = skills.CanonicalContentHash(skill)
	}
	session := skills.StorageSessionID(id, skill.Scope)
	key := refRowKey(id, session, string(skill.Scope), skill.AgentID, skill.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Legacy-mutation fence: the installed unit's membership row is
	// read-only from the legacy surface. Refuse BEFORE the probe so a
	// legacy upsert can never silently overwrite (or idempotently
	// echo) an installed key.
	if err := s.errIfInstalledKeyFenced(id, skill.Scope, skill.AgentID, skill.Name); err != nil {
		return err
	}
	if existing, ok := s.rows[key]; ok {
		if existing.Origin == skills.OriginPack && skill.Origin != skills.OriginPack {
			return skills.ErrPackOverwriteRefused
		}
		if existing.ContentHash == skill.ContentHash {
			return nil // idempotent
		}
	}
	now := time.Now().UTC()
	if skill.CreatedAt.IsZero() {
		skill.CreatedAt = now
	}
	skill.UpdatedAt = now
	s.rows[key] = refDeepCopySkill(skill)
	return nil
}

func (s *referenceStore) Get(ctx context.Context, id identity.Quadruple, name string) (skills.Skill, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return skills.Skill{}, err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.Skill{}, skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read precedence mirrors the drivers: the caller's own
	// session-pinned rows (any non-user scope) union the durable
	// user-scope rows of the same (tenant, user); a session-pinned row
	// wins a same-name collision.
	var userRow *skills.Skill
	for k, row := range s.rows {
		parts := strings.Split(k, "|")
		if len(parts) != 6 || parts[0] != id.TenantID || parts[1] != id.UserID ||
			parts[5] != name || parts[4] != "" {
			continue
		}
		switch {
		case parts[3] == string(skills.ScopeUser):
			cp := refDeepCopySkill(row)
			userRow = &cp
		case parts[2] == id.SessionID:
			return refDeepCopySkill(row), nil
		}
	}
	if userRow != nil {
		return *userRow, nil
	}
	return skills.Skill{}, fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
}

func (s *referenceStore) GetScope(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) (skills.Skill, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return skills.Skill{}, err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.Skill{}, skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := refRowKey(id, skills.StorageSessionID(id, scope), string(scope), "", name)
	if row, ok := s.rows[key]; ok {
		return refDeepCopySkill(row), nil
	}
	return skills.Skill{}, fmt.Errorf("%w: name=%q scope=%q", skills.ErrSkillNotFound, name, scope)
}

func (s *referenceStore) GetScopeAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) (skills.Skill, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return skills.Skill{}, err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.Skill{}, skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := skills.StorageSessionID(id, scope)
	bound := refRowKey(id, session, string(scope), agentID, name)
	if row, ok := s.rows[bound]; ok {
		return refDeepCopySkill(row), nil
	}
	legacy := refRowKey(id, session, string(scope), "", name)
	if row, ok := s.rows[legacy]; ok {
		return refDeepCopySkill(row), nil
	}
	return skills.Skill{}, fmt.Errorf("%w: name=%q scope=%q agent_id=%q", skills.ErrSkillNotFound, name, scope, agentID)
}

func (s *referenceStore) List(ctx context.Context, id identity.Quadruple, filter skills.ListFilter) ([]skills.Skill, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return nil, err
	}
	if skills.ValidateIdentity(id) != nil {
		return nil, skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []skills.Skill
	for k, row := range s.rows {
		parts := strings.Split(k, "|")
		if len(parts) != 6 || parts[0] != id.TenantID || parts[1] != id.UserID {
			continue
		}
		if parts[2] != id.SessionID && parts[2] != skills.UserScopeStorageSession {
			continue
		}
		if filter.Scope != "" && string(row.Scope) != string(filter.Scope) {
			continue
		}
		if filter.AgentID != "" && row.AgentID != filter.AgentID {
			continue
		}
		if filter.TaskType != "" && row.TaskType != filter.TaskType {
			continue
		}
		out = append(out, refDeepCopySkill(row))
	}
	return out, nil
}

func (s *referenceStore) Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]skills.RankedSkill, error) {
	rows, err := s.List(ctx, id, skills.ListFilter{})
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var out []skills.RankedSkill
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Name+" "+row.Title+" "+row.Trigger+" "+strings.Join(row.Tags, " ")), q) {
			out = append(out, skills.RankedSkill{Skill: row, Score: 1, Path: skills.PathFTS5})
		}
	}
	return out, nil
}

func (s *referenceStore) SearchAgent(ctx context.Context, id identity.Quadruple, agentID, query string, limit int) ([]skills.RankedSkill, error) {
	rows, err := s.List(ctx, id, skills.ListFilter{AgentID: agentID})
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var out []skills.RankedSkill
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Name+" "+row.Trigger), q) {
			out = append(out, skills.RankedSkill{Skill: row, Score: 1, Path: skills.PathFTS5})
		}
	}
	return out, nil
}

func (s *referenceStore) SearchSnapshot(ctx context.Context, id identity.Quadruple, query string, candidates []skills.Skill, limit int) ([]skills.RankedSkill, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return nil, err
	}
	if skills.ValidateIdentity(id) != nil {
		return nil, skills.ErrIdentityRequired
	}
	q := strings.ToLower(query)
	var out []skills.RankedSkill
	for _, c := range candidates {
		if strings.Contains(strings.ToLower(c.Name+" "+c.Title+" "+c.Description), q) {
			out = append(out, skills.RankedSkill{Skill: c, Score: 1, Path: skills.PathFTS5})
		}
	}
	return out, nil
}

func (s *referenceStore) Delete(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) error {
	if err := s.errIfClosed(ctx); err != nil {
		return err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Legacy-mutation fence (defensive on the agent-less Delete: the
	// installed membership row always carries a non-empty effective
	// agent, so this never fires here — it pins that Delete shares the
	// fence contract with DeleteAgent for every legacy mutation path).
	if err := s.errIfInstalledKeyFenced(id, scope, "", name); err != nil {
		return err
	}
	session := skills.StorageSessionID(id, scope)
	key := refRowKey(id, session, string(scope), "", name)
	if _, ok := s.rows[key]; !ok {
		return fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
	}
	delete(s.rows, key)
	return nil
}

func (s *referenceStore) DeleteAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) error {
	if err := s.errIfClosed(ctx); err != nil {
		return err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Legacy-mutation fence: DeleteAgent at the ScopeUser rung with the
	// installed package's effective agent + name targets the unit's
	// membership row. Refuse BEFORE the row probe so the atomic unit is
	// never torn (row deleted, package left) — the dedicated
	// DeleteInstalledPackage / RestoreInstalledPackage methods are the
	// only package-aware mutation path.
	if err := s.errIfInstalledKeyFenced(id, scope, agentID, name); err != nil {
		return err
	}
	key := refRowKey(id, skills.StorageSessionID(id, scope), string(scope), agentID, name)
	if _, ok := s.rows[key]; !ok {
		return fmt.Errorf("%w: name=%q", skills.ErrSkillNotFound, name)
	}
	delete(s.rows, key)
	return nil
}

func (s *referenceStore) DeleteSessionScope(ctx context.Context, id identity.Quadruple) error {
	if err := s.errIfClosed(ctx); err != nil {
		return err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.rows {
		parts := strings.Split(k, "|")
		if len(parts) == 6 && parts[0] == id.TenantID && parts[1] == id.UserID &&
			parts[2] == id.SessionID && parts[3] == string(skills.ScopeSession) {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *referenceStore) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// ---- Complete installed-package contract (the surface the suite
// pins; the semantics below ARE the contract). ----

// currentWinner returns the installed winner at the session-zeroed
// target key, or nil. Callers hold s.mu.
func (s *referenceStore) currentWinner(id identity.Quadruple, agentID, name string) *skills.InstalledPackage {
	u, ok := s.pkgs[refPkgKey(id, agentID, name)]
	if !ok {
		return nil
	}
	cp := refDeepCopyUnit(u)
	return &cp
}

func (s *referenceStore) GetInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string) (skills.InstalledPackage, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return skills.InstalledPackage{}, err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.InstalledPackage{}, skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if winner := s.currentWinner(id, agentID, name); winner != nil {
		return *winner, nil
	}
	return skills.InstalledPackage{}, fmt.Errorf("%w: name=%q agent_id=%q", skills.ErrInstalledPackageNotFound, name, agentID)
}

func (s *referenceStore) ResolveSupport(ctx context.Context, id identity.Quadruple, agentID, name string, uri skills.PackageURI) (skills.SupportFile, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return skills.SupportFile{}, err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.SupportFile{}, skills.ErrIdentityRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	winner := s.currentWinner(id, agentID, name)
	if winner == nil {
		return skills.SupportFile{}, fmt.Errorf("%w: name=%q agent_id=%q", skills.ErrInstalledPackageNotFound, name, agentID)
	}
	// A malformed URI, a foreign hash, or a dangling path never
	// resolves — the exact immutable reference is the only address.
	if uri.Hash != winner.PackageHash {
		return skills.SupportFile{}, fmt.Errorf("%w: uri hash %q is foreign to installed package %q", skills.ErrSupportNotFound, uri.Hash, winner.PackageHash)
	}
	for _, f := range winner.Package.Supports {
		if f.Path == uri.Path {
			out := f
			out.Data = append([]byte(nil), f.Data...)
			return out, nil
		}
	}
	return skills.SupportFile{}, fmt.Errorf("%w: %q is not in the installed package manifest", skills.ErrSupportNotFound, uri.Path)
}

func (s *referenceStore) PutInstalledPackage(ctx context.Context, id identity.Quadruple, agentID string, pkg skills.InstalledPackage, cond skills.InstalledPackageCondition, replace bool) (skills.InstalledPackageReceipt, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return skills.InstalledPackageReceipt{}, err
	}
	if skills.ValidateIdentity(id) != nil {
		return skills.InstalledPackageReceipt{}, skills.ErrIdentityRequired
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

	s.mu.Lock()
	defer s.mu.Unlock()

	winner, present := s.pkgs[refPkgKey(id, agentID, pkg.Package.Name)]
	priorHash, priorVersion := "", ""
	if present {
		priorHash, priorVersion = winner.PackageHash, winner.Package.Version
	}

	if present && winner.PackageHash == pkg.PackageHash {
		// Idempotent exact replay: the winner is already the incoming
		// package. No mutation; the receipt names the installed version.
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
	case winner.PackageHash != cond.ExpectedHash:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected hash %q, winner has %q", skills.ErrInstalledPackageConditionFailed, cond.ExpectedHash, winner.PackageHash)
	case cond.ExpectedVersion != "" && winner.Package.Version != cond.ExpectedVersion:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: expected version %q, winner has %q", skills.ErrInstalledPackageConditionFailed, cond.ExpectedVersion, winner.Package.Version)
	case !replace:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q", skills.ErrInstalledPackageReplaceRequired, pkg.Package.Name)
	case winner.Skill.Origin == skills.OriginPack && pkg.Skill.Origin != skills.OriginPack:
		return skills.InstalledPackageReceipt{}, fmt.Errorf("%w: name=%q existing_origin=pack incoming=%s",
			skills.ErrPackOverwriteRefused, pkg.Package.Name, pkg.Skill.Origin)
	}

	// ONE transaction per package: the skill row and the installed unit
	// land together; a reader never observes one without the other.
	key := refPkgKey(id, agentID, pkg.Package.Name)
	s.pkgs[key] = refDeepCopyUnit(pkg)
	rowKey := refRowKey(id, skills.UserScopeStorageSession, string(skills.ScopeUser), agentID, pkg.Package.Name)
	s.rows[rowKey] = refDeepCopySkill(pkg.Skill)

	return skills.InstalledPackageReceipt{
		TenantID: id.TenantID, UserID: id.UserID, AgentID: agentID, Name: pkg.Package.Name,
		WrittenHash: pkg.PackageHash, WrittenVersion: pkg.Package.Version,
		PriorHash: priorHash, PriorVersion: priorVersion,
	}, nil
}

func (s *referenceStore) DeleteInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt) (bool, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return false, err
	}
	if err := skills.ValidateInstalledPackageReceipt(receipt, id, agentID, name); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	winner, present := s.pkgs[refPkgKey(id, agentID, name)]
	if !present || winner.PackageHash != receipt.WrittenHash {
		// A different proposal's winner or an already-erased key is a
		// normal concurrent outcome — never deleted.
		return false, nil
	}
	delete(s.pkgs, refPkgKey(id, agentID, name))
	delete(s.rows, refRowKey(id, skills.UserScopeStorageSession, string(skills.ScopeUser), agentID, name))
	return true, nil
}

func (s *referenceStore) RestoreInstalledPackage(ctx context.Context, id identity.Quadruple, agentID, name string, receipt skills.InstalledPackageReceipt, prior skills.InstalledPackage) (bool, error) {
	if err := s.errIfClosed(ctx); err != nil {
		return false, err
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
	s.mu.Lock()
	defer s.mu.Unlock()
	winner, present := s.pkgs[refPkgKey(id, agentID, name)]
	if !present || winner.PackageHash != receipt.WrittenHash {
		// The receipt's write is no longer the winner — another
		// proposal's winner or an erased key; never replaced.
		return false, nil
	}
	if prior.PackageHash != receipt.PriorHash {
		return false, fmt.Errorf("%w: prior hash %q does not match the receipt's recorded prior %q", skills.ErrInstalledPackageConditionFailed, prior.PackageHash, receipt.PriorHash)
	}
	key := refPkgKey(id, agentID, name)
	s.pkgs[key] = refDeepCopyUnit(prior)
	rowKey := refRowKey(id, skills.UserScopeStorageSession, string(skills.ScopeUser), agentID, name)
	s.rows[rowKey] = refDeepCopySkill(prior.Skill)
	return true, nil
}

// ensure the reference store satisfies the full interface at compile
// time (test-only helper; the contract is enforced for real drivers by
// the conformancetest suite).
var _ skills.SkillStore = (*referenceStore)(nil)

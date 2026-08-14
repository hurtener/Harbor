package skills_test

// package_store_contract_test.go — directly corresponding unit tests
// for the complete installed-package contract helpers declared in
// `internal/skills/skills.go` (D-422 / HA-61). The driver-facing
// behaviors (conditional put/replace, exact-receipt compensation,
// erasure, isolation, concurrent reuse) are pinned by the shared
// conformancetest suite; these tests pin the closed-shape validation
// gates every driver calls at its boundary.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// installedUnitFixture returns a self-consistent atomic unit (the same
// shape the conformancetest suite builds): a valid package with one
// JSON support file, its versioned PackageHash, and a canonical
// ScopeUser/agent-bound stored skill.
func installedUnitFixture(name, agentID string, origin skills.Origin, version string) skills.InstalledPackage {
	now := time.Now().UTC()
	data := []byte(`{"k": "v"}`)
	pkg := skills.Package{
		Name:    name,
		Version: version,
		Skill: skills.PackageSkill{
			Name:        name,
			Title:       "Title",
			Description: "Description",
			Trigger:     "trigger:" + name,
			TaskType:    "code",
			Tags:        []string{"a"},
			Steps:       []string{"step one"},
		},
		Supports: []skills.SupportFile{
			{Path: "examples/one.json", Mime: "application/json", Size: int64(len(data)), Digest: pkgHexDigest(data), Data: data},
		},
	}
	hash, err := skills.PackageHash(pkg)
	if err != nil {
		panic(err) // fixture construction bug — test-time only
	}
	skill := skills.Skill{
		Name:        name,
		AgentID:     agentID,
		Title:       pkg.Skill.Title,
		Description: pkg.Skill.Description,
		Trigger:     pkg.Skill.Trigger,
		TaskType:    pkg.Skill.TaskType,
		Tags:        []string{"a"},
		Steps:       []string{"step one"},
		Origin:      origin,
		OriginRef:   "test:" + name,
		Scope:       skills.ScopeUser,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: hash}
}

func pkgHexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var pkgFixtureID = identity.Quadruple{
	Identity: identity.Identity{TenantID: "t-pkg", UserID: "u-pkg", SessionID: "s-pkg"},
	RunID:    "r-pkg",
}

func TestValidateInstalledPackageCondition(t *testing.T) {
	t.Parallel()
	valid := "v1:" + strings.Repeat("a", 64)

	cases := []struct {
		name string
		cond skills.InstalledPackageCondition
		want error
	}{
		{"absent", skills.InstalledPackageCondition{ExpectedAbsent: true}, nil},
		{"hash only", skills.InstalledPackageCondition{ExpectedHash: valid}, nil},
		{"hash and version", skills.InstalledPackageCondition{ExpectedHash: valid, ExpectedVersion: "1.0.0"}, nil},
		{"empty condition", skills.InstalledPackageCondition{}, skills.ErrInstalledPackageInvalid},
		{"absent with hash", skills.InstalledPackageCondition{ExpectedAbsent: true, ExpectedHash: valid}, skills.ErrInstalledPackageInvalid},
		{"absent with version", skills.InstalledPackageCondition{ExpectedAbsent: true, ExpectedVersion: "1.0.0"}, skills.ErrInstalledPackageInvalid},
		{"malformed hash", skills.InstalledPackageCondition{ExpectedHash: "not-a-hash"}, skills.ErrInstalledPackageInvalid},
		{"unversioned hash", skills.InstalledPackageCondition{ExpectedHash: strings.Repeat("b", 64)}, skills.ErrInstalledPackageInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := skills.ValidateInstalledPackageCondition(tc.cond)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateInstalledPackageCondition(%+v) = %v, want nil", tc.cond, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateInstalledPackageCondition(%+v) = %v, want errors.Is %v", tc.cond, err, tc.want)
			}
		})
	}
}

func TestValidateInstalledPackage_AcceptsWellFormed(t *testing.T) {
	t.Parallel()
	unit := installedUnitFixture("unit-ok", "agent-a", skills.OriginGenerated, "1.0.0")
	if err := skills.ValidateInstalledPackage(unit); err != nil {
		t.Fatalf("ValidateInstalledPackage(well-formed) = %v, want nil", err)
	}
}

func TestValidateInstalledPackage_RejectsEachClosedShapeViolation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(*skills.InstalledPackage)
		want   error
		wantIn string
	}{
		{"bad stored skill", func(u *skills.InstalledPackage) { u.Skill.Steps = nil }, skills.ErrInstalledPackageInvalid, "Steps empty"},
		{"bad package", func(u *skills.InstalledPackage) { u.Package.Skill.Trigger = "" }, skills.ErrInstalledPackageInvalid, "Trigger empty"},
		{"non-user scope", func(u *skills.InstalledPackage) { u.Skill.Scope = skills.ScopeSession }, skills.ErrInstalledPackageInvalid, "forces ScopeUser"},
		{"name mismatch", func(u *skills.InstalledPackage) { u.Skill.Name = "other" }, skills.ErrInstalledPackageInvalid, "single identity"},
		{"lying package hash", func(u *skills.InstalledPackage) {
			u.PackageHash = "v1:" + strings.Repeat("0", 64)
		}, skills.ErrInstalledPackageInvalid, "hash"},
		{"lying content hash", func(u *skills.InstalledPackage) { u.Skill.ContentHash = strings.Repeat("f", 64) }, skills.ErrInstalledPackageInvalid, "canonical"},
		{"missing support bytes", func(u *skills.InstalledPackage) { u.Package.Supports[0].Data = nil }, skills.ErrInstalledPackageInvalid, "no bytes"},
		{"bad origin", func(u *skills.InstalledPackage) { u.Skill.Origin = skills.Origin("made-up") }, skills.ErrInstalledPackageInvalid, "Origin="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := installedUnitFixture("unit-bad", "agent-a", skills.OriginGenerated, "1.0.0")
			tc.mutate(&bad)
			err := skills.ValidateInstalledPackage(bad)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateInstalledPackage = %v, want errors.Is %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("ValidateInstalledPackage error = %q, want substring %q", err.Error(), tc.wantIn)
			}
		})
	}
}

func TestValidateInstalledPackageReceipt(t *testing.T) {
	t.Parallel()
	unit := installedUnitFixture("receipt", "agent-a", skills.OriginGenerated, "1.0.0")
	receipt := skills.InstalledPackageReceipt{
		TenantID: pkgFixtureID.TenantID, UserID: pkgFixtureID.UserID,
		AgentID: "agent-a", Name: "receipt",
		WrittenHash: unit.PackageHash, WrittenVersion: unit.Package.Version,
	}

	if err := skills.ValidateInstalledPackageReceipt(receipt, pkgFixtureID, "agent-a", "receipt"); err != nil {
		t.Fatalf("ValidateInstalledPackageReceipt(well-formed) = %v, want nil", err)
	}

	// Missing identity fails closed (wrapped ErrIdentityRequired).
	badID := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}}
	if err := skills.ValidateInstalledPackageReceipt(receipt, badID, "agent-a", "receipt"); !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("missing-identity receipt = %v, want errors.Is ErrIdentityRequired", err)
	}

	// A receipt never applies to a foreign key (wrapped
	// ErrInstalledPackageInvalid) — cross-tenant, cross-user,
	// cross-agent, and cross-name variants all fail.
	foreign := []struct {
		name            string
		id              identity.Quadruple
		agentID, nameID string
	}{
		{"tenant", identity.Quadruple{Identity: identity.Identity{TenantID: "t-other", UserID: pkgFixtureID.UserID, SessionID: pkgFixtureID.SessionID}}, "agent-a", "receipt"},
		{"user", identity.Quadruple{Identity: identity.Identity{TenantID: pkgFixtureID.TenantID, UserID: "u-other", SessionID: pkgFixtureID.SessionID}}, "agent-a", "receipt"},
		{"agent", pkgFixtureID, "agent-other", "receipt"},
		{"name", pkgFixtureID, "agent-a", "other-name"},
	}
	for _, tc := range foreign {
		t.Run("foreign-"+tc.name, func(t *testing.T) {
			t.Parallel()
			if err := skills.ValidateInstalledPackageReceipt(receipt, tc.id, tc.agentID, tc.nameID); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
				t.Fatalf("foreign-%s receipt = %v, want errors.Is ErrInstalledPackageInvalid", tc.name, err)
			}
		})
	}

	// A malformed WrittenHash is a caller bug.
	badHash := receipt
	badHash.WrittenHash = "garbage"
	if err := skills.ValidateInstalledPackageReceipt(badHash, pkgFixtureID, "agent-a", "receipt"); !errors.Is(err, skills.ErrInstalledPackageInvalid) {
		t.Fatalf("malformed WrittenHash = %v, want errors.Is ErrInstalledPackageInvalid", err)
	}
}

// TestLegacyMutationTargetsInstalledKey pins the exact shape of the
// legacy-mutation fence predicate: only the ScopeUser rung with a
// non-empty effective agent and name can collide with an installed
// package's membership row (the session-zeroed
// (tenant, user, effective-agent, name) ScopeUser row). Every other
// legacy target shape reports false, so keys without a colliding shape
// keep their exact legacy behavior.
func TestLegacyMutationTargetsInstalledKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		scope           skills.Scope
		agentID, nameID string
		want            bool
	}{
		{"user rung with agent", skills.ScopeUser, "agent-a", "skill-a", true},
		{"user rung empty agent", skills.ScopeUser, "", "skill-a", false},
		{"user rung empty name", skills.ScopeUser, "agent-a", "", false},
		{"user rung empty both", skills.ScopeUser, "", "", false},
		{"session rung", skills.ScopeSession, "agent-a", "skill-a", false},
		{"project rung", skills.ScopeProject, "agent-a", "skill-a", false},
		{"tenant rung", skills.ScopeTenant, "agent-a", "skill-a", false},
		{"global rung", skills.ScopeGlobal, "agent-a", "skill-a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := skills.LegacyMutationTargetsInstalledKey(tc.scope, tc.agentID, tc.nameID); got != tc.want {
				t.Fatalf("LegacyMutationTargetsInstalledKey(%q, %q, %q) = %v, want %v",
					tc.scope, tc.agentID, tc.nameID, got, tc.want)
			}
		})
	}
}

// TestErrInstalledPackageReadOnly_DistinctSentinel pins the fence
// sentinel as its own typed error: it is a distinct sentinel (never
// aliased to another installed-package error) and it surfaces through
// errors.Is when wrapped, so callers can branch on it exactly like the
// other installed-package sentinels.
func TestErrInstalledPackageReadOnly_DistinctSentinel(t *testing.T) {
	t.Parallel()
	other := []error{
		skills.ErrInstalledPackageNotFound,
		skills.ErrInstalledPackageExists,
		skills.ErrInstalledPackageConditionFailed,
		skills.ErrInstalledPackageReplaceRequired,
		skills.ErrInstalledPackageInvalid,
		skills.ErrPackOverwriteRefused,
		skills.ErrSkillNotFound,
	}
	for _, o := range other {
		if errors.Is(o, skills.ErrInstalledPackageReadOnly) {
			t.Fatalf("ErrInstalledPackageReadOnly aliases %v", o)
		}
	}
	wrapped := fmt.Errorf("fence: %w", skills.ErrInstalledPackageReadOnly)
	if !errors.Is(wrapped, skills.ErrInstalledPackageReadOnly) {
		t.Fatalf("errors.Is(wrapped) = false, want true (wrapped %v)", wrapped)
	}
}

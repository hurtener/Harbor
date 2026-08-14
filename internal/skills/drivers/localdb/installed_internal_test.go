package localdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/skills"
)

// TestInstalledSkillWire_RoundTripAndErrors pins the stable JSON storage
// form of the canonical stored skill: a full-fidelity round trip through
// marshal/unmarshal, and a fail-loud unmarshal for non-skill bytes (a
// corrupt stored envelope must never be silently coerced).
func TestInstalledSkillWire_RoundTripAndErrors(t *testing.T) {
	now := time.Now().UTC()
	want := skills.Skill{
		Name:           "wire",
		AgentID:        "agent-wire",
		Title:          "Title",
		Description:    "Description",
		Trigger:        "trigger:wire",
		TaskType:       "code",
		Tags:           []string{"alpha", "beta"},
		Steps:          []string{"step one", "step two"},
		Preconditions:  []string{"pre"},
		FailureModes:   []string{"fail"},
		RequiredTools:  []string{"tool-a"},
		RequiredNS:     []string{"ns-a"},
		RequiredTags:   []string{"tag-a"},
		Origin:         skills.OriginPack,
		OriginRef:      "pack@v1",
		Scope:          skills.ScopeUser,
		ScopeTenantID:  "st",
		ScopeProjectID: "sp",
		ContentHash:    skills.CanonicalContentHash(skills.Skill{Name: "wire", Trigger: "t", Steps: []string{"s"}}),
		CreatedAt:      now,
		UpdatedAt:      now.Add(1),
		LastUsed:       now.Add(2),
		UseCount:       7,
		Extra:          map[string]any{"model": "claude", "pinned": true},
	}
	enc, err := marshalInstalledSkill(want)
	if err != nil {
		t.Fatalf("marshalInstalledSkill: %v", err)
	}
	got, err := unmarshalInstalledSkill(enc)
	if err != nil {
		t.Fatalf("unmarshalInstalledSkill: %v", err)
	}
	if got.Name != want.Name || got.AgentID != want.AgentID || got.Origin != want.Origin ||
		got.Scope != want.Scope || got.ContentHash != want.ContentHash || got.UseCount != want.UseCount ||
		got.CreatedAt != want.CreatedAt || got.UpdatedAt != want.UpdatedAt || got.LastUsed != want.LastUsed ||
		got.OriginRef != want.OriginRef || got.ScopeTenantID != want.ScopeTenantID ||
		got.ScopeProjectID != want.ScopeProjectID || got.Trigger != want.Trigger ||
		got.Extra["model"] != "claude" {
		t.Fatalf("wire round trip mismatch:\n got  %+v\n want %+v", got, want)
	}
	// Mutating the decoded slices never affects the wire's source (the
	// round trip produced fresh slices by construction).
	got.Steps[0] = "MUTATED"
	got.Extra["pinned"] = false
	redecoded, err := unmarshalInstalledSkill(enc)
	if err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if redecoded.Steps[0] != "step one" || redecoded.Extra["pinned"] != true {
		t.Fatalf("decoded slices alias the encoder input: %+v", redecoded)
	}

	// Non-skill bytes fail loudly — a corrupt envelope is never coerced.
	if _, err := unmarshalInstalledSkill(`{"name": 42}`); err == nil {
		t.Fatal("unmarshalInstalledSkill of non-skill JSON returned nil error")
	}
	if _, err := unmarshalInstalledSkill(`not json at all`); err == nil {
		t.Fatal("unmarshalInstalledSkill of garbage returned nil error")
	}
}

// TestInstalledUnitFromRow_FailLoudOnCorruptEnvelope pins the read-side
// reconstruction guards: a torn unit (manifest entry without its support
// bytes) and a non-canonical stored package both fail loudly instead of
// returning a partial unit.
func TestInstalledUnitFromRow_FailLoudOnCorruptEnvelope(t *testing.T) {
	ctx := context.Background()
	d := covDriver(t)
	const agent = "agent-corrupt"
	unit := installedFixtureInternal(t, "corrupt-envelope", agent, skills.OriginGenerated, "1.0.0", 1)
	if _, err := d.PutInstalledPackage(ctx, covID, agent, unit,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	// A manifest entry whose support bytes are absent is a torn unit.
	row, err := readInstalledRow(ctx, d.db, covID, agent, "corrupt-envelope")
	if err != nil {
		t.Fatalf("readInstalledRow: %v", err)
	}
	if _, err := installedUnitFromRow(row, nil); err == nil {
		t.Fatal("installedUnitFromRow with no support rows returned nil error")
	}

	// A non-canonical stored package fails the strict decode.
	bad := *row
	bad.packageJSON = `{"name": "not-canonical"}`
	if _, err := installedUnitFromRow(&bad, nil); err == nil {
		t.Fatal("installedUnitFromRow with corrupt package_json returned nil error")
	}
}

// TestReadInstalledHash_PresentAndAbsent pins the compensation read
// helper's distinct absent/present outcomes against a live driver.
func TestReadInstalledHash_PresentAndAbsent(t *testing.T) {
	ctx := context.Background()
	d := covDriver(t)
	const agent = "agent-hash"
	unit := installedFixtureInternal(t, "hash", agent, skills.OriginGenerated, "1.0.0", 1)
	if _, err := d.PutInstalledPackage(ctx, covID, agent, unit,
		skills.InstalledPackageCondition{ExpectedAbsent: true}, false); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	hash, present, err := readInstalledHash(ctx, d.db, covID, agent, "hash")
	if err != nil || !present || hash != unit.PackageHash {
		t.Fatalf("present read: hash=%q present=%v err=%v", hash, present, err)
	}
	_, present, err = readInstalledHash(ctx, d.db, covID, agent, "no-such")
	if err != nil || present {
		t.Fatalf("absent read: present=%v err=%v", present, err)
	}
	// A canceled context fails the read loudly (errors.Is sees the wrap).
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := readInstalledHash(canceled, d.db, covID, agent, "hash"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read err=%v, want context.Canceled", err)
	}
}

// TestInstalledPackage_StorageFailureFailsLoudly pins the fail-loud
// contract against a real storage failure: with the underlying SQLite
// pool closed behind the driver's back (the I/O-failure analogue — the
// closed-sentinel tests cover the orderly Close path), every installed-
// package method must surface the wrapped storage error instead of a
// sentinel misread or a silent no-op.
func TestInstalledPackage_StorageFailureFailsLoudly(t *testing.T) {
	ctx := context.Background()
	d := covDriver(t)
	const agent = "agent-storage-fail"
	unit := installedFixtureInternal(t, "storage-fail", agent, skills.OriginGenerated, "1.0.0", 1)

	// Kill the underlying DB pool without flipping the driver's closed
	// flag: every subsequent call hits a genuine "database is closed"
	// failure at BeginTx / query time.
	if err := d.db.Close(); err != nil {
		t.Fatalf("close underlying db: %v", err)
	}

	uri, err := skills.NewPackageURI(unit.PackageHash, unit.Package.Supports[0].Path)
	if err != nil {
		t.Fatalf("NewPackageURI: %v", err)
	}
	receipt := skills.InstalledPackageReceipt{
		TenantID: covID.TenantID, UserID: covID.UserID, AgentID: agent, Name: "storage-fail",
		WrittenHash: unit.PackageHash, PriorHash: unit.PackageHash,
	}
	calls := []struct {
		name string
		err  error
	}{
		{"GetInstalledPackage", func() error {
			_, err := d.GetInstalledPackage(ctx, covID, agent, "storage-fail")
			return err
		}()},
		{"ResolveSupport", func() error {
			_, err := d.ResolveSupport(ctx, covID, agent, "storage-fail", uri)
			return err
		}()},
		{"PutInstalledPackage", func() error {
			_, err := d.PutInstalledPackage(ctx, covID, agent, unit,
				skills.InstalledPackageCondition{ExpectedAbsent: true}, false)
			return err
		}()},
		{"DeleteInstalledPackage", func() error {
			_, err := d.DeleteInstalledPackage(ctx, covID, agent, "storage-fail", receipt)
			return err
		}()},
		{"RestoreInstalledPackage", func() error {
			_, err := d.RestoreInstalledPackage(ctx, covID, agent, "storage-fail", receipt, unit)
			return err
		}()},
	}
	for _, c := range calls {
		if c.err == nil {
			t.Fatalf("%s with dead storage returned nil error (silent success)", c.name)
		}
		if errors.Is(c.err, skills.ErrInstalledPackageNotFound) ||
			errors.Is(c.err, skills.ErrStoreClosed) ||
			errors.Is(c.err, context.Canceled) {
			t.Fatalf("%s with dead storage surfaced a sentinel misread: %v", c.name, c.err)
		}
	}
}

// installedFixtureInternal builds a valid installed-package unit for the
// internal-package tests (the external fixture lives in
// installed_package_test.go).
func installedFixtureInternal(t *testing.T, name, agentID string, origin skills.Origin, version string, nFiles int) skills.InstalledPackage {
	t.Helper()
	data := []byte(fmt.Sprintf(`{"file": "internal", "name": %q}`, name))
	sum := sha256SumInternal(data)
	pkg := skills.Package{
		Name:    name,
		Version: version,
		Skill: skills.PackageSkill{
			Name: name, Title: "Title " + name, Description: "Description for " + name,
			Trigger: "trigger:" + name, TaskType: "code",
			Tags: []string{"alpha"}, Steps: []string{"step one"},
		},
	}
	for i := 0; i < nFiles; i++ {
		pkg.Supports = append(pkg.Supports, skills.SupportFile{
			Path:   fmt.Sprintf("examples/file-%02d.json", i),
			Mime:   "application/json",
			Size:   int64(len(data)),
			Digest: sum,
			Data:   data,
		})
	}
	hash, err := skills.PackageHash(pkg)
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}
	now := time.Now().UTC()
	skill := skills.Skill{
		Name: name, AgentID: agentID, Title: pkg.Skill.Title, Description: pkg.Skill.Description,
		Trigger: pkg.Skill.Trigger, TaskType: pkg.Skill.TaskType,
		Tags: append([]string(nil), pkg.Skill.Tags...), Steps: append([]string(nil), pkg.Skill.Steps...),
		Origin: origin, OriginRef: "test:" + name, Scope: skills.ScopeUser,
		CreatedAt: now, UpdatedAt: now,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: hash}
}

// sha256SumInternal mirrors the external fixture helper.
func sha256SumInternal(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

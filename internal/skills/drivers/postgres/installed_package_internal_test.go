package postgres

// Driver-internal tests for the installed-package storage helpers that
// run WITHOUT a live Postgres: the advisory-lock key derivation, the
// canonical skill JSON round trip, and the atomic-unit reassembly
// (consistency + fail-loud corruption checks). The DSN-gated
// conformance suite covers the full DB surface.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/skills"
)

// fixtureInstalledUnit builds one valid atomic unit for the pure-helper
// tests (mirrors the conformance fixture shape).
func fixtureInstalledUnit(t *testing.T, name, agentID string, nFiles int) skills.InstalledPackage {
	t.Helper()
	now := time.Now().UTC()
	supports := make([]skills.SupportFile, 0, nFiles)
	for i := range nFiles {
		data := []byte(`{"file": ` + itoa(i) + `, "name": "` + name + `"}`)
		sum := sha256.Sum256(data)
		supports = append(supports, skills.SupportFile{
			Path:   "examples/file-0" + itoa(i) + ".json",
			Mime:   "application/json",
			Size:   int64(len(data)),
			Digest: hex.EncodeToString(sum[:]),
			Data:   data,
		})
	}
	pkg := skills.Package{
		Name:    name,
		Version: "1.0.0",
		Skill: skills.PackageSkill{
			Name: name, Title: "Title " + name, Trigger: "trigger:" + name,
			TaskType: "code", Tags: []string{"alpha"}, Steps: []string{"step one"},
		},
		Supports: supports,
	}
	hash, err := skills.PackageHash(pkg)
	if err != nil {
		t.Fatalf("PackageHash: %v", err)
	}
	skill := skills.Skill{
		Name: name, AgentID: agentID, Title: pkg.Skill.Title,
		Trigger: pkg.Skill.Trigger, TaskType: pkg.Skill.TaskType,
		Tags:   append([]string(nil), pkg.Skill.Tags...),
		Steps:  append([]string(nil), pkg.Skill.Steps...),
		Origin: skills.OriginGenerated, OriginRef: "internal:" + name,
		Scope: skills.ScopeUser, CreatedAt: now, UpdatedAt: now,
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	return skills.InstalledPackage{Skill: skill, Package: pkg, PackageHash: hash}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// unitToStoredParts renders a unit into its stored parts exactly as
// writeInstalledUnit would.
func unitToStoredParts(t *testing.T, unit skills.InstalledPackage) (hash, skillJSON, canonical string, rows []storedSupportRow) {
	t.Helper()
	var err error
	skillJSON, err = marshalInstalledSkill(unit.Skill)
	if err != nil {
		t.Fatalf("marshalInstalledSkill: %v", err)
	}
	cb, err := skills.CanonicalPackageBytes(unit.Package)
	if err != nil {
		t.Fatalf("CanonicalPackageBytes: %v", err)
	}
	canonical = string(cb)
	for _, f := range unit.Package.Supports {
		rows = append(rows, storedSupportRow{
			path: f.Path, mime: f.Mime, size: f.Size, digest: f.Digest,
			data: append([]byte(nil), f.Data...),
		})
	}
	return unit.PackageHash, skillJSON, canonical, rows
}

func TestPackageLockKey_DeterministicAndKeyScoped(t *testing.T) {
	a := packageLockKey("t", "u", "agent-a", "name")
	b := packageLockKey("t", "u", "agent-a", "name")
	if a != b {
		t.Fatalf("same package key derived different locks: %d vs %d", a, b)
	}
	// The lock key must be scoped to the FULL target: changing any
	// component must change the key.
	variants := [][4]string{
		{"t2", "u", "agent-a", "name"},
		{"t", "u2", "agent-a", "name"},
		{"t", "u", "agent-b", "name"},
		{"t", "u", "agent-a", "name2"},
	}
	for _, v := range variants {
		if got := packageLockKey(v[0], v[1], v[2], v[3]); got == a {
			t.Errorf("packageLockKey(%q) collides with the base key %d", v, a)
		}
	}
}

func TestMarshalInstalledSkill_RoundTrip(t *testing.T) {
	unit := fixtureInstalledUnit(t, "roundtrip", "agent-x", 1)
	s := unit.Skill
	s.OriginRef = "gen:run:7"
	s.ScopeTenantID = "st"
	s.ScopeProjectID = "sp"
	s.Extra = map[string]any{"enabled": true}
	s.UseCount = 3
	s.LastUsed = time.Unix(100, 0).UTC()

	encoded, err := marshalInstalledSkill(s)
	if err != nil {
		t.Fatalf("marshalInstalledSkill: %v", err)
	}
	got, err := unmarshalInstalledSkill(encoded)
	if err != nil {
		t.Fatalf("unmarshalInstalledSkill: %v", err)
	}
	if got.Name != s.Name || got.AgentID != s.AgentID || got.Origin != s.Origin || got.Scope != s.Scope ||
		got.ContentHash != s.ContentHash || got.OriginRef != s.OriginRef ||
		got.ScopeTenantID != s.ScopeTenantID || got.ScopeProjectID != s.ScopeProjectID ||
		got.UseCount != s.UseCount {
		t.Fatalf("round-trip mismatch:\n  got  %+v\n  want %+v", got, s)
	}
	if len(got.Steps) != 1 || got.Steps[0] != "step one" {
		t.Fatalf("Steps not preserved: %v", got.Steps)
	}
	if got.Extra["enabled"] != true {
		t.Fatalf("Extra not preserved: %#v", got.Extra)
	}
	if !got.LastUsed.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("LastUsed not preserved: %v", got.LastUsed)
	}

	if _, err := unmarshalInstalledSkill("not-json"); err == nil {
		t.Fatal("malformed skill JSON returned nil error")
	}
}

func TestReassembleInstalledUnit_ConsistencyAndFailLoudCorruption(t *testing.T) {
	unit := fixtureInstalledUnit(t, "reassemble", "agent-x", 2)
	hash, skillJSON, canonical, rows := unitToStoredParts(t, unit)

	got, err := reassembleInstalledUnit(hash, skillJSON, canonical, rows)
	if err != nil {
		t.Fatalf("reassembleInstalledUnit: %v", err)
	}
	if err := skills.ValidateInstalledPackage(got); err != nil {
		t.Fatalf("reassembled unit not valid: %v", err)
	}
	if got.PackageHash != unit.PackageHash || len(got.Package.Supports) != 2 {
		t.Fatalf("reassembled envelope mismatch: hash=%q supports=%d", got.PackageHash, len(got.Package.Supports))
	}
	for i := range unit.Package.Supports {
		if !bytes.Equal(got.Package.Supports[i].Data, unit.Package.Supports[i].Data) {
			t.Fatalf("support[%d] bytes differ after reassembly", i)
		}
	}

	// Corruption checks fail loudly instead of returning wrong bytes.
	t.Run("missing support bytes", func(t *testing.T) {
		if _, err := reassembleInstalledUnit(hash, skillJSON, canonical, rows[:1]); err == nil {
			t.Fatal("a manifest entry without stored bytes returned nil error")
		}
	})
	t.Run("data tamper fails digest", func(t *testing.T) {
		bad := append([]storedSupportRow(nil), rows...)
		bad[1].data[0] ^= 0xff
		if _, err := reassembleInstalledUnit(hash, skillJSON, canonical, bad); err == nil {
			t.Fatal("a digest lie returned nil error")
		}
	})
	t.Run("lying package hash", func(t *testing.T) {
		lying := "v1:0000000000000000000000000000000000000000000000000000000000000000"
		if _, err := reassembleInstalledUnit(lying, skillJSON, canonical, rows); err == nil {
			t.Fatal("a hash that lies about the canonical bytes returned nil error")
		}
	})
}

func TestHexDigestBytes_MatchesSha256Hex(t *testing.T) {
	data := []byte("digest-me")
	sum := sha256.Sum256(data)
	if want := hex.EncodeToString(sum[:]); hexDigestBytes(data) != want {
		t.Fatalf("hexDigestBytes = %q, want %q", hexDigestBytes(data), want)
	}
}

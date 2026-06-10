package skills

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestSnapshotFromConfig_Golden pins the 1:1 projection.
func TestSnapshotFromConfig_Golden(t *testing.T) {
	t.Parallel()
	snap := SnapshotFromConfig(config.SkillsConfig{
		Driver: "localdb",
		DSN:    "file:skills.db",
	})
	want := ConfigSnapshot{Driver: "localdb", DSN: "file:skills.db"}
	if snap != want {
		t.Errorf("SnapshotFromConfig = %+v, want %+v", snap, want)
	}
}

// TestSnapshotFromConfig_FieldParity_SkillsConfig — the reflection gate
// (Phase 110c): every `config.SkillsConfig` field is projected or
// explicitly excluded with a reason. Closes the D-155/B3 silent-field-
// drop class for the skills seam.
func TestSnapshotFromConfig_FieldParity_SkillsConfig(t *testing.T) {
	t.Parallel()
	projected := map[string]bool{
		"Driver": true,
		"DSN":    true,
	}
	excluded := map[string]string{
		// Phase 111d (D-201): the directory block feeds NewDirectory
		// via DirectoryFromConfig, not the store snapshot.
		"Directory": "projected by DirectoryFromConfig (separate constructor)",
	}
	typ := reflect.TypeOf(config.SkillsConfig{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		_, isProjected := projected[f.Name]
		_, isExcluded := excluded[f.Name]
		switch {
		case isProjected && isExcluded:
			t.Errorf("SkillsConfig.%s listed as both projected and excluded", f.Name)
		case !isProjected && !isExcluded:
			t.Errorf("SkillsConfig.%s is neither projected by SnapshotFromConfig nor excluded with a reason — map it or exclude it explicitly (D-155/B3 class)", f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field SkillsConfig.%s no longer exists — update the parity sets", name)
		}
	}
}

// TestDirectoryFromConfig_Golden pins the directory projection
// (Phase 111d — D-201), including the fallback-max semantics: an
// unset max_entries inherits the caller-resolved fallback (the
// planner.skills_context_max budget knob); an explicit value wins.
func TestDirectoryFromConfig_Golden(t *testing.T) {
	t.Parallel()
	cfg := config.SkillsConfig{
		Directory: config.SkillsDirectoryConfig{
			Pinned:    []string{"triage-incident", "summarise-paper"},
			Selection: "pinned_then_top",
		},
	}
	got := DirectoryFromConfig(cfg, 7)
	if got.MaxEntries != 7 {
		t.Errorf("fallback MaxEntries = %d, want 7", got.MaxEntries)
	}
	if got.Selection != SelectionPinnedThenTop {
		t.Errorf("Selection = %q, want %q", got.Selection, SelectionPinnedThenTop)
	}
	if len(got.Pinned) != 2 || got.Pinned[0] != "triage-incident" {
		t.Errorf("Pinned = %v, want declaration order preserved", got.Pinned)
	}

	cfg.Directory.MaxEntries = 42
	if got := DirectoryFromConfig(cfg, 7); got.MaxEntries != 42 {
		t.Errorf("explicit MaxEntries = %d, want 42 (explicit wins over fallback)", got.MaxEntries)
	}

	// Copied, not aliased: mutating the config slice must not reach
	// the projection.
	proj := DirectoryFromConfig(cfg, 7)
	cfg.Directory.Pinned[0] = "mutated"
	if proj.Pinned[0] != "triage-incident" {
		t.Error("DirectoryFromConfig aliased the caller's Pinned slice")
	}
}

// TestDirectoryFromConfig_FieldParity_SkillsDirectoryConfig — the
// reflection gate for the directory block: every
// `config.SkillsDirectoryConfig` field is projected (D-155/B3 class).
func TestDirectoryFromConfig_FieldParity_SkillsDirectoryConfig(t *testing.T) {
	t.Parallel()
	projected := map[string]bool{
		"Pinned":     true,
		"MaxEntries": true,
		"Selection":  true,
	}
	typ := reflect.TypeOf(config.SkillsDirectoryConfig{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		if !projected[f.Name] {
			t.Errorf("SkillsDirectoryConfig.%s is not projected by DirectoryFromConfig — map it (D-155/B3 class)", f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field SkillsDirectoryConfig.%s no longer exists — update the parity set", name)
		}
	}
}

// TestDirectoryFromConfig_SelectionAllowlistMirrorsValidator pins the
// §4.4 mirror between the config validator's selection allowlist
// (`internal/config/validate.go::allowedSkillsDirectorySelections`)
// and the canonical Selection values here. The validator cannot
// import this package; this test asserts every canonical Selection
// round-trips through config validation.
func TestDirectoryFromConfig_SelectionAllowlistMirrorsValidator(t *testing.T) {
	t.Parallel()
	validate := func(sel Selection) error {
		cfg := config.Defaults()
		// Required-for-core LLM fields (documented dummy values —
		// the validator only checks non-emptiness here).
		cfg.LLM.Provider = "openrouter"
		cfg.LLM.Model = "test/model"
		cfg.LLM.APIKey = "env.HARBOR_TEST_DUMMY_KEY"
		cfg.Skills.Driver = "localdb"
		cfg.Skills.DSN = ":memory:"
		cfg.Skills.Directory.Selection = string(sel)
		return cfg.ValidateCore()
	}
	if err := validate(SelectionPinnedThenRecent); err != nil {
		t.Errorf("config validator rejects canonical Selection %q: %v — the allowlists drifted", SelectionPinnedThenRecent, err)
	}
	// Wave C checkpoint audit (D-201 addendum): `pinned_then_top` stays
	// a canonical library-level Selection (an embedder that bumps
	// UseCount through Upsert can use it), but the OPERATOR validator
	// rejects it loud until a production usage-bump path lands — a knob
	// that validates cleanly and silently degrades to alphabetical
	// ordering violates §13. The rejection message must name the gap,
	// not read as an allowlist miss.
	err := validate(SelectionPinnedThenTop)
	if err == nil {
		t.Errorf("config validator accepts %q, want a loud not-wired-yet rejection (no production UseCount bump path exists)", SelectionPinnedThenTop)
	} else if !strings.Contains(err.Error(), "not wired yet") {
		t.Errorf("config validator rejection for %q = %v, want the not-wired-yet message (allowlist drift?)", SelectionPinnedThenTop, err)
	}
}

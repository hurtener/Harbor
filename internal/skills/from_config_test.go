package skills

import (
	"reflect"
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
	excluded := map[string]string{}
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

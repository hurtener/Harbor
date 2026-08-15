package memory

import (
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// TestSnapshotFromConfig_Golden pins the 1:1 projection.
func TestSnapshotFromConfig_Golden(t *testing.T) {
	t.Parallel()
	snap := SnapshotFromConfig(config.MemoryConfig{
		Driver:             "sqlite",
		DSN:                "file:mem.db",
		MigrationMode:      sqlmigrate.ModeVerify,
		Strategy:           "rolling_summary",
		BudgetTokens:       2048,
		RecoveryBacklogMax: 32,
		RecentTurns:        8,
		Retrieval:          "semantic",
		RetrievalTopK:      7,
	})
	want := ConfigSnapshot{
		Driver:             "sqlite",
		DSN:                "file:mem.db",
		MigrationMode:      sqlmigrate.ModeVerify,
		Strategy:           StrategyRollingSummary,
		BudgetTokens:       2048,
		RecoveryBacklogMax: 32,
		RecentTurns:        8,
		Retrieval:          RetrievalSemantic,
		RetrievalTopK:      7,
	}
	if snap != want {
		t.Errorf("SnapshotFromConfig = %+v, want %+v", snap, want)
	}
}

// TestSnapshotFromConfig_FieldParity_MemoryConfig — the reflection gate:
// every `config.MemoryConfig` field is projected by SnapshotFromConfig
// or explicitly excluded with a reason. Closes the D-155/B3 silent-field-
// drop class for the memory seam.
func TestSnapshotFromConfig_FieldParity_MemoryConfig(t *testing.T) {
	t.Parallel()
	projected := map[string]bool{
		"Driver":             true,
		"DSN":                true,
		"MigrationMode":      true,
		"Strategy":           true,
		"BudgetTokens":       true,
		"RecoveryBacklogMax": true,
		"RecentTurns":        true,
		"Retrieval":          true,
		"RetrievalTopK":      true,
	}
	// RetrievalMinScore is a recall-only knob that affects run-loop
	// injection policy, not the store's ConfigSnapshot.
	// It is projected by RecallFromConfig instead.
	excluded := map[string]string{
		"RetrievalMinScore": "recall-only: projected by RecallFromConfig, not SnapshotFromConfig",
		"Summarizer":        "summariser-only: consumed at assembly to build the rolling_summary Summarizer (WithModel + WithSystemPromptExtension), not part of the store ConfigSnapshot",
	}
	typ := reflect.TypeOf(config.MemoryConfig{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		_, isProjected := projected[f.Name]
		_, isExcluded := excluded[f.Name]
		switch {
		case isProjected && isExcluded:
			t.Errorf("MemoryConfig.%s listed as both projected and excluded", f.Name)
		case !isProjected && !isExcluded:
			t.Errorf("MemoryConfig.%s is neither projected by SnapshotFromConfig nor excluded with a reason — map it or exclude it explicitly (D-155/B3 class)", f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field MemoryConfig.%s no longer exists — update the parity sets", name)
		}
	}
}

// TestRecallFromConfig_Golden pins the RecallSettings projection.
func TestRecallFromConfig_Golden(t *testing.T) {
	t.Parallel()
	got := RecallFromConfig(config.MemoryConfig{
		Retrieval:         "semantic",
		RetrievalTopK:     3,
		RetrievalMinScore: 0.4,
	})
	want := RecallSettings{Enabled: true, TopK: 3, MinScore: 0.4}
	if got != want {
		t.Errorf("RecallFromConfig = %+v, want %+v", got, want)
	}
}

// TestRecallFromConfig_Disabled confirms zero/non-semantic configs
// produce Enabled=false.
func TestRecallFromConfig_Disabled(t *testing.T) {
	t.Parallel()
	got := RecallFromConfig(config.MemoryConfig{Retrieval: ""})
	if got.Enabled {
		t.Errorf("RecallFromConfig with no retrieval mode: Enabled = true, want false")
	}
}

// TestRecallFromConfig_FieldParity_RecallRelevant — the reflection gate
// for RecallFromConfig: every recall-relevant field in config.MemoryConfig
// is either projected here or explicitly excluded with a reason. This is
// the B3 drift-class guard for the recall side of the projection.
func TestRecallFromConfig_FieldParity_RecallRelevant(t *testing.T) {
	t.Parallel()
	// Fields RecallFromConfig projects.
	projected := map[string]bool{
		"Retrieval":         true,
		"RetrievalTopK":     true,
		"RetrievalMinScore": true,
	}
	// Fields that belong to the store snapshot, not recall settings.
	excluded := map[string]string{
		"Driver":             "store config, not recall",
		"DSN":                "store config, not recall",
		"MigrationMode":      "store migration policy, not recall",
		"Strategy":           "store config, not recall",
		"BudgetTokens":       "store config, not recall",
		"RecoveryBacklogMax": "store config, not recall",
		"RecentTurns":        "store config, not recall",
		"Summarizer":         "summariser config (model + prompt extension), consumed at assembly; not recall",
	}
	typ := reflect.TypeOf(config.MemoryConfig{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		_, isProjected := projected[f.Name]
		_, isExcluded := excluded[f.Name]
		switch {
		case isProjected && isExcluded:
			t.Errorf("MemoryConfig.%s listed as both projected and excluded in recall parity", f.Name)
		case !isProjected && !isExcluded:
			t.Errorf("MemoryConfig.%s is neither projected by RecallFromConfig nor excluded — map it or exclude it explicitly (D-155/B3 class)", f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field MemoryConfig.%s no longer exists — update parity sets", name)
		}
	}
}

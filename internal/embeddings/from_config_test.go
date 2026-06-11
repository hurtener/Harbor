package embeddings

import (
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// TestSnapshotFromConfig_Golden pins the 1:1 projection.
func TestSnapshotFromConfig_Golden(t *testing.T) {
	t.Parallel()
	snap := SnapshotFromConfig(config.EmbeddingsConfig{
		Driver:     "bifrost",
		Provider:   "openai",
		Model:      "text-embedding-3-small",
		APIKey:     "env.OPENAI_API_KEY",
		BaseURL:    "https://example.test/v1",
		Timeout:    30 * time.Second,
		Dimensions: 256,
	})
	want := ConfigSnapshot{
		Driver:     "bifrost",
		Provider:   "openai",
		Model:      "text-embedding-3-small",
		APIKey:     "env.OPENAI_API_KEY",
		BaseURL:    "https://example.test/v1",
		Timeout:    30 * time.Second,
		Dimensions: 256,
	}
	if snap != want {
		t.Errorf("SnapshotFromConfig = %+v, want %+v", snap, want)
	}
}

// TestSnapshotFromConfig_FieldParity_EmbeddingsConfig — the
// reflection gate (the D-155/B3 silent-field-drop class): every
// `config.EmbeddingsConfig` field is projected or explicitly
// excluded with a reason.
func TestSnapshotFromConfig_FieldParity_EmbeddingsConfig(t *testing.T) {
	t.Parallel()
	projected := map[string]bool{
		"Driver":     true,
		"Provider":   true,
		"Model":      true,
		"APIKey":     true,
		"BaseURL":    true,
		"Timeout":    true,
		"Dimensions": true,
	}
	excluded := map[string]string{}
	typ := reflect.TypeOf(config.EmbeddingsConfig{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		_, isProjected := projected[f.Name]
		_, isExcluded := excluded[f.Name]
		switch {
		case isProjected && isExcluded:
			t.Errorf("EmbeddingsConfig.%s listed as both projected and excluded", f.Name)
		case !isProjected && !isExcluded:
			t.Errorf("EmbeddingsConfig.%s is neither projected by SnapshotFromConfig nor excluded with a reason — map it or exclude it explicitly (D-155/B3 class)", f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field EmbeddingsConfig.%s no longer exists — update the parity sets", name)
		}
	}
}

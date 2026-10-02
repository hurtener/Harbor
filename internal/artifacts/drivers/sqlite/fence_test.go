package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/config"
)

func TestSQLite_AtomicScopeFence(t *testing.T) {
	conformancetest.RunScopeFence(t, func() (artifacts.ArtifactStore, func()) {
		s, err := sqlite.New(config.ArtifactsConfig{DSN: filepath.Join(t.TempDir(), "artifacts.db")})
		if err != nil {
			t.Fatal(err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}
func TestSQLite_ScopeFenceReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "artifacts.db")
	conformancetest.AssertFenceSurvivesReopen(t, func() artifacts.ArtifactStore {
		s, err := sqlite.New(config.ArtifactsConfig{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

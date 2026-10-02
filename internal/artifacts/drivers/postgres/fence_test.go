package postgres_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	"github.com/hurtener/Harbor/internal/config"
)

func TestPostgres_AtomicScopeFence(t *testing.T) {
	dsn := requireDSN(t)
	conformancetest.RunScopeFence(t, func() (artifacts.ArtifactStore, func()) {
		s, err := postgres.New(config.ArtifactsConfig{DSN: freshSchema(t, dsn)})
		if err != nil {
			t.Fatal(err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}
func TestPostgres_ScopeFenceReopen(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	conformancetest.AssertFenceSurvivesReopen(t, func() artifacts.ArtifactStore {
		s, err := postgres.New(config.ArtifactsConfig{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

package inmem_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
)

func TestInmem_AtomicScopeFence(t *testing.T) {
	conformancetest.RunScopeFence(t, func() (artifacts.ArtifactStore, func()) {
		s, err := inmem.New(config.ArtifactsConfig{})
		if err != nil {
			t.Fatal(err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

package serve

import (
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// TestCatalogCacheDefaultsMatchProjector pins the documented config defaults
// to the projector's built-in defaults, so an unconfigured runtime and an
// explicitly defaulted one behave identically.
func TestCatalogCacheDefaultsMatchProjector(t *testing.T) {
	eff := config.ToolsConfig{}.EffectiveCatalogCache()
	def := toolsprotocol.DefaultStructuralCacheLimits()
	if eff.MaxBytes != def.MaxBytes || eff.MaxEntryBytes != def.MaxEntryBytes || eff.MaxEntries != def.MaxEntries || eff.IdleTTL != def.IdleTTL {
		t.Fatalf("config defaults %+v != projector defaults %+v", eff, def)
	}
}

package bifrost

import (
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
)

// newDriverWithClient is the package-private constructor that the
// test suite uses to inject a stub bifrost client. Production callers
// go through `New` (which builds a real `*bf.Bifrost`).
//
// Visible only to tests in this package (file ends with `_test.go`).
func newDriverWithClient(client bifrostClient, provider bfschemas.ModelProvider, bus events.EventBus) *Driver {
	return &Driver{
		client:   client,
		provider: provider,
		bus:      bus,
		files:    newProviderFileCache(defaultFileCacheCapacity, defaultFileCacheTTL),
	}
}

// newDriverWithClientAndStore mirrors newDriverWithClient with an
// artifact store wired, for the provider-native upload-pass tests.
func newDriverWithClientAndStore(client bifrostClient, provider bfschemas.ModelProvider, bus events.EventBus, store artifacts.ArtifactStore) *Driver {
	d := newDriverWithClient(client, provider, bus)
	d.artifacts = store
	return d
}

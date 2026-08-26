package bifrost

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestRouteClientPool_IsolatesTenantAndGenerationAndEvictsBounded(t *testing.T) {
	pool := newRouteClientPool(2, llm.NetworkDefaults{})
	t.Cleanup(pool.Close)
	endpoint := "http://127.0.0.1:18080"
	base := routeClientPoolKey{
		TenantID: "tenant-a", RuntimeID: "runtime", RouteID: "route", RouteGeneration: 1,
		ProviderConnectionID: "connection", ProviderConnectionGeneration: 1,
		CredentialAssetGeneration: 1, Provider: "openai", EndpointDigest: "digest",
	}
	clientA, releaseA, err := pool.acquire(context.Background(), base, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	releaseA()
	tenantB := base
	tenantB.TenantID = "tenant-b"
	clientB, releaseB, err := pool.acquire(context.Background(), tenantB, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if clientA == clientB {
		t.Fatal("two tenants shared one endpoint route client")
	}
	releaseB()
	rotated := base
	rotated.CredentialAssetGeneration++
	clientRotated, releaseRotated, err := pool.acquire(context.Background(), rotated, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if clientRotated == clientA {
		t.Fatal("credential generation rotation reused stale endpoint client")
	}
	releaseRotated()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.entries) != 2 || pool.entries[rotated] == nil || pool.entries[tenantB] == nil {
		t.Fatalf("bounded pool entries = %#v", pool.entries)
	}
}

func TestRouteClientPool_RefusesOverflowWhileAllEntriesBusyAndCloses(t *testing.T) {
	pool := newRouteClientPool(1, llm.NetworkDefaults{})
	key := routeClientPoolKey{TenantID: "tenant", RuntimeID: "runtime", RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, Provider: "openai", EndpointDigest: "a"}
	_, release, err := pool.acquire(context.Background(), key, "http://127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	other := key
	other.EndpointDigest = "b"
	if _, _, err := pool.acquire(context.Background(), other, "http://127.0.0.1:18081"); !errors.Is(err, errRouteClientPoolBusy) {
		t.Fatalf("busy pool error = %v", err)
	}
	release()
	pool.Close()
	if _, _, err := pool.acquire(context.Background(), key, "http://127.0.0.1:18080"); !errors.Is(err, llm.ErrClientClosed) {
		t.Fatalf("closed pool error = %v", err)
	}
}

package a2a_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	a2adrv "github.com/hurtener/Harbor/internal/distributed/drivers/a2a"
)

func TestRegistry_AddPeer_RejectsEmptyURL(t *testing.T) {
	reg := a2adrv.NewRegistry()
	err := reg.AddPeer(a2adrv.PeerSpec{TrustTier: 3})
	if !errors.Is(err, a2adrv.ErrInvalidPeerURL) {
		t.Errorf("expected ErrInvalidPeerURL, got %v", err)
	}
}

func TestRegistry_AddPeer_RejectsOutOfRangeTier(t *testing.T) {
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://a", TrustTier: 0}); err == nil {
		t.Errorf("expected error for TrustTier=0")
	}
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://a", TrustTier: 6}); err == nil {
		t.Errorf("expected error for TrustTier=6")
	}
}

func TestRegistry_Resolve_TrustTierMonotonic(t *testing.T) {
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://low", TrustTier: 1, LatencyTierMS: 10}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://mid", TrustTier: 3, LatencyTierMS: 10}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://high", TrustTier: 5, LatencyTierMS: 10}); err != nil {
		t.Fatal(err)
	}
	routes, err := reg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(routes) != 3 {
		t.Fatalf("want 3 routes, got %d", len(routes))
	}
	if routes[0].PeerURL != "https://high" || routes[1].PeerURL != "https://mid" || routes[2].PeerURL != "https://low" {
		t.Errorf("ranking mismatch: %v", routes)
	}
}

func TestRegistry_Resolve_LatencyTieBreaker(t *testing.T) {
	reg := a2adrv.NewRegistry()
	for i, lat := range []int{100, 10, 1000} {
		if err := reg.AddPeer(a2adrv.PeerSpec{
			URL:           fmt.Sprintf("https://peer-%d", i),
			TrustTier:     3,
			LatencyTierMS: lat,
		}); err != nil {
			t.Fatal(err)
		}
	}
	routes, err := reg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Lower latency wins: lat=10 first, then 100, then 1000.
	if routes[0].LatencyTierMS != 10 || routes[1].LatencyTierMS != 100 || routes[2].LatencyTierMS != 1000 {
		t.Errorf("latency tie-breaker failed: %v", routes)
	}
}

func TestRegistry_Resolve_CapabilityMatchBoost(t *testing.T) {
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://generalist", TrustTier: 3, LatencyTierMS: 10, Capabilities: []string{"echo", "summarize"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://specialist", TrustTier: 3, LatencyTierMS: 10, Capabilities: []string{"translate"}}); err != nil {
		t.Fatal(err)
	}
	routes, err := reg.Resolve("translate")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(routes) == 0 || routes[0].PeerURL != "https://specialist" {
		t.Errorf("expected specialist first, got %+v", routes)
	}
}

func TestRegistry_Resolve_EmptyRegistry(t *testing.T) {
	reg := a2adrv.NewRegistry()
	_, err := reg.Resolve("anything")
	if !errors.Is(err, a2adrv.ErrPeerNotAllowed) {
		t.Errorf("expected ErrPeerNotAllowed, got %v", err)
	}
}

func TestRegistry_ConcurrentResolve_NoRace(t *testing.T) {
	reg := a2adrv.NewRegistry()
	for i := 1; i <= 5; i++ {
		_ = reg.AddPeer(a2adrv.PeerSpec{
			URL:           fmt.Sprintf("https://peer-%d", i),
			TrustTier:     i,
			LatencyTierMS: 10 * i,
			Capabilities:  []string{"echo"},
		})
	}
	const N = 128
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if routes, err := reg.Resolve("echo"); err != nil || len(routes) == 0 {
				t.Errorf("Resolve(echo): err=%v routes=%v", err, routes)
			}
		}()
	}
	wg.Wait()
}

func TestRegistry_UpdateCapabilities_RoundTrip(t *testing.T) {
	reg := a2adrv.NewRegistry()
	if err := reg.AddPeer(a2adrv.PeerSpec{URL: "https://p", TrustTier: 3, LatencyTierMS: 10}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpdateCapabilities("https://p", []string{"echo"}, []string{"test"}); err != nil {
		t.Fatalf("UpdateCapabilities: %v", err)
	}
	routes, err := reg.Resolve("echo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(routes) != 1 || routes[0].CapabilityScore != 1 {
		t.Errorf("expected one route with CapabilityScore=1, got %v", routes)
	}
}

func TestRegistry_RemovePeer(t *testing.T) {
	reg := a2adrv.NewRegistry()
	_ = reg.AddPeer(a2adrv.PeerSpec{URL: "https://p", TrustTier: 3, LatencyTierMS: 10})
	reg.RemovePeer("https://p")
	if got, _ := reg.PeerSpec("https://p"); got.URL != "" {
		t.Errorf("RemovePeer left record: %v", got)
	}
}

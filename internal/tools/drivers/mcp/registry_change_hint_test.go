package mcp

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/auth"
)

// TestRegistry_ChangeObserver_ExactRemovalStage pins the review gap: moving
// an invalidated private stage into closing (BeginExactRemoval) and releasing
// it (Cancel) both move the registry generation, so both report a structural
// change to the observer — after the registry lock is released.
func TestRegistry_ChangeObserver_ExactRemovalStage(t *testing.T) {
	var mu sync.Mutex
	var changes []RegistryChangeReason
	var reg *Registry
	reg = NewRegistry(WithRegistryChangeObserver(func(_ context.Context, name string, owner auth.Owner, reason RegistryChangeReason) {
		// A re-entrant read proves the observer runs outside the lock.
		reg.OwnerOf(name)
		if owner.Agent != "agent" {
			t.Errorf("owner = %+v", owner)
		}
		mu.Lock()
		changes = append(changes, reason)
		mu.Unlock()
	}))
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	provider := &stubProvider{id: "staged-hint"}
	if _, err := reg.StageRegistration(ServerRegistration{Provider: provider, Transport: "http+sse", Owner: owner, DescriptorFingerprint: "generation-a"}, nil); err != nil {
		t.Fatal(err)
	}
	before := reg.RegistryGeneration()
	fence, err := reg.BeginExactRemoval("staged-hint", owner, "generation-a")
	if err != nil {
		t.Fatal(err)
	}
	afterBegin := reg.RegistryGeneration()
	if afterBegin == before {
		t.Fatal("invalidating a stage into closing did not move the generation")
	}
	if err := fence.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.RegistryGeneration() == afterBegin {
		t.Fatal("releasing the invalidated stage did not move the generation")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 2 || changes[0] != RegistryChangeConnection || changes[1] != RegistryChangeConnection {
		t.Fatalf("observer changes = %v, want two connection changes", changes)
	}
}

package engine_test

import (
	"context"
	"errors"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func TestEngine_ProviderRoutePersistsDefensivelyAndFencesIdempotency(t *testing.T) {
	bus := mkBus(t)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })

	id := identity.Identity{TenantID: "tenant-route", UserID: "user-route", SessionID: "session-route"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	route := &llm.ProviderRoute{
		RouteID: "route-a", RouteGeneration: 4,
		ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 3,
		CredentialAssetGeneration: 2, ModelSelector: "balanced",
	}
	req := tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
		Query: "route persistence", IdempotencyKey: "route-key", ProviderRoute: route,
	}
	first, err := eng.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// The stored task must not alias the caller's mutable request pointer.
	route.RouteGeneration = 99
	stored, err := eng.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.ProviderRoute == nil || stored.ProviderRoute.RouteGeneration != 4 {
		t.Fatalf("stored route generation = %+v, want 4", stored.ProviderRoute)
	}

	exact := req
	exact.ProviderRoute = &llm.ProviderRoute{
		RouteID: "route-a", RouteGeneration: 4,
		ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 3,
		CredentialAssetGeneration: 2, ModelSelector: "balanced",
	}
	reused, err := eng.Spawn(ctx, exact)
	if err != nil || !reused.Reused || reused.ID != first.ID {
		t.Fatalf("exact route retry = (%+v, %v), want reused task %q", reused, err, first.ID)
	}

	rotated := exact
	copyRoute := *exact.ProviderRoute
	copyRoute.CredentialAssetGeneration++
	rotated.ProviderRoute = &copyRoute
	if _, err := eng.Spawn(ctx, rotated); !errors.Is(err, tasks.ErrIdempotencyConflict) {
		t.Fatalf("same key with rotated route = %v, want ErrIdempotencyConflict", err)
	}
}

func TestValidateRequest_ProviderRouteRejectsExplicitEmptySelector(t *testing.T) {
	err := tasks.ValidateRequest(tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}},
		Kind:     tasks.KindForeground, ProviderRoute: &llm.ProviderRoute{},
	})
	if !errors.Is(err, tasks.ErrInvalidRequest) {
		t.Fatalf("ValidateRequest(empty provider route) = %v, want ErrInvalidRequest", err)
	}
}

package protocol_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestStartLLMSettingsConcurrentIsolationAndReplay(t *testing.T) {
	fx := newSurfaceFixture(t)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Shared sessions and different tenants exercise both collision shapes.
			id := identity.Identity{TenantID: fmt.Sprint("tenant-", i%4), UserID: "user", SessionID: "shared"}
			ctx, err := identity.WithVerified(context.Background(), id)
			if err != nil {
				t.Error(err)
				return
			}
			model, effort, max := fmt.Sprint("model-", i), []string{"low", "medium", "high"}[i%3], 100+i
			req := &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, IdempotencyKey: fmt.Sprint("choice-", i), LLMSettings: &types.RunLLMSettings{Model: &model, ReasoningEffort: &effort, MaxTokens: &max}}
			first, err := fx.surface.Dispatch(ctx, methods.MethodStart, req)
			if err != nil {
				t.Error(err)
				return
			}
			replay, err := fx.surface.Dispatch(ctx, methods.MethodStart, req)
			if err != nil {
				t.Error(err)
				return
			}
			a, b := first.(*types.StartResponse), replay.(*types.StartResponse)
			if a.TaskID != b.TaskID || !b.Reused {
				t.Error("exact replay did not reuse task")
			}
			// Mutating caller memory and returned snapshots must not change accepted work.
			expectedModel, expectedEffort, expectedMax := model, effort, max
			model, effort, max = "changed", "off", 1
			if _, err := fx.surface.Dispatch(ctx, methods.MethodStart, req); err == nil {
				t.Error("changed selection reused the original idempotency key")
			}
			got, err := fx.tasks.Get(ctx, tasks.TaskID(a.TaskID))
			if err != nil {
				t.Error(err)
				return
			}
			if got.LLMSettings == nil || *got.LLMSettings.Model != expectedModel || *got.LLMSettings.ReasoningEffort != expectedEffort || *got.LLMSettings.MaxTokens != expectedMax {
				t.Errorf("wrong accepted settings: %+v", got.LLMSettings)
				return
			}
			*got.LLMSettings.Model = "mutated snapshot"
			again, err := fx.tasks.Get(ctx, tasks.TaskID(a.TaskID))
			if err != nil {
				t.Error(err)
				return
			}
			if *again.LLMSettings.Model != expectedModel {
				t.Error("task get leaked mutable settings")
			}
		}()
	}
	wg.Wait()
}

func TestStartLLMSettingsInvalidRefused(t *testing.T) {
	fx := newSurfaceFixture(t)
	empty, invalid, zero := "", "turbo", 0
	for _, s := range []*types.RunLLMSettings{{Model: &empty}, {ReasoningEffort: &invalid}, {MaxTokens: &zero}} {
		_, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, LLMSettings: s})
		if err == nil {
			t.Errorf("accepted invalid settings %+v", s)
		}
	}
	model := "native-model"
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, LLMSettings: &types.RunLLMSettings{Model: &model}, ProviderRoute: &types.LLMProviderRouteSelector{RouteID: "route", RouteGeneration: 1, ProviderConnectionID: "connection", ProviderConnectionGeneration: 1, CredentialAssetGeneration: 1, ModelSelector: "routed"}})
	if err == nil {
		t.Fatal("accepted ambiguous native and routed models")
	}
}

package projection_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func ps(s string) *string { return &s }

// TestActivePromptLayers_NoRegistry returns the no-op path.
func TestActivePromptLayers_NoRegistry(t *testing.T) {
	base, user, ok, err := projection.ActivePromptLayers(context.Background(), nil, projAgent, projID())
	if err != nil || ok || base != "" || user != "" {
		t.Fatalf("nil registry should be a no-op: base=%q user=%q ok=%v err=%v", base, user, ok, err)
	}
}

// TestActivePromptLayers_NoActiveRevision returns the backward-compatible path.
func TestActivePromptLayers_NoActiveRevision(t *testing.T) {
	reg := newRegistry(t)
	_, _, ok, err := projection.ActivePromptLayers(context.Background(), reg, projAgent, projID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("no active revision should return ok=false")
	}
}

// TestActivePromptLayers_ResolvesBaseAndUser proves the projection reads the
// active revision's base + user layers.
func TestActivePromptLayers_ResolvesBaseAndUser(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("the base"), User: ps("the user")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	base, user, ok, err := projection.ActivePromptLayers(ctx, reg, projAgent, projID())
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if base != "the base" || user != "the user" {
		t.Fatalf("base=%q user=%q", base, user)
	}
}

// TestActivePromptLayers_NoPromptSection returns ok=false when the active
// revision has only sibling sections.
func TestActivePromptLayers_NoPromptSection(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	_, _, ok, err := projection.ActivePromptLayers(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("a revision with no prompt-layer section should return ok=false")
	}
}

// TestApplyPromptLayers_OverlaysOntoNil allocates a bundle when none exists.
func TestApplyPromptLayers_OverlaysOntoNil(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("B"), User: ps("U")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov == nil || ov.BasePromptLayer == nil || *ov.BasePromptLayer != "B" ||
		ov.UserPromptLayer == nil || *ov.UserPromptLayer != "U" {
		t.Fatalf("overlay = %+v", ov)
	}
}

// TestApplyPromptLayers_PreservesExistingOverrides overlays onto an existing
// bundle without clobbering its other fields.
func TestApplyPromptLayers_PreservesExistingOverrides(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("B")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	existing := &planner.LLMOverrides{ExtraInstructions: ps("tenant default")}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), existing)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov.ExtraInstructions == nil || *ov.ExtraInstructions != "tenant default" {
		t.Fatalf("extra instructions clobbered: %+v", ov)
	}
	if ov.BasePromptLayer == nil || *ov.BasePromptLayer != "B" {
		t.Fatalf("base not applied: %+v", ov)
	}
	if ov.UserPromptLayer != nil {
		t.Fatalf("user should be unset when not configured: %+v", ov.UserPromptLayer)
	}
}

// TestApplyPromptLayers_NoLayersUnchanged leaves the bundle untouched when no
// durable layers are configured.
func TestApplyPromptLayers_NoLayersUnchanged(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	existing := &planner.LLMOverrides{Model: ps("m")}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), existing)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov != existing {
		t.Fatal("bundle should be returned unchanged when no durable layers exist")
	}
}

// TestApplyPromptLayers_DurableUserLayer_Precedence proves the durable
// USER-scope layer composes BETWEEN the admin user layer and the session
// overlay — admin Base > admin User > USER-durable > session User — in the
// single lower-trust <user_instructions> block.
func TestApplyPromptLayers_DurableUserLayer_Precedence(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	// admin Base + admin User (ConfigScopeAgent).
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("admin-base"), User: ps("admin-user")},
	}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	// the durable USER-scope layer (ConfigScopeUser).
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{User: ps("durable-user")},
	}); err != nil {
		t.Fatalf("set durable: %v", err)
	}
	// the ephemeral session overlay.
	if _, err := ov.SetUserPrompt(ctx, projID(), projAgent, "session-user"); err != nil {
		t.Fatalf("set session: %v", err)
	}

	got, err := projection.ApplyPromptLayers(ctx, reg, ov, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.BasePromptLayer == nil || *got.BasePromptLayer != "admin-base" {
		t.Fatalf("admin base not the spine: %+v", got.BasePromptLayer)
	}
	want := "admin-user\n\ndurable-user\n\nsession-user"
	if got.UserPromptLayer == nil || *got.UserPromptLayer != want {
		t.Fatalf("user composition wrong:\n got=%q\nwant=%q", deref(got.UserPromptLayer), want)
	}
}

// TestApplyPromptLayers_DurableUserLayer_AloneReachesRun proves the durable
// layer alone (no admin, no session) reaches the run.
func TestApplyPromptLayers_DurableUserLayer_AloneReachesRun(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{User: ps("just-durable")},
	}); err != nil {
		t.Fatalf("set durable: %v", err)
	}
	got, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got == nil || got.UserPromptLayer == nil || *got.UserPromptLayer != "just-durable" {
		t.Fatalf("durable-only layer did not reach the run: %+v", got)
	}
}

// TestApplyPromptLayers_EmptyDurable_ByteIdentical proves a run with no durable
// USER-scope layer composes exactly as before this phase (admin User + session
// User, no durable segment).
func TestApplyPromptLayers_EmptyDurable_ByteIdentical(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{User: ps("admin-user")},
	}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	if _, err := ov.SetUserPrompt(ctx, projID(), projAgent, "session-user"); err != nil {
		t.Fatalf("set session: %v", err)
	}
	got, err := projection.ApplyPromptLayers(ctx, reg, ov, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := "admin-user\n\nsession-user"
	if got.UserPromptLayer == nil || *got.UserPromptLayer != want {
		t.Fatalf("empty-durable composition not byte-identical:\n got=%q\nwant=%q", deref(got.UserPromptLayer), want)
	}
}

// TestApplyPromptLayers_DurableReadError_FailsLoud proves a registry read error
// on the durable USER-scope layer fails the run loudly (no silent drop).
func TestApplyPromptLayers_DurableReadError_FailsLoud(t *testing.T) {
	ctx := context.Background()
	sentinel := errSentinel("durable read exploded")
	reg := &userScopeErrRegistry{err: sentinel}
	_, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("durable read error must fail loud, got %v", err)
	}
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// userScopeErrRegistry is a fake agentcfg.Registry that errors ONLY on a
// ConfigScopeUser Active read (the durable user-layer read), and is a no-op on
// the ConfigScopeAgent path, so it isolates the durable-read fail-loud path
// from the admin-layer read.
type userScopeErrRegistry struct{ err error }

func (r *userScopeErrRegistry) Active(_ context.Context, _ identity.Quadruple, _ string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	if scope == agentcfg.ConfigScopeUser {
		return agentcfg.Revision{}, false, r.err
	}
	return agentcfg.Revision{}, false, nil
}
func (r *userScopeErrRegistry) SetRevision(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, agentcfg.ConfigPayload) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, nil
}
func (r *userScopeErrRegistry) Get(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, nil
}
func (r *userScopeErrRegistry) ListRevisions(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, int) ([]agentcfg.Revision, error) {
	return nil, nil
}
func (r *userScopeErrRegistry) Rollback(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, nil
}
func (r *userScopeErrRegistry) Diff(context.Context, identity.Quadruple, string, string, string, agentcfg.ConfigScope) (agentcfg.Diff, error) {
	return agentcfg.Diff{}, nil
}
func (r *userScopeErrRegistry) Close(context.Context) error { return nil }

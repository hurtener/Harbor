package projection_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func ps(s string) *string { return &s }

// TestActivePromptLayers_NoRegistry returns the no-op path.
func TestActivePromptLayers_NoRegistry(t *testing.T) {
	base, user, guidance, ok, err := projection.ActivePromptLayers(context.Background(), nil, projAgent, projID())
	if err != nil || ok || base != "" || user != "" || guidance != nil {
		t.Fatalf("nil registry should be a no-op: base=%q user=%q guidance=%v ok=%v err=%v", base, user, guidance, ok, err)
	}
}

// TestActivePromptLayers_NoActiveRevision returns the backward-compatible path.
func TestActivePromptLayers_NoActiveRevision(t *testing.T) {
	reg := newRegistry(t)
	_, _, _, ok, err := projection.ActivePromptLayers(context.Background(), reg, projAgent, projID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("no active revision should return ok=false")
	}
}

func TestActivePromptLayers_ResolvesBaseUserAndGuidance(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("the base"), User: ps("the user"), Guidance: ps("  the guidance\n")},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set: %v", err)
	}
	base, user, guidance, ok, err := projection.ActivePromptLayers(ctx, reg, projAgent, projID())
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if base != "the base" || user != "the user" || deref(guidance) != "  the guidance\n" {
		t.Fatalf("base=%q user=%q guidance=%q", base, user, deref(guidance))
	}
}

// TestActivePromptLayers_NoPromptSection returns ok=false when the active
// revision has only sibling sections.
func TestActivePromptLayers_NoPromptSection(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set: %v", err)
	}
	_, _, _, ok, err := projection.ActivePromptLayers(ctx, reg, projAgent, projID())
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
	}, agentcfg.SetOptions{}); err != nil {
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

func TestApplyPromptLayers_GuidanceOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		guidance *string
		want     string
	}{
		{name: "unset"},
		{name: "empty", guidance: ps("")},
		{name: "whitespace", guidance: ps(" \n\t ")},
		{name: "verbatim", guidance: ps("  <operator>\n\tGuidance & details.\n "), want: "  <operator>\n\tGuidance & details.\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			reg := newRegistry(t)
			if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
				PromptLayers: &agentcfg.PromptLayers{Guidance: tc.guidance},
			}, agentcfg.SetOptions{}); err != nil {
				t.Fatalf("set guidance: %v", err)
			}
			ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
			if err != nil {
				t.Fatalf("apply guidance: %v", err)
			}
			if tc.want == "" {
				if ov != nil {
					t.Fatalf("blank guidance allocated a bundle: %+v", ov)
				}
				return
			}
			if ov == nil || deref(ov.GuidanceOverride) != tc.want || ov.BasePromptLayer != nil || ov.UserPromptLayer != nil {
				t.Fatalf("guidance-only projection should set only guidance: %+v", ov)
			}
		})
	}
}

func TestApplyPromptLayers_GuidanceConcurrentIsolation(t *testing.T) {
	ctx := t.Context()
	reg := newRegistry(t)
	for _, tenant := range []string{projTenant, "other-tenant"} {
		id := projID()
		id.TenantID = tenant
		if _, err := reg.SetRevision(ctx, id, projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			PromptLayers: &agentcfg.PromptLayers{Guidance: ps("guidance for " + tenant)},
		}, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("set %s guidance: %v", tenant, err)
		}
	}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			id := projID()
			id.SessionID = fmt.Sprintf("session-%d", i)
			if i%2 == 1 {
				id.TenantID = "other-tenant"
			}
			ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, id, nil)
			if err != nil || ov == nil {
				t.Errorf("%s: apply guidance: bundle=%+v err=%v", id.SessionID, ov, err)
				return
			}
			if got, want := deref(ov.GuidanceOverride), "guidance for "+id.TenantID; got != want {
				t.Errorf("%s: guidance = %q, want %q", id.SessionID, got, want)
			}
		})
	}
	wg.Wait()
}

// TestApplyPromptLayers_PreservesExistingOverrides overlays onto an existing
// bundle without clobbering its other fields.
func TestApplyPromptLayers_PreservesExistingOverrides(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("B"), Guidance: ps("G")},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set: %v", err)
	}
	existing := &planner.LLMOverrides{ExtraInstructions: ps("tenant default"), SystemPromptOverride: ps("one-shot")}
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
	if deref(ov.GuidanceOverride) != "G" || deref(ov.SystemPromptOverride) != "one-shot" {
		t.Fatalf("guidance and one-shot base override should coexist: %+v", ov)
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
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	// the durable USER-scope layer (ConfigScopeUser).
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{User: ps("durable-user")},
	}, agentcfg.SetOptions{}); err != nil {
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
	}, agentcfg.SetOptions{}); err != nil {
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
	}, agentcfg.SetOptions{}); err != nil {
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

// composedUserLayer builds a fresh registry + overlay with the given (admin,
// durable, session) user segments — a segment is set only when non-empty, so
// "" models an absent tier and a whitespace string models a present-but-blank
// one — then returns the projected <user_instructions> composition. Helper for
// the subset table below.
func composedUserLayer(t *testing.T, adminUser, durableUser, sessionUser string) string {
	t.Helper()
	ctx := context.Background()
	reg := newRegistry(t)
	ov := newOverlay(t)
	if adminUser != "" {
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			PromptLayers: &agentcfg.PromptLayers{User: ps(adminUser)},
		}, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("set admin: %v", err)
		}
	}
	if durableUser != "" {
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{
			PromptLayers: &agentcfg.PromptLayers{User: ps(durableUser)},
		}, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("set durable: %v", err)
		}
	}
	if sessionUser != "" {
		if _, err := ov.SetUserPrompt(ctx, projID(), projAgent, sessionUser); err != nil {
			t.Fatalf("set session: %v", err)
		}
	}
	got, err := projection.ApplyPromptLayers(ctx, reg, ov, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got == nil || got.UserPromptLayer == nil {
		return ""
	}
	return *got.UserPromptLayer
}

// TestApplyPromptLayers_UserLayerSubsets covers EVERY remaining subset of the
// three user segments (the precedence/alone/empty-durable tests above cover
// all-three, durable-only, admin+session, and none) plus the whitespace-only
// drop — composeUserLayer trims each segment and joins only the non-empty ones,
// so a blank segment must vanish from the composition, not leave a dangling
// "\n\n".
func TestApplyPromptLayers_UserLayerSubsets(t *testing.T) {
	cases := []struct {
		name, admin, durable, session, want string
	}{
		{"admin+durable", "admin-user", "durable-user", "", "admin-user\n\ndurable-user"},
		{"durable+session", "", "durable-user", "session-user", "durable-user\n\nsession-user"},
		{"admin-only", "admin-user", "", "", "admin-user"},
		{"session-only", "", "", "session-user", "session-user"},
		{"whitespace admin dropped", "   ", "durable-user", "session-user", "durable-user\n\nsession-user"},
		{"whitespace durable dropped", "admin-user", "  \t ", "session-user", "admin-user\n\nsession-user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := composedUserLayer(t, tc.admin, tc.durable, tc.session); got != tc.want {
				t.Fatalf("composed user layer = %q, want %q", got, tc.want)
			}
		})
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
func (r *userScopeErrRegistry) SetRevision(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, agentcfg.ConfigPayload, agentcfg.SetOptions) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, nil
}
func (r *userScopeErrRegistry) Get(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, nil
}
func (r *userScopeErrRegistry) ListRevisions(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, int) ([]agentcfg.Revision, error) {
	return nil, nil
}
func (r *userScopeErrRegistry) Rollback(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope, agentcfg.SetOptions) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, nil
}
func (r *userScopeErrRegistry) DeactivateIfActive(context.Context, identity.Quadruple, string, string, agentcfg.ConfigScope) (bool, error) {
	return false, nil
}
func (r *userScopeErrRegistry) Diff(context.Context, identity.Quadruple, string, string, string, agentcfg.ConfigScope) (agentcfg.Diff, error) {
	return agentcfg.Diff{}, nil
}
func (r *userScopeErrRegistry) Close(context.Context) error { return nil }

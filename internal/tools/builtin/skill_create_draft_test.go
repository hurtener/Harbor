package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/skills/drafter"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// skill_create_draft_test.go — the registration boundary and the
// ordinary-tool behaviour of the draft-only skill tool.
//
// The boundary contract: `skill_create_draft` is DISABLED BY DEFAULT.
// It is absent from KnownNames() and unreachable through the
// `tools.built_in` registry until the composition owner explicitly
// wires the registration carrier — so a default build never surfaces
// it to the planner, and enabling it is an ordinary per-agent
// operator decision.

// scriptedClient is a minimal scripted LLM client for the builtin
// tests.
type scriptedClient struct {
	content string
	err     error
}

func (c scriptedClient) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	if c.err != nil {
		return llm.CompleteResponse{}, c.err
	}
	return llm.CompleteResponse{Content: c.content}, nil
}

func (scriptedClient) Close(context.Context) error { return nil }

var _ llm.LLMClient = scriptedClient{}

func draftContentWithRequiredTools(required ...string) string {
	b, _ := json.Marshal(map[string]any{
		"name":           "builtin-draft",
		"trigger":        "when tested",
		"steps":          []string{"One step"},
		"required_tools": required,
	})
	return string(b)
}

func TestSkillCreateDraft_DisabledByDefault(t *testing.T) {
	// The tool IS a known built-in now (the composition owner wired the
	// carrier into the registry), but it stays DISABLED BY DEFAULT: it
	// is never registered implicitly — an operator must list it in
	// `tools.built_in` (like skill_propose), and registering it without
	// the composed LLM client fails loud rather than silently skipping.
	cat := tools.NewCatalog()
	if err := RegisterWith(RegistryContext{Catalog: cat}, nil); err != nil {
		t.Fatalf("RegisterWith(empty list): %v", err)
	}
	if _, ok := cat.Resolve(drafter.ToolName); ok {
		t.Fatal("skill_create_draft resolved without being listed in tools.built_in")
	}
	// Listing it without an LLM client is a LOUD registration failure
	// (the carrier's wiring-shaped check), never a silent no-op.
	err := RegisterWith(RegistryContext{Catalog: cat}, []string{drafter.ToolName})
	if !errors.Is(err, ErrRegisterFailed) {
		t.Fatalf("RegisterWith(skill_create_draft, no LLM client) err = %v, want ErrRegisterFailed", err)
	}
	if !strings.Contains(err.Error(), "llm client is required") {
		t.Fatalf("RegisterWith(skill_create_draft, no LLM client) error %q does not name the missing client", err)
	}
	if _, ok := cat.Resolve(drafter.ToolName); ok {
		t.Fatal("skill_create_draft resolved despite the loud registration failure")
	}
}

func TestRegisterSkillCreateDraft_RegistersAndInvokes(t *testing.T) {
	cat := tools.NewCatalog()
	store, err := inmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	client := scriptedClient{content: draftContentWithRequiredTools()}
	if err := RegisterSkillCreateDraft(RegistryContext{Catalog: cat, ArtifactStore: store}, client); err != nil {
		t.Fatalf("RegisterSkillCreateDraft: %v", err)
	}

	desc, ok := cat.Resolve(drafter.ToolName)
	if !ok {
		t.Fatal("tool did not resolve after explicit registration")
	}
	if desc.Tool.SideEffects != tools.SideEffectWrite {
		t.Fatalf("side effect = %q, want write", desc.Tool.SideEffects)
	}
	if desc.Tool.Loading != tools.LoadingAlways {
		t.Fatalf("loading = %q, want always", desc.Tool.Loading)
	}

	ctx, err := identity.WithRun(context.Background(), identity.Identity{
		TenantID: "t", UserID: "u", SessionID: "s",
	}, "run-1")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(SkillCreateDraftArgs{Intent: "draft a builtin test skill"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := desc.Invoke(ctx, raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, ok := result.Value.(SkillCreateDraftResult)
	if !ok {
		t.Fatalf("result type %T, want SkillCreateDraftResult", result.Value)
	}
	if out.Installed {
		t.Fatal("installed must be false")
	}
	if out.State != drafter.StateDraft || out.ArtifactRef == "" || !strings.HasPrefix(out.PackageHash, "v1:") {
		t.Fatalf("incomplete draft result: %+v", out)
	}

	// The artifact is stored under the invocation's caller scope.
	scope := artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "s"}
	_, found, err := store.Get(context.Background(), scope, out.ArtifactRef)
	if err != nil || !found {
		t.Fatalf("artifact not found under caller scope: found=%v err=%v", found, err)
	}
	// A foreign user cannot read it.
	foreign := artifacts.ArtifactScope{TenantID: "t", UserID: "other", SessionID: "s"}
	if _, found, err := store.Get(context.Background(), foreign, out.ArtifactRef); err != nil || found {
		t.Fatalf("artifact readable under a foreign user: found=%v err=%v", found, err)
	}
}

func TestRegisterSkillCreateDraft_ArgsRejectAuthorityFields(t *testing.T) {
	cat := tools.NewCatalog()
	store, _ := inmem.New(config.ArtifactsConfig{})
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	if err := RegisterSkillCreateDraft(RegistryContext{Catalog: cat, ArtifactStore: store}, scriptedClient{content: draftContentWithRequiredTools()}); err != nil {
		t.Fatal(err)
	}
	desc, _ := cat.Resolve(drafter.ToolName)

	ctx, _ := identity.WithRun(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "run-1")
	for _, args := range []string{
		`{"intent":"x","scope":"user"}`,
		`{"intent":"x","tenant":"other"}`,
		`{"intent":"x","user_id":"victim"}`,
		`{"intent":"x","agent_id":"a"}`,
		`{"intent":"x","persist":true}`,
		`{"intent":"x","publish":true}`,
		`{"intent":"x","grant":["tool.x"]}`,
		`{"intent":"x","bogus":1}`,
	} {
		_, err := desc.Invoke(ctx, json.RawMessage(args))
		if !errors.Is(err, tools.ErrToolInvalidArgs) {
			t.Fatalf("args %s: err = %v, want ErrToolInvalidArgs", args, err)
		}
	}
}

func TestRegisterSkillCreateDraft_UnavailableToolWarning(t *testing.T) {
	cat := tools.NewCatalog()
	// A visible tool in the run's set.
	if err := inproc.RegisterFunc[struct {
		In string `json:"in"`
	}, struct {
		Out string `json:"out"`
	}](
		cat, "example.tool",
		func(ctx context.Context, in struct {
			In string `json:"in"`
		}) (struct {
			Out string `json:"out"`
		}, error) {
			return struct {
				Out string `json:"out"`
			}{Out: "ok"}, nil
		},
		tools.WithDescription("example"),
	); err != nil {
		t.Fatal(err)
	}
	store, _ := inmem.New(config.ArtifactsConfig{})
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	// The draft declares one available and one absent required tool.
	client := scriptedClient{content: draftContentWithRequiredTools("example.tool", "absent.tool")}
	if err := RegisterSkillCreateDraft(RegistryContext{Catalog: cat, ArtifactStore: store}, client); err != nil {
		t.Fatal(err)
	}
	desc, _ := cat.Resolve(drafter.ToolName)
	ctx, _ := identity.WithRun(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "run-1")

	raw, _ := json.Marshal(SkillCreateDraftArgs{Intent: "i"})
	result, err := desc.Invoke(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	out := result.Value.(SkillCreateDraftResult)
	// The absent tool warns; the available tool does not.
	var warnedAbsent, warnedPresent bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "absent.tool") && strings.Contains(w, "metadata only") {
			warnedAbsent = true
		}
		if strings.Contains(w, "example.tool") {
			warnedPresent = true
		}
	}
	if !warnedAbsent {
		t.Fatalf("warnings = %v, want the absent-tool warning", out.Warnings)
	}
	if warnedPresent {
		t.Fatalf("warnings = %v, must not warn for an available tool", out.Warnings)
	}
}

func TestRegisterSkillCreateDraft_MissingStoreFailsAtInvoke(t *testing.T) {
	cat := tools.NewCatalog()
	if err := RegisterSkillCreateDraft(RegistryContext{Catalog: cat}, scriptedClient{content: draftContentWithRequiredTools()}); err != nil {
		t.Fatalf("registration without store must succeed (store-shaped dep fails at invoke): %v", err)
	}
	desc, _ := cat.Resolve(drafter.ToolName)
	ctx, _ := identity.WithRun(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "run-1")
	raw, _ := json.Marshal(SkillCreateDraftArgs{Intent: "i"})
	_, err := desc.Invoke(ctx, raw)
	if err == nil || !strings.Contains(err.Error(), "ArtifactStore is nil") {
		t.Fatalf("err = %v, want operator-misconfiguration store error", err)
	}
}

func TestRegisterSkillCreateDraft_NilClientFailsRegistration(t *testing.T) {
	cat := tools.NewCatalog()
	store, _ := inmem.New(config.ArtifactsConfig{})
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	err := RegisterSkillCreateDraft(RegistryContext{Catalog: cat, ArtifactStore: store}, nil)
	if err == nil || !strings.Contains(err.Error(), "llm client is required") {
		t.Fatalf("err = %v, want nil-client registration failure", err)
	}
	if _, ok := cat.Resolve(drafter.ToolName); ok {
		t.Fatal("tool must not resolve after a failed registration")
	}
}

func TestRegisterSkillCreateDraft_MissingIdentity(t *testing.T) {
	cat := tools.NewCatalog()
	store, _ := inmem.New(config.ArtifactsConfig{})
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	if err := RegisterSkillCreateDraft(RegistryContext{Catalog: cat, ArtifactStore: store}, scriptedClient{content: draftContentWithRequiredTools()}); err != nil {
		t.Fatal(err)
	}
	desc, _ := cat.Resolve(drafter.ToolName)
	raw, _ := json.Marshal(SkillCreateDraftArgs{Intent: "i"})
	_, err := desc.Invoke(context.Background(), raw)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("err = %v, want builtin.ErrIdentityRequired", err)
	}
}

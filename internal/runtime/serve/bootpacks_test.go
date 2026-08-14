package serve

// bootpacks_test.go — the serve-band HA-66 boot-baseline composition:
// the eager immutable index opener (with the granted-scopes ceiling
// enforcement), the resolved-agent validation wrapper, the pre-read
// durable collision check (under the canonical reserved tenant-agent
// control-plane identity), and the read-only composition preview's
// frozen boot contribution (index or the empty immutable seam). The
// loader itself (internal/skills/bootpacks) and the config validation
// are covered by their own phases; these tests pin the serve-band
// wiring posture: an empty declaration resolves NO baseline, nil seams
// are no-ops, the static catalog adapter answers presence + scope-
// ceiling (metadata only — never a grant), the pre-read reads the
// agent-scope active revision (never a synthetic real-user read), and
// boot config removal never 501s the preview.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	"github.com/hurtener/Harbor/internal/skills/importer"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestOpenBootPackIndex_EmptyDeclaration_NoBaseline pins the
// partial-build posture: no `skills.boot_agent_packs` declaration
// resolves (nil, nil) — no boot baseline bound, guards inert. The
// read-only composition preview separately resolves its EMPTY immutable
// boot contribution (see TestPreviewBootReader_EmptySeam).
func TestOpenBootPackIndex_EmptyDeclaration_NoBaseline(t *testing.T) {
	idx, err := OpenBootPackIndex(context.Background(), &config.Config{}, tools.NewCatalog(), nil)
	if err != nil {
		t.Fatalf("OpenBootPackIndex (empty): %v", err)
	}
	if idx != nil {
		t.Fatal("OpenBootPackIndex (empty) returned a non-nil index")
	}
}

// TestValidateBootAgentPacksForAgent_Delegation pins the serve wrapper
// over the config package's pure helper: no declarations pass, a
// declaration naming a different agent fails loud (the resolved
// boot/default agent is the ONLY admissible agent id).
func TestValidateBootAgentPacksForAgent_Delegation(t *testing.T) {
	if err := ValidateBootAgentPacksForAgent(&config.Config{}, "boot-agent"); err != nil {
		t.Fatalf("ValidateBootAgentPacksForAgent (no declarations): %v", err)
	}
	cfg := &config.Config{Skills: config.SkillsConfig{BootAgentPacks: []config.BootAgentPackConfig{
		{TenantID: "t1", AgentID: "other-agent", Directory: "/tmp/boot"},
	}}}
	if err := ValidateBootAgentPacksForAgent(cfg, "boot-agent"); err == nil {
		t.Fatal("ValidateBootAgentPacksForAgent with a mismatched agent must fail loud")
	}
	if err := ValidateBootAgentPacksForAgent(cfg, "other-agent"); err != nil {
		t.Fatalf("ValidateBootAgentPacksForAgent with the exact agent: %v", err)
	}
}

// TestPreReadBootPackCollisions_NilSafe pins the nil posture: no index
// or no registry is a no-op (an absent boot key / absent active
// revision passes).
func TestPreReadBootPackCollisions_NilSafe(t *testing.T) {
	ctx := context.Background()
	if err := PreReadBootPackCollisions(ctx, nil, nil); err != nil {
		t.Fatalf("PreReadBootPackCollisions (nil index): %v", err)
	}
}

// cannedBootParser is a bootpacks.Parser stub that returns ONE canned
// ingest regardless of the markdown bytes — the serve-band tests pin
// the serve composition, not the pure parser (which its own phase
// covers), so the filesystem SKILL.md content is irrelevant.
type cannedBootParser struct{ ingest importer.PackageIngest }

func (p cannedBootParser) ImportPackageMarkdown(context.Context, importer.PackageMarkdownSource) (importer.PackageIngest, error) {
	return p.ingest, nil
}

// bootSkillFor builds a valid stored-form boot skill whose ContentHash
// is pinned to its canonical content hash (the loader freezes that
// value as the entry's SemanticHash; the strict composer re-validates
// it).
func bootSkillFor(name, trigger, step string) skills.Skill {
	s := skills.Skill{
		Name:     name,
		Title:    name + " title",
		Trigger:  trigger,
		Steps:    []string{step},
		Origin:   skills.OriginPack,
		Scope:    skills.ScopeProject,
		TaskType: "domain",
	}
	s.ContentHash = skills.CanonicalContentHash(s)
	return s
}

// buildTestBootIndex loads ONE (t1, agent-x) boot declaration over a
// temp pack directory through the REAL eager loader with a canned
// parser, returning the frozen index.
func buildTestBootIndex(t *testing.T, skill skills.Skill) *bootpacks.Index {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+skill.Name+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}
	idx, err := bootpacks.New(context.Background(), []config.BootAgentPackConfig{{
		TenantID: "t1", AgentID: "agent-x", Directory: root, Include: []string{"pkg"},
	}}, bootpacks.Deps{
		Parser:  cannedBootParser{ingest: importer.PackageIngest{Skill: skill, Hash: "v1:test"}},
		Catalog: bootCatalogAdapter{cat: tools.NewCatalog(), grantedScopes: nil},
	})
	if err != nil {
		t.Fatalf("bootpacks.New: %v", err)
	}
	return idx
}

// TestPreReadBootPackCollisions_UsesAgentScopeIdentity is the P1
// regression for the readiness pre-read: the durable active revision
// is read under the CANONICAL reserved tenant-agent control-plane
// identity (agentcfg.AgentScope — the (tenant, __agentcfg__, agent)
// slot the agent-level desired state persists under), never a
// bootUser / default / MCP / dev-user parameter. A caller-supplied or
// defaulted real-user value cannot choose (or miss) the active
// agent-scope revision — there is no such parameter anymore, and the
// pre-read finds the revision the agent scope owns. The old synthetic
// real-user shape (an empty-session triple) failed the real registry's
// mandatory identity check; this regression pins the canonical read.
func TestPreReadBootPackCollisions_UsesAgentScopeIdentity(t *testing.T) {
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(ctx) })

	// (a) The durable active revision is persisted under the canonical
	//     reserved tenant-agent control-plane identity — the SAME slot
	//     the pre-read must read. A conflicting name/content pair is
	//     seeded so the pre-read's collision check MUST fire.
	scopeQ, err := agentcfg.AgentScope("t1", "agent-x")
	if err != nil {
		t.Fatalf("AgentScope: %v", err)
	}
	if _, err := reg.SetRevision(ctx, scopeQ, "agent-x", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{{
			Name: "playbook", Title: "revision playbook", Trigger: "revision trigger", Steps: []string{"revision step"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision (agent scope): %v", err)
	}

	// The boot baseline declares the SAME canonical name with DIFFERENT
	// content → the strict composer's conflict must surface at pre-read.
	// If the pre-read read under any synthetic real-user identity (the
	// old bootUser parameter) it would find NO revision — or fail the
	// registry's mandatory identity check — and the collision would be
	// missed.
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	retirementReg, ok := reg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatal("fixture: the real agentcfg registry must implement the retirement/read seam")
	}
	err = PreReadBootPackCollisions(ctx, idx, retirementReg)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("PreReadBootPackCollisions with a conflicting agent-scope revision: got %v, want a typed composition conflict", err)
	}

	// (b) Control: a NON-conflicting agent-scope revision passes the
	//     pre-read (the collision is content-keyed, not identity-keyed).
	if _, err := reg.SetRevision(ctx, scopeQ, "agent-x", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{{
			Name: "other-pack", Title: "other", Trigger: "other trigger", Steps: []string{"other step"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision (non-conflicting): %v", err)
	}
	if err := PreReadBootPackCollisions(ctx, idx, retirementReg); err != nil {
		t.Fatalf("PreReadBootPackCollisions with a non-conflicting agent-scope revision: %v", err)
	}
}

// TestBootCatalogAdapter_Compatible pins the loader's static
// compatibility reader: presence in the wrapped catalog answers true;
// an unknown name or a nil catalog answers false; and — the granted-
// scopes ceiling — a tool whose descriptor declares an auth scope
// OUTSIDE the configured `tools.granted_scopes` ceiling is NOT
// satisfiable (required_tools is metadata-only and grants nothing).
func TestBootCatalogAdapter_Compatible(t *testing.T) {
	cat := tools.NewCatalog()
	register := func(name string, scopes []string) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: name, Transport: tools.TransportInProcess, SideEffects: tools.SideEffectPure, Loading: tools.LoadingAlways, AuthScopes: scopes},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	register("clock.now", nil)
	register("shell.run", []string{"shell.ops"})

	// No declared scopes → always satisfiable when present.
	adapter := bootCatalogAdapter{cat: cat, grantedScopes: []string{"shell.ops"}}
	if !adapter.Compatible("clock.now") {
		t.Error("Compatible(clock.now) = false, want true (registered in the wrapped catalog)")
	}
	if adapter.Compatible("no.such.tool") {
		t.Error("Compatible(no.such.tool) = true, want false")
	}
	if (bootCatalogAdapter{cat: nil}).Compatible("clock.now") {
		t.Error("Compatible with nil catalog = true, want false")
	}

	// Positive: every declared auth scope is inside the ceiling.
	if !adapter.Compatible("shell.run") {
		t.Error("Compatible(shell.run) = false, want true (declared scope shell.ops is within the granted ceiling)")
	}

	// Negative: a declared auth scope OUTSIDE the ceiling is
	// unsatisfiable — the boot baseline never grants a scope the
	// operator's ceiling denies.
	denied := bootCatalogAdapter{cat: cat, grantedScopes: []string{"other.scope"}}
	if denied.Compatible("shell.run") {
		t.Error("Compatible(shell.run) = true with a ceiling that lacks shell.ops, want false")
	}
	// Negative: an empty ceiling grants nothing — any declared scope is
	// unsatisfiable.
	empty := bootCatalogAdapter{cat: cat, grantedScopes: nil}
	if empty.Compatible("shell.run") {
		t.Error("Compatible(shell.run) = true with an empty granted ceiling, want false (required metadata grants nothing)")
	}
}

// TestOpenBootPackIndex_GrantedScopesCeiling is the loader-level
// positive/negative pair for P1: a required_tools entry whose tool
// descriptor's auth scopes are inside the configured
// `tools.granted_scopes` ceiling loads; the same entry with a ceiling
// that lacks the scope fails the boot LOUD with the typed
// ErrRequiredTool — the loader never grants a scope the ceiling denies.
func TestOpenBootPackIndex_GrantedScopesCeiling(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	skillMD := `---
name: runbook
title: Runbook
trigger: when asked about the runbook
task_type: domain
required_tools: [shell.run]
---
Runbook body.

## Steps
- do the thing
- verify the thing
`
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}
	cfg := &config.Config{Skills: config.SkillsConfig{BootAgentPacks: []config.BootAgentPackConfig{
		{TenantID: "t1", AgentID: "agent-x", Directory: root, Include: []string{"pkg"}},
	}}}
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "shell.run", Transport: tools.TransportInProcess, SideEffects: tools.SideEffectRead, Loading: tools.LoadingAlways, AuthScopes: []string{"shell.ops"}},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register shell.run: %v", err)
	}

	// Positive: the ceiling contains the tool's declared auth scope.
	cfg.Tools.GrantedScopes = []string{"shell.ops", "readonly"}
	artStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(ctx) })
	idx, err := OpenBootPackIndex(ctx, cfg, cat, artStore)
	if err != nil {
		t.Fatalf("OpenBootPackIndex (scope in ceiling): %v", err)
	}
	if idx == nil {
		t.Fatal("OpenBootPackIndex (scope in ceiling) returned a nil index")
	}

	// Negative: the ceiling lacks the tool's declared auth scope — the
	// load fails LOUD with the typed ErrRequiredTool (required_tools
	// grants nothing beyond the configured ceiling).
	cfg.Tools.GrantedScopes = []string{"readonly"}
	if _, err := OpenBootPackIndex(ctx, cfg, cat, artStore); !errors.Is(err, bootpacks.ErrRequiredTool) {
		t.Fatalf("OpenBootPackIndex (scope outside ceiling) = %v, want ErrRequiredTool", err)
	}
}

// TestPreviewBootReader_EmptySeam pins the P1 read-only preview path:
// with NO boot baseline the shared helper resolves the EMPTY immutable
// reader (every lookup answers an empty boot contribution), so boot
// config removal never 501s the preview and an independently persisted
// active revision composes as provenance "revision"; with a baseline
// the helper returns the frozen index unchanged (the boot+revision
// collision defense is untouched — a real index with a removed key
// keeps its non-oracular unavailable outcome).
func TestPreviewBootReader_EmptySeam(t *testing.T) {
	// No baseline → the narrow empty reader, never nil (a nil reader
	// would fail the preview service construction with
	// ErrPreviewMisconfigured).
	reader := PreviewBootReader(nil)
	if reader == nil {
		t.Fatal("PreviewBootReader(nil) returned nil — the preview seam must stay constructible without a baseline")
	}
	entries, ok := reader.Lookup("t1", "agent-x")
	if !ok || len(entries) != 0 {
		t.Fatalf("empty reader Lookup = (%v, %v), want (nil, true) — an empty immutable boot contribution", entries, ok)
	}
	if _, ok := reader.Lookup("other-tenant", "other-agent"); !ok {
		t.Fatal("empty reader Lookup must answer true for EVERY (tenant, agent) — no boot key is a tombstone or an oracle")
	}

	// A baseline → the frozen index itself is the reader (byte-identical
	// behavior to the eager index; the empty seam never replaces it).
	idx := buildTestBootIndex(t, bootSkillFor("playbook", "boot trigger", "boot step"))
	if PreviewBootReader(idx) != agentcfgprotocol.BootPackReader(idx) {
		t.Fatal("PreviewBootReader(index) must return the index itself")
	}
}

// artifactStoreSpy is the narrowest failing spy for the HA-66 resource-free
// loader: it EMBEDS the full artifacts.ArtifactStore interface with no
// concrete implementation, so ANY accidental method call panics with a nil
// pointer dereference on the embedded interface. A loader that touched the
// store even once fails the test loudly instead of silently passing against
// a permissive fake.
type artifactStoreSpy struct {
	artifacts.ArtifactStore
}

// TestOpenBootPackIndex_ResourceFreeLoader_ZeroArtifactStoreCalls is the
// P1 regression for the HA-66 resource-free loader: OpenBootPackIndex
// drives a valid resource-free SKILL.md through the REAL importer over the
// failing spy and must succeed — proving the eager boot baseline makes ZERO
// ArtifactStore calls even though `importer.New` mechanically requires the
// dependency (the loader is a pure eager filesystem read + parse +
// validate, never a store writer).
func TestOpenBootPackIndex_ResourceFreeLoader_ZeroArtifactStoreCalls(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	skillMD := `---
name: runbook
title: Runbook
trigger: when asked about the runbook
task_type: domain
---
Runbook body.

## Steps
- do the thing
- verify the thing
`
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}
	cfg := &config.Config{Skills: config.SkillsConfig{BootAgentPacks: []config.BootAgentPackConfig{
		{TenantID: "t1", AgentID: "agent-x", Directory: root, Include: []string{"pkg"}},
	}}}
	idx, err := OpenBootPackIndex(ctx, cfg, tools.NewCatalog(), artifactStoreSpy{})
	if err != nil {
		t.Fatalf("OpenBootPackIndex over the failing spy: %v", err)
	}
	if idx == nil {
		t.Fatal("OpenBootPackIndex over the failing spy returned a nil index")
	}
}

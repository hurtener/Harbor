package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

// TestBuildMux_AgentPacks_RealStateStoreMatrix drives the shipped Agent-pack
// Protocol routes through the real BuildMux and real StateStore-backed
// Registry. It intentionally uses httptest.ResponseRecorder rather than a
// listener: the route, body-scope, admin, reach, resolver, runtime service,
// and driver seams are still exercised without making the test depend on a
// sandbox permitting sockets.
func TestBuildMux_AgentPacks_RealStateStoreMatrix(t *testing.T) {
	registered := state.RegisteredDrivers()
	for _, want := range []string{"inmem", "sqlite"} {
		found := false
		for _, got := range registered {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("required StateStore driver %q is not registered: %v", want, registered)
		}
	}

	drivers := []struct {
		name string
		cfg  func(*testing.T) config.StateConfig
	}{
		{name: "inmem", cfg: func(*testing.T) config.StateConfig {
			return config.StateConfig{Driver: "inmem"}
		}},
		{name: "sqlite", cfg: func(t *testing.T) config.StateConfig {
			return config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "agent-packs.sqlite")}
		}},
	}
	for _, driver := range drivers {
		t.Run(driver.name, func(t *testing.T) {
			t.Parallel()
			phase267RunAgentPackDriver(t, driver.name, driver.cfg(t))
		})
	}
}

func phase267RunAgentPackDriver(t *testing.T, driverName string, cfg config.StateConfig) {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("state.Open(%s): %v", driverName, err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	deps := buildProjWiringMux(t)
	registry, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: store, Bus: deps.in.Bus})
	if err != nil {
		t.Fatalf("agentcfg.Open(%s): %v", driverName, err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })

	id := identity.Identity{TenantID: "tenant-phase267-" + driverName, UserID: "operator", SessionID: "session-" + driverName}
	sourceID := "source-" + driverName
	targetID := "target-" + driverName
	emptyTargetID := "empty-target-" + driverName
	alpha := skills.AgentPackItem{
		Name: "alpha", Title: "Alpha", Trigger: "trigger-alpha", Steps: []string{"alpha-body"},
		RequiredTools: []string{"tool-not-granted"},
	}
	beta := skills.AgentPackItem{
		Name: "beta", Title: "Beta", Trigger: "trigger-beta", Steps: []string{"beta-body"},
	}
	keep := skills.AgentPackItem{
		Name: "keep", Title: "Keep", Trigger: "trigger-keep", Steps: []string{"keep-body"},
		OriginRef: "operator-authored",
	}
	phase267SetAgentRevision(t, registry, id, sourceID, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{alpha, beta},
	})
	targetRevision := phase267SetAgentRevision(t, registry, id, targetID, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{keep},
	})
	phase267SetAgentRevision(t, registry, id, emptyTargetID, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"alpha", "beta"}},
	})

	currentResolver := NewAgentResolverAdapter(registry, "")
	in := deps.in
	in.State = store
	in.AgentConfig = registry
	in.AgentConfigID = ""
	in.AgentResolver = currentResolver
	in.AgentReach = auth.NewAgentReachAuthorizer()
	in.Cfg.Tools.GrantedScopes = []string{"phase267:tool:granted"}

	// Assemble the actual catalog and capability carrier used by production:
	// the copied Agent-pack metadata names a tool that is registered but not
	// granted, while a second scoped tool is granted by the server config.
	// The invocation counters make an unauthorized declarative dispatch an
	// observable non-invocation rather than a boolean-only assertion.
	var deniedInvocations atomic.Int64
	var grantedInvocations atomic.Int64
	for _, fixture := range []struct {
		name      string
		authScope string
		invoked   *atomic.Int64
	}{
		{name: "tool-not-granted", authScope: "phase267:tool:secret", invoked: &deniedInvocations},
		{name: "tool-granted", authScope: "phase267:tool:granted", invoked: &grantedInvocations},
	} {
		if err := in.Catalog.Register(tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:        fixture.name,
				Description: "Phase 267 capability fixture",
				AuthScopes:  []string{fixture.authScope},
				Loading:     tools.LoadingAlways,
				SideEffects: tools.SideEffectRead,
			},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				fixture.invoked.Add(1)
				return tools.ToolResult{Value: fixture.name}, nil
			},
		}); err != nil {
			t.Fatalf("catalog.Register(%s): %v", fixture.name, err)
		}
	}

	// This is the real SkillStore carrier used by the built-in
	// skill_search/skill_get handlers. The rows intentionally mirror the
	// copied RequiredTools body: that metadata must be filtered against the
	// server-computed capability envelope, never treated as a grant.
	skillStore, err := skills.Open(ctx, skills.ConfigSnapshot{
		Driver: "localdb",
		DSN:    filepath.Join(t.TempDir(), "phase267-skills.sqlite"),
	}, skills.Deps{Bus: deps.in.Bus})
	if err != nil {
		t.Fatalf("skills.Open(%s): %v", driverName, err)
	}
	t.Cleanup(func() { _ = skillStore.Close(context.Background()) })
	for _, candidate := range []skills.Skill{
		{
			Name:          "copied-alpha",
			Title:         "Copied alpha metadata",
			Trigger:       "phase267 copied capability",
			Steps:         []string{"run copied alpha"},
			RequiredTools: append([]string(nil), alpha.RequiredTools...),
			Origin:        skills.OriginPack,
			Scope:         skills.ScopeProject,
		},
		{
			Name:          "granted-beta",
			Title:         "Granted beta metadata",
			Trigger:       "phase267 granted capability",
			Steps:         []string{"run granted beta"},
			RequiredTools: []string{"tool-granted"},
			Origin:        skills.OriginPack,
			Scope:         skills.ScopeProject,
		},
		{
			Name:    "unrestricted",
			Title:   "Unrestricted capability",
			Trigger: "phase267 unrestricted capability",
			Steps:   []string{"run unrestricted"},
			Origin:  skills.OriginPack,
			Scope:   skills.ScopeProject,
		},
	} {
		if err := skillStore.Upsert(ctx, identity.Quadruple{Identity: id}, candidate); err != nil {
			t.Fatalf("skills.Upsert(%s): %v", candidate.Name, err)
		}
	}
	if err := builtin.RegisterWith(builtin.RegistryContext{
		Catalog:       in.Catalog,
		SkillStore:    skillStore,
		Bus:           deps.in.Bus,
		GrantedScopes: append([]string(nil), in.Cfg.Tools.GrantedScopes...),
	}, []string{"skill_search", "skill_get", "declarative_action"}); err != nil {
		t.Fatalf("builtin.RegisterWith(%s): %v", driverName, err)
	}
	// These handles belong to the helper's unrelated projection fixtures. A
	// nil value keeps this test focused on the real agent-pack route while the
	// ControlSurface and BuildMux assembly remain the production ones.
	in.Tasks = nil
	in.Sessions = nil
	in.Skills = skillStore
	built, err := BuildMux(in)
	if err != nil {
		t.Fatalf("BuildMux(%s): %v", driverName, err)
	}

	adminReach := func() context.Context {
		return phase267AgentPackContext(t, id, []string{sourceID, targetID, emptyTargetID}, true)
	}
	inspect := func(agentID string, requestCtx context.Context) prototypes.AgentConfigAgentPacksInspectResponse {
		t.Helper()
		body := phase267Marshal(t, prototypes.AgentConfigAgentPacksInspectRequest{
			Identity: phase267IdentityScope(id), AgentID: agentID,
		})
		status, raw := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/inspect", id, body, requestCtx)
		if status != http.StatusOK {
			t.Fatalf("inspect %s status=%d body=%s", agentID, status, raw)
		}
		var response prototypes.AgentConfigAgentPacksInspectResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatalf("decode inspect %s: %v; body=%s", agentID, err, raw)
		}
		return response
	}

	sourceView := inspect(sourceID, adminReach())
	targetInitial := inspect(targetID, adminReach())
	emptyTargetInitial := inspect(emptyTargetID, adminReach())
	if len(sourceView.EffectivePacks) != 2 || sourceView.EffectivePacks[0].Pack.Name == "" {
		t.Fatalf("source inspection did not return complete pack bodies: %+v", sourceView)
	}
	if len(targetInitial.EffectivePacks) != 1 || targetInitial.EffectivePacks[0].Pack.Name != "keep" {
		t.Fatalf("target inspection = %+v", targetInitial)
	}
	if sourceView.CompositionHash == "" || targetInitial.CompositionHash == "" {
		t.Fatal("inspection omitted source or target composition hash")
	}
	if len(emptyTargetInitial.EffectivePacks) != 0 || emptyTargetInitial.CompositionHash == "" {
		t.Fatalf("empty target inspection = %+v", emptyTargetInitial)
	}

	// A first-time derived Agent has a real active revision but zero packs.
	// Its inspection uses Harbor's canonical non-empty wire sentinel for the
	// empty composition. That sentinel must survive the wire adapter as an
	// explicit expected-empty CAS rather than collapsing to a missing value.
	{
		emptyTargetCopy := prototypes.AgentConfigAgentPacksCopyRequest{
			Identity: phase267IdentityScope(id), SourceAgentID: sourceID, TargetAgentID: emptyTargetID,
			PackIDs:                       []string{"alpha", "beta"},
			ExpectedSourceCompositionHash: sourceView.CompositionHash,
			ExpectedTargetCompositionHash: emptyTargetInitial.CompositionHash,
			IdempotencyKey:                "phase267-empty-target-" + driverName,
		}
		status, raw := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
			phase267Marshal(t, emptyTargetCopy), adminReach())
		if status != http.StatusOK {
			t.Fatalf("empty-target copy status=%d body=%s", status, raw)
		}
		status, replayRaw := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
			phase267Marshal(t, emptyTargetCopy), adminReach())
		if status != http.StatusOK || string(replayRaw) != string(raw) {
			t.Fatalf("empty-target replay status=%d first=%s replay=%s", status, raw, replayRaw)
		}
		emptyTargetAfter := inspect(emptyTargetID, adminReach())
		if len(emptyTargetAfter.EffectivePacks) != 2 {
			t.Fatalf("empty-target copy pack count=%d, want 2", len(emptyTargetAfter.EffectivePacks))
		}
		staleEmptyTarget := emptyTargetCopy
		staleEmptyTarget.IdempotencyKey += "-stale"
		status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
			phase267Marshal(t, staleEmptyTarget), adminReach())
		if status != http.StatusConflict || phase267ErrorCode(t, raw) != protoerrors.CodeRevisionConflict {
			t.Fatalf("stale empty-target CAS status=%d code=%q body=%s", status, phase267ErrorCode(t, raw), raw)
		}
		malformedEmptyTarget := emptyTargetCopy
		malformedEmptyTarget.ExpectedTargetCompositionHash = "not-a-canonical-hash"
		malformedEmptyTarget.IdempotencyKey += "-malformed"
		status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
			phase267Marshal(t, malformedEmptyTarget), adminReach())
		if status != http.StatusBadRequest || phase267ErrorCode(t, raw) != protoerrors.CodeInvalidRequest {
			t.Fatalf("malformed empty-target CAS status=%d code=%q body=%s", status, phase267ErrorCode(t, raw), raw)
		}
		if afterDenied := inspect(emptyTargetID, adminReach()); afterDenied.CompositionHash != emptyTargetAfter.CompositionHash {
			t.Fatalf("denied empty-target requests changed target: before=%+v after=%+v", emptyTargetAfter, afterDenied)
		}

		// The copied bodies must immediately back the target's run-start skill
		// snapshot. They live in the target's active AgentPacks revision, not in
		// the base SkillStore, while Skills pins their names. This is the exact
		// derived-Agent shape that regressed live in v1.31.2.
		runQ := identity.Quadruple{Identity: id, RunID: "phase267-copied-run-" + driverName}
		runDriver, _ := newRunSnapshotDriver(t, registry, skillStore, store)
		snapshot, ok, snapshotErr := runDriver.captureRunSkillSnapshot(t.Context(), emptyTargetID, runQ, nil)
		if snapshotErr != nil || !ok {
			t.Fatalf("capture copied target snapshot: ok=%t err=%v", ok, snapshotErr)
		}
		reader, resolveErr := skills.ResolveSkillReader(withRunSnapshot(t, runQ, snapshot), runQ, skillStore)
		if resolveErr != nil {
			t.Fatalf("resolve copied target reader: %v", resolveErr)
		}
		resolvedAlpha, resolveErr := reader.Get(t.Context(), runQ, "alpha")
		if resolveErr != nil || len(resolvedAlpha.Steps) != 1 || resolvedAlpha.Steps[0] != "alpha-body" {
			t.Fatalf("resolved copied alpha = (%+v, %v)", resolvedAlpha, resolveErr)
		}

		// Execute the production skill_get handler against the immutable target
		// snapshot with no allowed tools. The unrestricted copied beta body is
		// returned, while alpha's copied RequiredTools metadata cannot grant or
		// expose tool-not-granted.
		getOut, getErr := skilltools.GetHandler(withRunSnapshot(t, runQ, snapshot), skillStore, deps.in.Bus, skilltools.GetArgs{
			Names: []string{"alpha", "beta"}, MaxTokens: 4096,
		})
		if getErr != nil {
			t.Fatalf("skill_get copied target: %v", getErr)
		}
		if phase267HasSkillValue(getOut.Skills, "alpha") || !phase267HasSkillValue(getOut.Skills, "beta") {
			t.Fatalf("copied target capability projection = %+v", getOut.Skills)
		}
		if deniedInvocations.Load() != 0 {
			t.Fatalf("copied RequiredTools metadata invoked ungranted tool %d times", deniedInvocations.Load())
		}
	}

	// The server's configured grant is applied through the same real catalog
	// visibility predicate used by runCapability. It admits tool-granted but
	// not tool-not-granted, proving this is a scope decision rather than an
	// artifact-body or test-only boolean.
	visible := in.Catalog.List(tools.CatalogFilter{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		GrantedScopes: in.Cfg.Tools.GrantedScopes,
		LoadingModes:  []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	})
	if !phase267HasTool(visible, "tool-granted") || phase267HasTool(visible, "tool-not-granted") {
		t.Fatalf("server-granted catalog visibility = %v, want granted tool only", phase267ToolNames(visible))
	}

	// A competing real Registry write advances the target after inspection.
	// The stale Protocol copy must return 409 and leave the competitor as the
	// only target item: no selected pack may be partially published.
	competitor := keep
	competitor.Steps = []string{"competitor-body"}
	phase267SetAgentRevision(t, registry, id, targetID, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{competitor},
	}, agentcfg.SetOptions{ExpectedContentHash: targetRevision.ContentHash})
	staleCopy := prototypes.AgentConfigAgentPacksCopyRequest{
		Identity: phase267IdentityScope(id), SourceAgentID: sourceID, TargetAgentID: targetID,
		PackIDs:                       []string{"alpha", "beta"},
		ExpectedSourceCompositionHash: sourceView.CompositionHash,
		ExpectedTargetCompositionHash: targetInitial.CompositionHash,
		IdempotencyKey:                "phase267-stale-" + driverName,
	}
	status, raw := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
		phase267Marshal(t, staleCopy), adminReach())
	if status != http.StatusConflict || phase267ErrorCode(t, raw) != protoerrors.CodeRevisionConflict {
		t.Fatalf("stale copy status=%d code=%q body=%s", status, phase267ErrorCode(t, raw), raw)
	}
	staleTarget := inspect(targetID, adminReach())
	if len(staleTarget.EffectivePacks) != 1 || staleTarget.EffectivePacks[0].Pack.Name != "keep" || staleTarget.EffectivePacks[0].Pack.Steps[0] != "competitor-body" {
		t.Fatalf("stale CAS partially changed target: %+v", staleTarget)
	}

	// Re-read the competing target and perform the real successful copy. The
	// source's RequiredTools value is deliberately not granted in cfg: it is
	// copied as metadata, never treated as authority to widen capabilities.
	successCopy := staleCopy
	successCopy.ExpectedTargetCompositionHash = staleTarget.CompositionHash
	successCopy.IdempotencyKey = "phase267-success-" + driverName
	status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
		phase267Marshal(t, successCopy), adminReach())
	if status != http.StatusOK {
		t.Fatalf("successful copy status=%d body=%s", status, raw)
	}
	var copyResponse prototypes.AgentConfigAgentPacksCopyResponse
	if err := json.Unmarshal(raw, &copyResponse); err != nil {
		t.Fatalf("decode successful copy: %v; body=%s", err, raw)
	}
	if len(copyResponse.Outcomes) != 2 || copyResponse.Outcomes[0].Outcome != "copied" || copyResponse.Outcomes[1].Outcome != "copied" {
		t.Fatalf("successful copy outcomes = %+v", copyResponse.Outcomes)
	}
	var responseFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &responseFields); err != nil {
		t.Fatalf("decode copy response fields: %v", err)
	}
	for _, forbidden := range []string{"pack", "item", "origin_ref", "steps"} {
		if _, ok := responseFields[forbidden]; ok {
			t.Fatalf("copy response exposed body field %q: %s", forbidden, raw)
		}
	}

	// The exact request is a durable retry. It returns the same terminal
	// response and does not advance target state a second time.
	status, replayRaw := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
		phase267Marshal(t, successCopy), adminReach())
	if status != http.StatusOK {
		t.Fatalf("copy replay status=%d body=%s", status, replayRaw)
	}
	var replayResponse prototypes.AgentConfigAgentPacksCopyResponse
	if err := json.Unmarshal(replayRaw, &replayResponse); err != nil {
		t.Fatalf("decode copy replay: %v; body=%s", err, replayRaw)
	}
	if replayResponse.CompositionHash != copyResponse.CompositionHash || len(replayResponse.Outcomes) != len(copyResponse.Outcomes) {
		t.Fatalf("copy replay advanced or changed result: first=%+v replay=%+v", copyResponse, replayResponse)
	}
	targetAfterCopy := inspect(targetID, adminReach())
	if len(targetAfterCopy.EffectivePacks) != 3 {
		t.Fatalf("successful target effective pack count=%d, want competitor+alpha+beta", len(targetAfterCopy.EffectivePacks))
	}
	if got := phase267FindPack(targetAfterCopy.EffectivePacks, "alpha"); got == nil || len(got.Pack.RequiredTools) != 1 || got.Pack.RequiredTools[0] != "tool-not-granted" {
		t.Fatalf("required-tool metadata was not preserved as non-authorizing data: %+v", targetAfterCopy)
	}

	// Exercise the production builtin carrier. The capability envelope is
	// computed inside internal/tools/builtin/skill_capability.go from the
	// real catalog + server-granted scopes; the caller cannot add a
	// `capability` field. Both search and get must hide the skill whose
	// RequiredTools came from the copied body, while retaining the granted
	// and unrestricted rows.
	runCtx, err := identity.WithRun(ctx, id, "phase267-capability-"+driverName)
	if err != nil {
		t.Fatalf("identity.WithRun(%s): %v", driverName, err)
	}
	searchDesc, ok := in.Catalog.Resolve("skill_search")
	if !ok {
		t.Fatal("skill_search was not registered")
	}
	searchResult, err := searchDesc.Invoke(runCtx, json.RawMessage(phase267Marshal(t, builtin.SkillSearchArgs{Query: "phase267", Limit: 10})))
	if err != nil {
		t.Fatalf("skill_search invoke: %v", err)
	}
	searchOut, ok := searchResult.Value.(skilltools.SearchResult)
	if !ok {
		t.Fatalf("skill_search result type=%T, want %T", searchResult.Value, skilltools.SearchResult{})
	}
	if phase267HasSkill(searchOut.Skills, "copied-alpha") || !phase267HasSkill(searchOut.Skills, "granted-beta") || !phase267HasSkill(searchOut.Skills, "unrestricted") {
		t.Fatalf("skill_search capability projection = %+v", searchOut.Skills)
	}

	getDesc, ok := in.Catalog.Resolve("skill_get")
	if !ok {
		t.Fatal("skill_get was not registered")
	}
	getResult, err := getDesc.Invoke(runCtx, json.RawMessage(phase267Marshal(t, builtin.SkillGetArgs{
		Names: []string{"copied-alpha", "granted-beta", "unrestricted"}, MaxTokens: 4096,
	})))
	if err != nil {
		t.Fatalf("skill_get invoke: %v", err)
	}
	getOut, ok := getResult.Value.(skilltools.GetResult)
	if !ok {
		t.Fatalf("skill_get result type=%T, want %T", getResult.Value, skilltools.GetResult{})
	}
	if phase267HasSkillValue(getOut.Skills, "copied-alpha") || !phase267HasSkillValue(getOut.Skills, "granted-beta") || !phase267HasSkillValue(getOut.Skills, "unrestricted") {
		t.Fatalf("skill_get capability projection = %+v", getOut.Skills)
	}

	// Finally exercise the real declared-name dispatch gate. A remembered or
	// copied body name cannot invoke the ungranted descriptor; the granted
	// descriptor does invoke and increments only its own counter.
	declarativeDesc, ok := in.Catalog.Resolve("declarative_action")
	if !ok {
		t.Fatal("declarative_action was not registered")
	}
	deniedResult, err := declarativeDesc.Invoke(runCtx, json.RawMessage(phase267Marshal(t, builtin.DeclarativeActionArgs{Tool: "tool-not-granted"})))
	if err != nil {
		t.Fatalf("declarative_action denied invoke: %v", err)
	}
	deniedOut, ok := deniedResult.Value.(builtin.DeclarativeActionOut)
	if !ok {
		t.Fatalf("declarative_action denied result type=%T, want %T", deniedResult.Value, builtin.DeclarativeActionOut{})
	}
	if deniedOut.Dispatched || deniedInvocations.Load() != 0 {
		t.Fatalf("ungranted tool dispatched=%t invokes=%d; copied metadata widened authority", deniedOut.Dispatched, deniedInvocations.Load())
	}
	grantedResult, err := declarativeDesc.Invoke(runCtx, json.RawMessage(phase267Marshal(t, builtin.DeclarativeActionArgs{Tool: "tool-granted"})))
	if err != nil {
		t.Fatalf("declarative_action granted invoke: %v", err)
	}
	grantedOut, ok := grantedResult.Value.(builtin.DeclarativeActionOut)
	if !ok || !grantedOut.Dispatched || grantedInvocations.Load() != 1 {
		t.Fatalf("granted tool dispatch=%+v invokes=%d, want dispatched once", grantedOut, grantedInvocations.Load())
	}

	// Reach is checked for each addressed agent before the Registry resolver or
	// runtime port. Neither one-sided reach claim may write the target.
	for _, tc := range []struct {
		name  string
		reach []string
	}{
		{name: "source-only", reach: []string{sourceID}},
		{name: "target-only", reach: []string{targetID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := successCopy
			request.IdempotencyKey = "phase267-denied-" + driverName + "-" + tc.name
			status, raw := postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
				phase267Marshal(t, request), phase267AgentPackContext(t, id, tc.reach, true))
			if status != http.StatusForbidden || phase267ErrorCode(t, raw) != protoerrors.CodeScopeMismatch {
				t.Fatalf("reach=%v status=%d code=%q body=%s", tc.reach, status, phase267ErrorCode(t, raw), raw)
			}
			after := inspect(targetID, adminReach())
			if after.CompositionHash != targetAfterCopy.CompositionHash || len(after.EffectivePacks) != len(targetAfterCopy.EffectivePacks) {
				t.Fatalf("denied %s reach changed target: before=%+v after=%+v", tc.name, targetAfterCopy, after)
			}
		})
	}

	// A forged body identity is rejected by the transport body-scope gate
	// before the Agent-pack surface or Registry sees agent/source data.
	forged := id
	forged.TenantID = "forged-tenant"
	forgedBody := phase267Marshal(t, prototypes.AgentConfigAgentPacksInspectRequest{
		Identity: phase267IdentityScope(forged), AgentID: sourceID,
	})
	status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/inspect", id, forgedBody, adminReach())
	if status != http.StatusUnauthorized || phase267ErrorCode(t, raw) != protoerrors.CodeIdentityRequired || strings.Contains(string(raw), sourceID) {
		t.Fatalf("forged identity was not refused before exposure: status=%d code=%q body=%s", status, phase267ErrorCode(t, raw), raw)
	}

	// A separate Registry + StateStore represents another runtime. Its target
	// is real and resolvable there, but the current runtime's resolver must
	// refuse it before the copy port can inspect or write it.
	otherCfg := cfg
	if driverName == "sqlite" {
		otherCfg.DSN = filepath.Join(t.TempDir(), "other-runtime.sqlite")
	}
	otherStore, err := state.Open(ctx, otherCfg)
	if err != nil {
		t.Fatalf("other state.Open(%s): %v", driverName, err)
	}
	t.Cleanup(func() { _ = otherStore.Close(context.Background()) })
	otherRegistry, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: otherStore, Bus: deps.in.Bus})
	if err != nil {
		t.Fatalf("other agentcfg.Open(%s): %v", driverName, err)
	}
	t.Cleanup(func() { _ = otherRegistry.Close(context.Background()) })
	foreignTarget := "foreign-runtime-target"
	phase267SetAgentRevision(t, otherRegistry, id, foreignTarget, agentcfg.ConfigPayload{
		AgentPacks: []skills.AgentPackItem{keep},
	})
	otherResolver := NewAgentResolverAdapter(otherRegistry, "")
	if allowed, err := otherResolver.ResolveAgent(adminReach(), id, foreignTarget); err != nil || !allowed {
		t.Fatalf("other runtime did not resolve its own target: allowed=%t err=%v", allowed, err)
	}
	if allowed, err := currentResolver.ResolveAgent(adminReach(), id, foreignTarget); err != nil || allowed {
		t.Fatalf("current runtime resolved foreign target: allowed=%t err=%v", allowed, err)
	}
	foreignCopy := successCopy
	foreignCopy.TargetAgentID = foreignTarget
	foreignCopy.ExpectedTargetCompositionHash = strings.Repeat("f", 64)
	foreignCopy.IdempotencyKey = "phase267-foreign-runtime-" + driverName
	status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
		phase267Marshal(t, foreignCopy), phase267AgentPackContext(t, id, []string{sourceID, targetID, foreignTarget}, true))
	if status != http.StatusBadRequest || phase267ErrorCode(t, raw) != protoerrors.CodeInvalidRequest {
		t.Fatalf("foreign-runtime resolver denial status=%d code=%q body=%s", status, phase267ErrorCode(t, raw), raw)
	}
	afterForeign := inspect(targetID, adminReach())
	if afterForeign.CompositionHash != targetAfterCopy.CompositionHash {
		t.Fatalf("foreign-runtime request changed local target: before=%+v after=%+v", targetAfterCopy, afterForeign)
	}

	// Omitted and null pack_ids decode to nil and must remain invalid. Only an
	// explicit [] below authorizes the destructive 1-to-0 reconciliation.
	for _, tc := range []struct {
		name       string
		includeKey bool
	}{
		{name: "omitted"},
		{name: "null", includeKey: true},
	} {
		body := map[string]any{
			"identity":                         phase267IdentityScope(id),
			"source_agent_id":                  sourceID,
			"target_agent_id":                  targetID,
			"expected_source_composition_hash": sourceView.CompositionHash,
			"expected_target_composition_hash": targetAfterCopy.CompositionHash,
			"idempotency_key":                  "phase267-nil-selection-" + driverName + "-" + tc.name,
		}
		if tc.includeKey {
			body["pack_ids"] = nil
		}
		status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
			phase267Marshal(t, body), adminReach())
		if status != http.StatusBadRequest || phase267ErrorCode(t, raw) != protoerrors.CodeInvalidRequest {
			t.Fatalf("%s pack_ids status=%d code=%q body=%s", tc.name, status, phase267ErrorCode(t, raw), raw)
		}
		afterNil := inspect(targetID, adminReach())
		if afterNil.CompositionHash != targetAfterCopy.CompositionHash {
			t.Fatalf("%s pack_ids changed target: before=%+v after=%+v", tc.name, targetAfterCopy, afterNil)
		}
	}

	// An empty selection is a deliberate 1-to-0 reconciliation request: it
	// removes only server-stamped copies from this source while preserving the
	// independently authored target pack. This must traverse the public HTTP
	// validator rather than succeeding only through the direct runtime port.
	emptyCopy := successCopy
	emptyCopy.PackIDs = []string{}
	emptyCopy.ExpectedTargetCompositionHash = targetAfterCopy.CompositionHash
	emptyCopy.IdempotencyKey = "phase267-empty-reconcile-" + driverName
	status, raw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
		phase267Marshal(t, emptyCopy), adminReach())
	if status != http.StatusOK {
		t.Fatalf("empty reconciliation status=%d body=%s", status, raw)
	}
	var emptyResponse prototypes.AgentConfigAgentPacksCopyResponse
	if err := json.Unmarshal(raw, &emptyResponse); err != nil {
		t.Fatalf("decode empty reconciliation: %v; body=%s", err, raw)
	}
	if len(emptyResponse.Outcomes) != 0 {
		t.Fatalf("empty reconciliation outcomes = %+v, want none", emptyResponse.Outcomes)
	}
	targetAfterEmpty := inspect(targetID, adminReach())
	if len(targetAfterEmpty.EffectivePacks) != 1 || targetAfterEmpty.EffectivePacks[0].Pack.Name != "keep" || targetAfterEmpty.EffectivePacks[0].Pack.Steps[0] != "competitor-body" {
		t.Fatalf("empty reconciliation did not preserve only independent target pack: %+v", targetAfterEmpty)
	}
	status, replayRaw = postMuxWithContext(t, built.Mux, "/v1/agent_config/agent_packs/copy", id,
		phase267Marshal(t, emptyCopy), adminReach())
	if status != http.StatusOK {
		t.Fatalf("empty reconciliation replay status=%d body=%s", status, replayRaw)
	}
	var emptyReplay prototypes.AgentConfigAgentPacksCopyResponse
	if err := json.Unmarshal(replayRaw, &emptyReplay); err != nil {
		t.Fatalf("decode empty reconciliation replay: %v; body=%s", err, replayRaw)
	}
	if emptyReplay.CompositionHash != emptyResponse.CompositionHash || len(emptyReplay.Outcomes) != 0 {
		t.Fatalf("empty reconciliation replay changed result: first=%+v replay=%+v", emptyResponse, emptyReplay)
	}
}

func phase267IdentityScope(id identity.Identity) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

func phase267AgentPackContext(t *testing.T, id identity.Identity, reach []string, admin bool) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	if admin {
		ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	}
	return auth.WithAgentReach(ctx, reach)
}

func phase267Marshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(raw)
}

func phase267ErrorCode(t *testing.T, raw []byte) protoerrors.Code {
	t.Helper()
	var response protoerrors.Error
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode Protocol error: %v; body=%s", err, raw)
	}
	return response.Code
}

func phase267SetAgentRevision(t *testing.T, registry agentcfg.Registry, id identity.Identity, agentID string, payload agentcfg.ConfigPayload, opts ...agentcfg.SetOptions) agentcfg.Revision {
	t.Helper()
	setOptions := agentcfg.SetOptions{}
	if len(opts) > 0 {
		setOptions = opts[0]
	}
	revision, err := registry.SetRevision(context.Background(), identity.Quadruple{Identity: id}, agentID, agentcfg.ConfigScopeAgent, payload, setOptions)
	if err != nil {
		t.Fatalf("seed %s: %v", agentID, err)
	}
	return revision
}

func phase267FindPack(items []prototypes.AgentConfigAgentPackInspection, packID string) *prototypes.AgentConfigAgentPackInspection {
	for i := range items {
		if items[i].PackID == packID {
			return &items[i]
		}
	}
	return nil
}

func phase267HasTool(items []tools.Tool, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}

func phase267ToolNames(items []tools.Tool) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}
	return names
}

func phase267HasSkill(items []skills.RankedSkill, name string) bool {
	for _, item := range items {
		if item.Skill.Name == name {
			return true
		}
	}
	return false
}

func phase267HasSkillValue(items []skills.Skill, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}

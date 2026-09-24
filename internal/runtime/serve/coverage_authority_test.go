package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/materializer"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tasks"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
	"github.com/hurtener/Harbor/internal/virtualagent"
)

func TestTaskSnapshotAdapter_ProjectsOnlyBoundTaskEvidence(t *testing.T) {
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}

	if got, err := (&taskSnapshotAdapter{}).Task(ctx, id, "missing"); err != nil || got.TaskID != "" {
		t.Fatalf("nil registry snapshot = %+v, %v; want empty", got, err)
	}

	deps := buildProjWiringMux(t)
	artifactStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artifactStore.Close(context.Background()) })
	artifactScope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	inputRef, err := artifactStore.PutText(ctx, artifactScope, "attachment", artifacts.PutOpts{
		Filename: "input.txt", MimeType: "text/plain", Namespace: "input",
	})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}
	adapter := &taskSnapshotAdapter{reg: deps.tasks, arts: artifactStore, clock: time.Now}
	if _, err := adapter.Task(ctx, identity.Identity{}, "missing"); err == nil {
		t.Fatal("missing identity must fail closed before a task lookup")
	}
	if _, err := adapter.Task(ctx, id, "missing"); !errors.Is(err, materializer.ErrTaskSnapshotNotFound) {
		t.Fatalf("missing task error = %v, want ErrTaskSnapshotNotFound", err)
	}

	answer, err := json.Marshal(planner.AnswerEnvelope{Answer: "redacted final answer"})
	if err != nil {
		t.Fatal(err)
	}
	taskCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	h, err := deps.tasks.Spawn(taskCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id, RunID: "run-complete"},
		Kind:     tasks.KindForeground, Query: "redacted query", AgentID: "agent-a",
		InputArtifactIDs:          []string{inputRef.ID},
		InputArtifactDispositions: map[string]string{inputRef.ID: "ref"},
	})
	if err != nil {
		t.Fatalf("Spawn complete task: %v", err)
	}
	if err := deps.tasks.MarkRunning(taskCtx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := deps.tasks.MarkComplete(taskCtx, h.ID, tasks.TaskResult{Value: answer}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	snap, err := adapter.Task(ctx, id, string(h.ID))
	if err != nil {
		t.Fatalf("Task snapshot: %v", err)
	}
	if snap.TaskID != string(h.ID) || snap.RunID != "run-complete" || snap.Query != "redacted query" || !snap.QueryPresent {
		t.Fatalf("bound task/query projection = %+v", snap)
	}
	if !snap.AgentPresent || snap.AgentID != "agent-a" || snap.AgentBindingSource != turns.AgentBindingExplicit {
		t.Fatalf("agent projection = %+v", snap)
	}
	if !snap.AnswerPresent || snap.Answer.Inline != "redacted final answer" || snap.Answer.Complete != turns.CompletenessComplete {
		t.Fatalf("answer projection = %+v", snap.Answer)
	}
	if !snap.InputsPresent || len(snap.Inputs) != 1 || snap.Inputs[0].ID != inputRef.ID || snap.Inputs[0].Disposition != "ref" || snap.Inputs[0].Availability != turns.CompletenessComplete || snap.Inputs[0].Filename != "input.txt" || snap.Inputs[0].SHA256 != inputRef.SHA256 {
		t.Fatalf("input metadata projection = %+v", snap.Inputs)
	}
	if snap.QueryAt.IsZero() {
		t.Fatal("created-at timestamp was not projected")
	}

	failed, err := deps.tasks.Spawn(taskCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id, RunID: "run-failed"},
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatalf("Spawn failed task: %v", err)
	}
	if err := deps.tasks.MarkRunning(taskCtx, failed.ID); err != nil {
		t.Fatalf("MarkRunning failed task: %v", err)
	}
	if err := deps.tasks.MarkFailed(taskCtx, failed.ID, tasks.TaskError{Code: "refused", Message: "redacted refusal"}); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	failedSnap, err := adapter.Task(ctx, id, string(failed.ID))
	if err != nil {
		t.Fatalf("failed task snapshot: %v", err)
	}
	if !failedSnap.FailurePresent || failedSnap.ErrorCode != "refused" || failedSnap.ErrorMessage != "redacted refusal" {
		t.Fatalf("failure projection = %+v", failedSnap)
	}
	if failedSnap.AgentPresent || failedSnap.AgentBindingSource != turns.AgentBindingUnknown {
		t.Fatalf("default-agent projection = %+v", failedSnap)
	}

	malformedAnswer, err := deps.tasks.Spawn(taskCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id, RunID: "run-malformed-answer"},
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatalf("Spawn malformed answer task: %v", err)
	}
	if err := deps.tasks.MarkRunning(taskCtx, malformedAnswer.ID); err != nil {
		t.Fatalf("MarkRunning malformed answer task: %v", err)
	}
	if err := deps.tasks.MarkComplete(taskCtx, malformedAnswer.ID, tasks.TaskResult{Value: json.RawMessage(`"not-an-answer-envelope"`)}); err != nil {
		t.Fatalf("MarkComplete malformed answer task: %v", err)
	}
	malformedSnap, err := adapter.Task(ctx, id, string(malformedAnswer.ID))
	if err != nil {
		t.Fatalf("malformed answer snapshot: %v", err)
	}
	if malformedSnap.AnswerPresent {
		t.Fatalf("malformed persisted answer fabricated a claim: %+v", malformedSnap.Answer)
	}
}

type coverageErasureProbe struct {
	erased bool
	err    error
}

func (p coverageErasureProbe) Erased(context.Context, identity.Identity) (bool, error) {
	return p.erased, p.err
}

func TestProjectionComposition_FailsLoudAndPreservesErasureAuthority(t *testing.T) {
	if turnsErasureProbe(nil) != nil {
		t.Fatal("nil session eraser must keep the erasure probe unavailable")
	}
	wantErr := errors.New("erasure unavailable")
	probe := turnsErasureProbe(coverageErasureProbe{erased: true, err: wantErr})
	erased, err := probe.Erased(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	if !erased || !errors.Is(err, wantErr) {
		t.Fatalf("erasure probe = %v, %v", erased, err)
	}
	if got := unixNanoTime(0); !got.IsZero() {
		t.Fatalf("zero unix-nano = %v", got)
	}
	if got := unixNanoTime(123); got.UnixNano() != 123 || got.Location() != time.UTC {
		t.Fatalf("unix-nano projection = %v", got)
	}

	cfg := config.Defaults()
	cfg.Sessions.Turns = config.TurnsConfig{Driver: "inmem"}
	if _, _, _, err := OpenTurnsProjection(context.Background(), cfg, TurnsProjectionDeps{Logger: projectionsLogger()}); err == nil || !strings.Contains(err.Error(), "projection source") {
		t.Fatalf("turns projection without an event source error = %v", err)
	}
	cfg.Observability.Rollups = config.RollupsConfig{Driver: "inmem"}
	if _, _, _, err := OpenRollupsProjection(context.Background(), cfg, RollupsProjectionDeps{Logger: projectionsLogger()}); err == nil || !strings.Contains(err.Error(), "projection source") {
		t.Fatalf("rollups projection without an event source error = %v", err)
	}

	order := []int{}
	closeProjectionClosers([]func(context.Context) error{
		func(context.Context) error { order = append(order, 1); return errors.New("ignored one") },
		func(context.Context) error { order = append(order, 2); return errors.New("ignored two") },
	})
	if !reflect.DeepEqual(order, []int{2, 1}) {
		t.Fatalf("partial construction closed in order %v, want [2 1]", order)
	}
}

func TestSessionWindowProjection_UsesOnlyRegistryLifecycleEvidence(t *testing.T) {
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}

	opened, last, ok, err := sessionWindowFunc(nil)(ctx, id, id.SessionID)
	if err != nil || ok || !opened.IsZero() || !last.IsZero() {
		t.Fatalf("nil registry window = %v %v %v %v", opened, last, ok, err)
	}

	deps := buildProjWiringMux(t)
	window := sessionWindowFunc(deps.sess)
	identityCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := window(identityCtx, id, id.SessionID); err == nil {
		t.Fatal("unknown session must preserve the registry error")
	}
	if _, err := deps.sess.Open(identityCtx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	opened, last, ok, err = window(identityCtx, id, id.SessionID)
	if err != nil || !ok || opened.IsZero() || last.Before(opened) {
		t.Fatalf("open session window = %v %v %v %v", opened, last, ok, err)
	}
}

func TestBoot_InMemoryProjectionServicesComposeAndClose(t *testing.T) {
	cfg, err := loadTestCfg(t, writeTestCfg(t))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Sessions.Turns = config.TurnsConfig{Driver: "inmem", Retention: 32}
	cfg.Memory.Strategy = "rolling_summary"
	cfg.Memory.RecentTurns = 2
	cfg.Observability.Rollups = config.RollupsConfig{Driver: "inmem"}

	opts := baseOptions(t)
	opts.Config = cfg
	h, err := Boot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Boot with in-memory projections: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.Close(closeCtx)
}

func TestSignedOAuthCapabilityAuthorities_BootAnchorsAreExplicit(t *testing.T) {
	ctx := context.Background()
	if got, err := SignedOAuthMCPCapabilityAuthoritiesFromConfig(ctx, nil, nil); err != nil || got != nil {
		t.Fatalf("nil config = %v, %v", got, err)
	}
	cfg := &config.Config{Tools: config.ToolsConfig{OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{Name: "disabled"}}}}
	got, err := SignedOAuthMCPCapabilityAuthoritiesFromConfig(ctx, cfg, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("broker without authority = %v, %v", got, err)
	}
	cfg.Tools.OAuthCredentialBrokers[0].SignedOAuthMCPCapabilityAuthority = &config.ToolSignedOAuthMCPCapabilityAuthorityConfig{Enabled: true}
	if _, err := SignedOAuthMCPCapabilityAuthoritiesFromConfig(ctx, cfg, nil); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete authority error = %v", err)
	}

	cfg.Tools.OAuthCredentialBrokers[0] = config.ToolOAuthCredentialBrokerConfig{
		Name: "broker", ScopeCeiling: []string{"read", "edit"},
		SignedOAuthMCPCapabilityAuthority: &config.ToolSignedOAuthMCPCapabilityAuthorityConfig{
			Enabled: true, Issuer: "https://issuer.example", MaxAuthorityLifetime: 10 * time.Minute,
			JWKSFile: filepath.Join("..", "..", "protocol", "auth", "testdata", "jwks.json"),
		},
	}
	got, err = SignedOAuthMCPCapabilityAuthoritiesFromConfig(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("valid authority: %v", err)
	}
	a, ok := got["broker"]
	if !ok || a.Broker != "broker" || a.Issuer != "https://issuer.example" || a.Keys == nil || a.MaxAuthorityLifetime != 10*time.Minute {
		t.Fatalf("authority = %+v", a)
	}
	cfg.Tools.OAuthCredentialBrokers[0].ScopeCeiling[0] = "mutated"
	if a.ScopeCeiling[0] != "read" {
		t.Fatalf("authority scope ceiling aliased config: %v", a.ScopeCeiling)
	}
}

func TestPreparedOAuthProvider_PublicationIsReversibleUntilCommit(t *testing.T) {
	ctx := context.Background()
	owner := toolauth.Owner{Tenant: "tenant", Agent: "agent"}
	set := toolauth.NewProviderSet(nil)
	installer := &OAuthProviderInstaller{set: set, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	prepared := &preparedOAuthProvider{installer: installer, owner: owner, name: "provider", provider: &recordingProvider{}}
	if prepared.Binding() == nil {
		t.Fatal("prepared binding is nil")
	}
	if err := prepared.Publish(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := prepared.Publish(ctx); err != nil {
		t.Fatalf("idempotent Publish: %v", err)
	}
	if _, ok := set.Get("provider"); !ok {
		t.Fatal("published provider is absent")
	}
	if err := prepared.Close(ctx); err == nil || !strings.Contains(err.Error(), "requires Rollback or Commit") {
		t.Fatalf("Close on published provider error = %v", err)
	}
	if err := prepared.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, ok := set.Get("provider"); ok {
		t.Fatal("rolled-back provider remains published")
	}
	if err := prepared.Rollback(ctx); err != nil {
		t.Fatalf("idempotent Rollback: %v", err)
	}
	if err := prepared.Publish(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Publish after rollback error = %v", err)
	}

	committed := &preparedOAuthProvider{installer: installer, owner: owner, name: "committed", provider: &recordingProvider{}}
	if err := committed.Publish(ctx); err != nil {
		t.Fatalf("Publish committed provider: %v", err)
	}
	committed.Commit(ctx)
	committed.Commit(ctx)
	if _, ok := set.Get("committed"); !ok {
		t.Fatal("committed provider is absent")
	}
	if err := committed.Rollback(ctx); err == nil || !strings.Contains(err.Error(), "cannot be rolled back") {
		t.Fatalf("Rollback after commit error = %v", err)
	}

	closed := &preparedOAuthProvider{installer: installer, owner: owner, name: "closed", provider: &recordingProvider{}}
	if err := closed.Close(ctx); err != nil {
		t.Fatalf("Close unpublished provider: %v", err)
	}
	if err := closed.Close(ctx); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}

func TestOAuthProviderInstaller_PreparationGuardsAndWireKillSwitch(t *testing.T) {
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{}, toolauth.BuildDeps{})
	if err != nil {
		t.Fatal(err)
	}
	installer := NewOAuthProviderInstaller(builder, toolauth.NewProviderSet(nil), false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := installer.PrepareProvider(context.Background(), "", "agent", agentcfg.OAuthProviderDescriptor{Name: "p"}); !errors.Is(err, agentcfgprotocol.ErrInvalidProvider) {
		t.Fatalf("PrepareProvider missing owner = %v", err)
	}
	if _, err := installer.PrepareProvider(context.Background(), "tenant", "agent", agentcfg.OAuthProviderDescriptor{Name: "p", CredentialBroker: "missing"}); !errors.Is(err, agentcfgprotocol.ErrProviderBrokerUnknown) {
		t.Fatalf("PrepareProvider unknown broker = %v", err)
	}
	if _, err := installer.PrepareSignedCapabilityProvider(context.Background(), "missing", toolauth.SignedCapabilityExchangeBinding{}, nil); !errors.Is(err, agentcfgprotocol.ErrInvalidProvider) {
		t.Fatalf("PrepareSignedCapabilityProvider missing owner = %v", err)
	}
	if err := installer.InstallProvider(context.Background(), "tenant", "agent", agentcfg.OAuthProviderDescriptor{
		Name: "wire", CredentialBroker: "missing", TokenURL: "https://oauth.example/token",
	}); err != nil {
		t.Fatalf("wire kill-switch reconcile must omit the disabled provider without bricking run start: %v", err)
	}
	if _, ok := installer.set.Get("wire"); ok {
		t.Fatal("wire provider was installed while the kill-switch was off")
	}
}

func TestMCPAttacher_CompensationAndCloseAreOwnerBound(t *testing.T) {
	a := NewMCPConnectionAttacher(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Identity{}, nil, nil, nil)
	if err := a.DetachConnection(context.Background(), "", "agent", "server"); !errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Fatalf("DetachConnection missing owner = %v", err)
	}
	if _, err := a.BeginExactConnectionTeardown("tenant", "agent", "server", "fingerprint"); err == nil {
		t.Fatal("exact teardown without registry must fail loud")
	}
	if _, err := a.BeginExactConnectionTeardownForOwner(toolauth.Owner{Tenant: "tenant", Agent: "agent"}, "server", "fingerprint"); err == nil {
		t.Fatal("user-scoped teardown without user and registry must fail loud")
	}

	wantErr := errors.New("closer three")
	order := []int{}
	a = &MCPConnectionAttacher{closers: []func(context.Context) error{
		func(context.Context) error { order = append(order, 1); return nil },
		func(context.Context) error { order = append(order, 2); return errors.New("closer two") },
		func(context.Context) error { order = append(order, 3); return wantErr },
	}}
	if err := a.Close(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Close error = %v, want first reverse-order error %v", err, wantErr)
	}
	if !reflect.DeepEqual(order, []int{3, 2, 1}) {
		t.Fatalf("close order = %v, want [3 2 1]", order)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}

	registry := mcpdrv.NewRegistry()
	a = NewMCPConnectionAttacher(nil, registry, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Identity{}, nil, nil, nil)
	if _, err := a.BeginExactConnectionTeardownForOwner(toolauth.Owner{Tenant: "tenant", Agent: "agent"}, "server", "fingerprint"); err == nil {
		t.Fatal("user-scoped teardown without a verified user must fail loud")
	}
}

func TestVirtualProfileProjection_NarrowsWithoutMutatingBase(t *testing.T) {
	if got := applyVirtualOverlay(nil, virtualagent.Profile{}); got != nil {
		t.Fatalf("zero overlay changed nil base: %+v", got)
	}
	baseModel, baseInstructions := "base-model", "base guidance"
	baseMax := 800
	base := &planner.LLMOverrides{
		Model: &baseModel, MaxTokens: &baseMax, ExtraInstructions: &baseInstructions,
		ExtraSystemBlocks: []planner.NamedBlock{{Name: "base", Body: "keep"}},
	}
	model, effort, instructions := "narrow-model", "high", "specialist guidance"
	temperature := 0.2
	maxTokens := 400
	profile := virtualagent.Profile{Key: "reviewer", Overlay: virtualagent.Overlay{
		Model: &model, Temperature: &temperature, MaxTokens: &maxTokens,
		ReasoningEffort: &effort, Instructions: instructions,
	}}
	got := applyVirtualOverlay(base, profile)
	if got == base || *got.Model != model || *got.Temperature != temperature || *got.MaxTokens != maxTokens || *got.ReasoningEffort != effort {
		t.Fatalf("overlay = %+v", got)
	}
	if *got.ExtraInstructions != baseInstructions+"\n"+instructions || len(got.ExtraSystemBlocks) != 2 || got.ExtraSystemBlocks[1].Name != virtualagent.BlockName(profile.Key) {
		t.Fatalf("instruction projection = %+v / %+v", got.ExtraInstructions, got.ExtraSystemBlocks)
	}
	if *base.Model != baseModel || *base.MaxTokens != baseMax || len(base.ExtraSystemBlocks) != 1 {
		t.Fatalf("base mutated: %+v", base)
	}
	model = "mutated-after-projection"
	if *got.Model != "narrow-model" {
		t.Fatalf("overlay model pointer aliases profile: %q", *got.Model)
	}

	views := []skills.SkillView{{Name: "read"}, {Name: "edit"}, {Name: "inspect"}}
	if narrowed := applyVirtualSkills(views, nil); !reflect.DeepEqual(narrowed, views) {
		t.Fatalf("nil profile narrowed skills: %v", narrowed)
	}
	allowed := []string{"inspect", "read"}
	profile.Overlay.Skills = &allowed
	if narrowed := applyVirtualSkills(views, &profile); !reflect.DeepEqual(narrowed, []skills.SkillView{{Name: "read"}, {Name: "inspect"}}) {
		t.Fatalf("narrowed skills = %v", narrowed)
	}
	if cloneString(nil) != nil {
		t.Fatal("cloneString(nil) must stay nil")
	}
}

// Package integration — in-process pass-by-reference tool routing and
// the substitution invariant, end to end.
//
// The claim under test is directional: bytes flow store → consumer. A
// tool declares an artifact-reference parameter, the model supplies an
// id, the runtime resolves it at dispatch, and the tool reads content
// the model never saw. The invariant that makes it worth having is that
// the resolved value is DISPATCH-LOCAL — it does not appear in the
// trajectory, in the observation the runtime interleaves into the next
// chat thread, in any canonical event payload, in an audit payload, or
// in a log.
//
// The runtime bounds that invariant on the PRODUCTION side (one
// substitution call site, held to one by a mechanical scan). This file
// is the ARRIVAL side of the same claim, which is the half a production
// bound cannot supply on its own: it plants a unique marker as the
// artifact's content and then proves the marker is absent from every
// observable surface the dispatch touches, while proving in the same
// breath that the tool DID read it — so the absences are not vacuous.
//
// Every component on the seam is real (CLAUDE.md §17.3): the in-memory
// artifacts driver, the in-memory event bus opened over the real
// pattern-based audit redactor, a real `tools.ToolCatalog` with the
// lifecycle-emitting shell live, the production `dispatch.ToolExecutor`,
// and the shipped `examples/tools/artifactstats` consumer.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/examples/tools/artifactstats"
	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
)

// pbrMarker is the planted content. It appears nowhere else in the
// repository, so a substring search over any surface is a decisive test
// of whether the resolved value reached it.
const pbrMarker = "PBR-RESOLVED-ARTIFACT-CONTENT-b7f31c2a"

// pbrBody is the artifact's stored content: the marker plus enough
// filler that a size assertion distinguishes "read the bytes" from
// "read the id".
var pbrBody = []byte(pbrMarker + "\n" + strings.Repeat("filler line\n", 64))

type pbrStack struct {
	store artifacts.ArtifactStore
	bus   events.EventBus
	cat   tools.ToolCatalog
	exec  steering.ToolExecutor
	// logs captures everything the executor logs during a dispatch.
	logs *bytes.Buffer
}

// newPBRStack wires the real components the routing crosses.
func newPBRStack(t *testing.T) *pbrStack {
	t.Helper()

	store, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	// The REAL redactor, so an event payload that carried the marker
	// would have to survive the same pass every emitted payload does.
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// A catalog with the bus wired, so the universal tool-lifecycle
	// shell emits tool.invoked / .completed / .failed for every dispatch
	// here exactly as it does in production.
	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register %s: %v", artifactstats.ToolName, err)
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	exec := dispatch.NewToolExecutor(cat, store, nil, dispatch.WithLogger(logger))

	return &pbrStack{store: store, bus: bus, cat: cat, exec: exec, logs: logs}
}

func pbrQuad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

func pbrCtx(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// pbrPut stores the marker-bearing body under the quadruple's triple and
// returns the ref id the model would be told to author.
func pbrPut(t *testing.T, store artifacts.ArtifactStore, q identity.Quadruple, body []byte) string {
	t.Helper()
	scope := artifacts.ArtifactScope{
		TenantID:  q.TenantID,
		UserID:    q.UserID,
		SessionID: q.SessionID,
	}
	ref, err := store.PutBytes(pbrCtx(t, q), scope, body, artifacts.PutOpts{
		Filename: "planted.txt",
		MimeType: "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref.ID
}

// pbrCall builds the CallTool decision the planner would emit: an id,
// never content.
func pbrCall(refID string) planner.CallTool {
	return planner.CallTool{
		CallID: "call_pbr_1",
		Tool:   artifactstats.ToolName,
		Args:   json.RawMessage(fmt.Sprintf(`{"artifact":%q}`, refID)),
	}
}

// mustJSON renders v so a substring search covers the whole structure
// rather than only its top level.
func mustJSON(t *testing.T, label string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", label, err)
	}
	return string(b)
}

// TestE2E_PassByRef_ResolvedValueNeverReachesTheObservableRecord is the
// substitution invariant, asserted from the arrival side across every
// surface the contract names.
func TestE2E_PassByRef_ResolvedValueNeverReachesTheObservableRecord(t *testing.T) {
	st := newPBRStack(t)
	q := pbrQuad("pbr-tenant-a", "pbr-user-a", "pbr-session-a", "pbr-run-1")
	ctx := pbrCtx(t, q)
	refID := pbrPut(t, st.store, q, pbrBody)

	// Subscribe BEFORE dispatch so nothing published during it is missed.
	sub, err := st.bus.Subscribe(ctx, events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	rc := planner.RunContext{Quadruple: q, Trajectory: &trajectory.Trajectory{}}
	call := pbrCall(refID)
	raw, llmObs, err := st.exec.ExecuteDecision(ctx, rc, call)
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}

	// --- The routing worked. Without this the absences below are
	// --- vacuous: a tool that read nothing leaks nothing.
	out, ok := raw.(artifactstats.StatsResult)
	if !ok {
		t.Fatalf("observation = %T, want artifactstats.StatsResult", raw)
	}
	if out.SizeBytes != len(pbrBody) {
		t.Fatalf("tool measured %d bytes, want %d — the tool did not receive the stored content",
			out.SizeBytes, len(pbrBody))
	}
	if out.ArtifactID != refID {
		t.Fatalf("tool saw ref %q, want %q", out.ArtifactID, refID)
	}
	if out.RuneCount != len([]rune(string(pbrBody))) {
		t.Fatalf("tool counted %d runes, want %d", out.RuneCount, len([]rune(string(pbrBody))))
	}

	// --- The invariant, surface by surface.
	rawJSON := mustJSON(t, "raw observation", raw)
	if strings.Contains(rawJSON, pbrMarker) {
		t.Errorf("the raw observation carries the resolved value: %s", rawJSON)
	}
	llmJSON := mustJSON(t, "llm observation", llmObs)
	if strings.Contains(llmJSON, pbrMarker) {
		t.Errorf("the LLM-facing observation carries the resolved value: %s", llmJSON)
	}

	// The trajectory the next prompt renders from — appended exactly as
	// the run loop appends it after a dispatched step.
	rc.Trajectory.Steps = append(rc.Trajectory.Steps, trajectory.Step{
		Action:         call,
		Observation:    raw,
		LLMObservation: llmObs,
	})
	serialised, err := rc.Trajectory.Serialize()
	if err != nil {
		t.Fatalf("Trajectory.Serialize: %v", err)
	}
	if strings.Contains(string(serialised), pbrMarker) {
		t.Errorf("the trajectory carries the resolved value: %s", serialised)
	}
	// And the argument the trajectory replays is still the id.
	if !strings.Contains(string(serialised), refID) {
		t.Errorf("the trajectory dropped the reference id the model authored: %s", serialised)
	}

	// Every canonical event payload published during the dispatch. The
	// universal tool-lifecycle shell emits at least `tool.invoked` and
	// `tool.completed` for a successful call, so the arm waits (bounded,
	// no sleep) for those two before draining whatever else arrived —
	// otherwise a fast drain could report zero events and call the
	// absence proven.
	const wantAtLeast = 2
	seen := 0
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case ev, open := <-sub.Events():
			if !open {
				break collect
			}
			seen++
			payload := mustJSON(t, string(ev.Type), ev.Payload)
			if strings.Contains(payload, pbrMarker) {
				t.Errorf("event %q carries the resolved value: %s", ev.Type, payload)
			}
			// Also check the envelope's own rendering, not only the
			// payload: an identity or type field is a surface too.
			if strings.Contains(mustJSON(t, "event envelope", ev), pbrMarker) {
				t.Errorf("event %q envelope carries the resolved value", ev.Type)
			}
		case <-deadline:
			break collect
		}
		if seen >= wantAtLeast {
			// The expected pair has landed; take anything else that is
			// already queued and stop.
			for {
				select {
				case ev, open := <-sub.Events():
					if !open {
						break collect
					}
					seen++
					if strings.Contains(mustJSON(t, string(ev.Type), ev.Payload), pbrMarker) {
						t.Errorf("event %q carries the resolved value", ev.Type)
					}
				default:
					break collect
				}
			}
		}
	}
	if seen < wantAtLeast {
		t.Fatalf("observed %d lifecycle events, want at least %d; the event arm of this test proved nothing", seen, wantAtLeast)
	}

	// Everything the executor logged. A clean dispatch logs nothing, so
	// this arm is a cheap guard here; the LIVE log arm — a dispatch that
	// actually produces records — is asserted in the refusal test below,
	// which forces the executor's failure Warn.
	if strings.Contains(st.logs.String(), pbrMarker) {
		t.Errorf("a log record carries the resolved value: %s", st.logs.String())
	}
}

// TestE2E_PassByRef_CrossTenantReferenceDoesNotResolve — a reference is
// resolved under the DISPATCHING run's identity, so an id minted in one
// tenant is not readable from another. The refusal is loud, and the
// bytes are never handed over.
func TestE2E_PassByRef_CrossTenantReferenceDoesNotResolve(t *testing.T) {
	st := newPBRStack(t)

	owner := pbrQuad("pbr-tenant-a", "pbr-user-a", "pbr-session-a", "pbr-run-a")
	refID := pbrPut(t, st.store, owner, pbrBody)

	for _, tc := range []struct {
		name string
		q    identity.Quadruple
	}{
		{"another tenant", pbrQuad("pbr-tenant-b", "pbr-user-a", "pbr-session-a", "pbr-run-b")},
		{"another user", pbrQuad("pbr-tenant-a", "pbr-user-b", "pbr-session-a", "pbr-run-c")},
		{"another session", pbrQuad("pbr-tenant-a", "pbr-user-a", "pbr-session-b", "pbr-run-d")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := pbrCtx(t, tc.q)
			rc := planner.RunContext{Quadruple: tc.q, Trajectory: &trajectory.Trajectory{}}
			raw, llmObs, err := st.exec.ExecuteDecision(ctx, rc, pbrCall(refID))
			if err == nil {
				t.Fatalf("an out-of-scope reference resolved: raw=%v llm=%v", raw, llmObs)
			}
			if !strings.Contains(err.Error(), refID) {
				t.Errorf("err = %v, want it to name the reference id", err)
			}
			if strings.Contains(err.Error(), pbrMarker) {
				t.Errorf("the refusal leaked the content it refused: %v", err)
			}
			if raw != nil || llmObs != nil {
				t.Errorf("a refused dispatch produced observations: raw=%v llm=%v", raw, llmObs)
			}
		})
	}

	// The LIVE log arm: the executor logs a Warn for each refusal above,
	// so the buffer is non-empty and the "no marker in a log" assertion
	// is a statement about records that exist rather than about silence.
	if st.logs.Len() == 0 {
		t.Fatal("the executor logged nothing for three refused dispatches; the log arm of this test proved nothing")
	}
	if strings.Contains(st.logs.String(), pbrMarker) {
		t.Errorf("a log record carries the resolved value: %s", st.logs.String())
	}

	// The owner still reads it — the refusals above are a boundary, not
	// a broken store.
	ctx := pbrCtx(t, owner)
	rc := planner.RunContext{Quadruple: owner, Trajectory: &trajectory.Trajectory{}}
	raw, _, err := st.exec.ExecuteDecision(ctx, rc, pbrCall(refID))
	if err != nil {
		t.Fatalf("the owning run could not read its own artifact: %v", err)
	}
	if out, ok := raw.(artifactstats.StatsResult); !ok || out.SizeBytes != len(pbrBody) {
		t.Fatalf("owning run read %v, want %d bytes", raw, len(pbrBody))
	}
}

// TestE2E_PassByRef_UnknownReferenceFailsLoudly — a reference the model
// invented resolves to nothing, and nothing is exactly what the tool
// must NOT be handed. The failure is the step's error, so the planner
// re-plans instead of reasoning over an empty artifact.
func TestE2E_PassByRef_UnknownReferenceFailsLoudly(t *testing.T) {
	st := newPBRStack(t)
	q := pbrQuad("pbr-tenant-a", "pbr-user-a", "pbr-session-a", "pbr-run-x")
	ctx := pbrCtx(t, q)
	rc := planner.RunContext{Quadruple: q, Trajectory: &trajectory.Trajectory{}}

	_, _, err := st.exec.ExecuteDecision(ctx, rc, pbrCall("tool_doesnotexist"))
	if err == nil {
		t.Fatal("an unknown reference dispatched successfully")
	}
	if !strings.Contains(err.Error(), "tool_doesnotexist") {
		t.Errorf("err = %v, want it to name the reference id", err)
	}
}

// TestE2E_PassByRef_NoArtifactStoreWiredFailsLoudly — a degraded stack
// refuses the reference by name rather than handing the tool an empty
// value it would read as an empty artifact.
func TestE2E_PassByRef_NoArtifactStoreWiredFailsLoudly(t *testing.T) {
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register: %v", err)
	}
	exec := dispatch.NewToolExecutor(cat, nil, nil)

	q := pbrQuad("pbr-tenant-a", "pbr-user-a", "pbr-session-a", "pbr-run-y")
	ctx := pbrCtx(t, q)
	rc := planner.RunContext{Quadruple: q, Trajectory: &trajectory.Trajectory{}}

	_, _, err := exec.ExecuteDecision(ctx, rc, pbrCall("tool_anything"))
	if err == nil {
		t.Fatal("a reference resolved with no artifact store wired")
	}
	if !strings.Contains(err.Error(), "no artifact store wired") {
		t.Errorf("err = %v, want it to name the missing store", err)
	}
}

// TestE2E_PassByRef_ConcurrentRunsDoNotCrossTalk — N concurrent runs
// across two tenants share ONE catalog, ONE store and ONE executor. Each
// plants content whose length is derived from its own index, so a run
// reading another run's artifact shows up as a size mismatch, and a
// cross-tenant read shows up as an error where a success was expected.
func TestE2E_PassByRef_ConcurrentRunsDoNotCrossTalk(t *testing.T) {
	const perTenant = 24

	st := newPBRStack(t)
	tenants := []string{"pbr-tenant-a", "pbr-tenant-b"}

	type plant struct {
		q     identity.Quadruple
		refID string
		want  int
	}
	plants := make([]plant, 0, perTenant*len(tenants))
	for ti, tenant := range tenants {
		for i := range perTenant {
			q := pbrQuad(tenant,
				fmt.Sprintf("pbr-user-%d", i),
				fmt.Sprintf("pbr-session-%d", i),
				fmt.Sprintf("pbr-run-%d-%d", ti, i))
			// Unique content per (tenant, index), and a LENGTH derived
			// from both — so a bleed shows up as a size mismatch on its
			// own, without relying on the wrong id also failing to
			// resolve. Two runs at the same index in different tenants
			// would otherwise carry equal-length bodies.
			body := []byte(fmt.Sprintf("%s/%d/", tenant, i) +
				strings.Repeat("z", ti*(perTenant+2)+i+1))
			plants = append(plants, plant{q: q, refID: pbrPut(t, st.store, q, body), want: len(body)})
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, len(plants))
	sizes := make([]int, len(plants))
	for i, p := range plants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, err := identity.WithRun(context.Background(), p.q.Identity, p.q.RunID)
			if err != nil {
				errs[i] = err
				return
			}
			rc := planner.RunContext{Quadruple: p.q, Trajectory: &trajectory.Trajectory{}}
			raw, _, err := st.exec.ExecuteDecision(ctx, rc, pbrCall(p.refID))
			if err != nil {
				errs[i] = err
				return
			}
			out, ok := raw.(artifactstats.StatsResult)
			if !ok {
				errs[i] = fmt.Errorf("observation = %T", raw)
				return
			}
			if out.ArtifactID != p.refID {
				errs[i] = fmt.Errorf("resolved ref %q, want %q", out.ArtifactID, p.refID)
				return
			}
			sizes[i] = out.SizeBytes
		}()
	}
	wg.Wait()

	for i, p := range plants {
		if errs[i] != nil {
			t.Fatalf("run %d (%s): %v", i, p.q.TenantID, errs[i])
		}
		if sizes[i] != p.want {
			t.Fatalf("run %d (%s) read %d bytes, want %d — a run resolved another run's artifact",
				i, p.q.TenantID, sizes[i], p.want)
		}
	}
	if strings.Contains(st.logs.String(), pbrMarker) {
		t.Errorf("a log record carries the resolved value: %s", st.logs.String())
	}
}

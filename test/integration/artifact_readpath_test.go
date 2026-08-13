// Package integration — the artifact READ path, end to end.
//
// Two claims are under test and they meet in the same run:
//
//  1. A read that cannot travel through the model-facing string channel
//     REFUSES with facts (its MIME, the failing byte offset, the
//     by-reference route) instead of delivering U+FFFD soup at a length
//     it misreports. A read that can travel is byte-exact, and a paging
//     loop over it terminates and reassembles.
//  2. A tool's artifact-reference argument that fails to resolve reaches
//     the planner's NEXT turn as a CLASSIFIED observation, so the model
//     distinguishes "the id you named does not resolve" from "the tool
//     failed" from "the runtime is misconfigured" by reading a field.
//
// Every seam is real (CLAUDE.md §17.3): two production artifact drivers
// (inmem and sqlite), the real built-in registration path, the shipped
// `examples/tools/artifactstats` reference consumer, the production
// dispatch executor, and the real run loop over a real pause
// coordinator. Identity propagation is asserted on every leg, and three
// failure modes ship: an unresolvable reference, a stack with no
// artifact store, and a tool whose own body errors (the unclassified
// control).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/examples/tools/artifactstats"
	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	artifactssqlite "github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

// readPathDrivers are the production artifact drivers this suite runs
// every leg against. Two, not one: the read path's correctness must not
// be an in-memory accident.
func readPathDrivers() map[string]func(t *testing.T) artifacts.ArtifactStore {
	return map[string]func(t *testing.T) artifacts.ArtifactStore{
		"inmem": func(t *testing.T) artifacts.ArtifactStore {
			t.Helper()
			s, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
			if err != nil {
				t.Fatalf("inmem driver: %v", err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			return s
		},
		"sqlite": func(t *testing.T) artifacts.ArtifactStore {
			t.Helper()
			s, err := artifactssqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: ":memory:"})
			if err != nil {
				t.Fatalf("sqlite driver: %v", err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			return s
		},
	}
}

// readPathQuad builds a run quadruple; the triple is what every read
// resolves on.
func readPathQuad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

func readPathCtx(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// readPathPut stores a payload under the quadruple's TRIPLE — the read
// key — and returns the id the model would author.
func readPathPut(t *testing.T, store artifacts.ArtifactStore, q identity.Quadruple, body []byte, mime string) string {
	t.Helper()
	ref, err := store.PutBytes(readPathCtx(t, q), artifacts.ArtifactScope{
		TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
	}, body, artifacts.PutOpts{MimeType: mime, Filename: "fixture.bin"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref.ID
}

// readPathCatalog registers `artifact_fetch` through the REAL builtin
// registration path (not a hand-wired descriptor), so the bounds the
// operator configures are the ones under test.
func readPathCatalog(t *testing.T, store artifacts.ArtifactStore, defaultMax, hardMax int) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	if err := builtin.RegisterWith(builtin.RegistryContext{
		Catalog:                      cat,
		ArtifactStore:                store,
		ArtifactFetchDefaultMaxBytes: defaultMax,
		ArtifactFetchHardMaxBytes:    hardMax,
	}, []string{"artifact_fetch"}); err != nil {
		t.Fatalf("register artifact_fetch: %v", err)
	}
	return cat
}

// fetchResponse is the decoded artifact_fetch result. Decoded from the
// tool's JSON rather than read off a Go struct on purpose: JSON is where
// the U+FFFD rewrite happened, so a test that never encodes could not
// have seen the defect.
type fetchResponse struct {
	Ref            string `json:"ref"`
	MIME           string `json:"mime"`
	SizeBytes      int64  `json:"size_bytes"`
	Content        string `json:"content"`
	Offset         int64  `json:"offset"`
	ReturnedBytes  int64  `json:"returned_bytes"`
	TotalSizeBytes int64  `json:"total_size_bytes"`
	Truncated      bool   `json:"truncated"`
	Error          string `json:"error"`
}

// invokeFetch dispatches artifact_fetch through the catalog exactly as
// the runtime does and decodes the response through JSON.
func invokeFetch(t *testing.T, cat tools.ToolCatalog, ctx context.Context, args map[string]any) fetchResponse {
	t.Helper()
	desc, ok := cat.Resolve("artifact_fetch")
	if !ok {
		t.Fatal("artifact_fetch is not registered")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := desc.Invoke(ctx, raw)
	if err != nil {
		t.Fatalf("artifact_fetch invoke: %v", err)
	}
	encoded, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out fetchResponse
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

// TestE2E_ArtifactReadPath_BinaryRefusesAndTextPages is legs (a) and (b)
// of the read half, across two real drivers.
func TestE2E_ArtifactReadPath_BinaryRefusesAndTextPages(t *testing.T) {
	for name, open := range readPathDrivers() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			cat := readPathCatalog(t, store, 0, 0)
			owner := readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-a", "rp-run-1")
			ctx := readPathCtx(t, owner)

			// (a) A PDF refuses rather than corrupting.
			pdf := append([]byte("%PDF-1.7\n%"), 0xE2, 0xE3, 0xCF, 0xD3, 0x0A)
			pdfID := readPathPut(t, store, owner, pdf, "application/pdf")

			got := invokeFetch(t, cat, ctx, map[string]any{"ref": pdfID})
			if got.Error == "" {
				t.Fatalf("a PDF was returned as text: %q", got.Content)
			}
			if got.Content != "" {
				t.Fatalf("a refusal still carried content: %q", got.Content)
			}
			if !strings.Contains(got.Error, "application/pdf") {
				t.Errorf("the refusal does not name the stored MIME: %s", got.Error)
			}
			if !strings.Contains(got.Error, "artifact-reference parameter") {
				t.Errorf("the refusal does not name the by-reference route: %s", got.Error)
			}
			// The refusal still identifies WHAT it refused, so a model
			// that followed a fetch hint learns what it holds.
			if got.Ref != pdfID || got.MIME != "application/pdf" || got.TotalSizeBytes != int64(len(pdf)) {
				t.Errorf("refusal identity fields = (%q, %q, %d), want (%q, application/pdf, %d)",
					got.Ref, got.MIME, got.TotalSizeBytes, pdfID, len(pdf))
			}
			if got.Truncated {
				t.Error("a refusal reported truncated=true — a model would page into the same wall forever")
			}

			// (b) Multi-byte UTF-8 pages to a byte-exact reassembly.
			text := []byte(strings.Repeat("aé☃𝄞 line\n", 40))
			textID := readPathPut(t, store, owner, text, "text/plain; charset=utf-8")

			var assembled []byte
			offset := int64(0)
			for i := 0; ; i++ {
				if i > len(text)+1 {
					t.Fatal("paging did not terminate")
				}
				page := invokeFetch(t, cat, ctx, map[string]any{
					"ref": textID, "offset": offset, "max_bytes": 7,
				})
				if page.Error != "" {
					t.Fatalf("offset=%d: a valid-UTF-8 artifact was refused: %s", offset, page.Error)
				}
				// The reassembly invariant, asserted per page against the
				// stored bytes rather than only at the end.
				if want := string(text[page.Offset : page.Offset+page.ReturnedBytes]); page.Content != want {
					t.Fatalf("offset=%d: content = %q, want blob[%d:%d] = %q",
						offset, page.Content, page.Offset, page.Offset+page.ReturnedBytes, want)
				}
				assembled = append(assembled, page.Content...)
				if !page.Truncated {
					break
				}
				next := page.Offset + page.ReturnedBytes
				if next <= offset {
					t.Fatalf("LIVELOCK: next offset %d does not advance past %d", next, offset)
				}
				offset = next
			}
			if string(assembled) != string(text) {
				t.Fatalf("paged read assembled %d bytes, want %d", len(assembled), len(text))
			}

			// Identity propagation: another tenant reaching for the same
			// ids gets the shipped indistinguishable not-found, and never
			// bytes.
			for _, other := range []identity.Quadruple{
				readPathQuad("rp-tenant-b", "rp-user-a", "rp-session-a", "rp-run-2"),
				readPathQuad("rp-tenant-a", "rp-user-b", "rp-session-a", "rp-run-3"),
				readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-b", "rp-run-4"),
			} {
				for _, id := range []string{textID, pdfID} {
					out := invokeFetch(t, cat, readPathCtx(t, other), map[string]any{"ref": id})
					if out.Content != "" {
						t.Fatalf("SECURITY: %v read content across the isolation boundary: %q", other.Identity, out.Content)
					}
					if !strings.Contains(out.Error, "not found") {
						t.Fatalf("cross-identity read = %q, want the indistinguishable not-found shape", out.Error)
					}
				}
			}
		})
	}
}

// TestE2E_ArtifactReadPath_StoreErrorMidReadIsASoftFailure is failure
// mode 1 of three: the store answers an error mid-read. The tool
// surfaces it on the soft-error channel so the planner can re-plan,
// and never as content.
func TestE2E_ArtifactReadPath_StoreErrorMidReadIsASoftFailure(t *testing.T) {
	base := artifactsinmemStore(t)
	owner := readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-a", "rp-run-err")
	id := readPathPut(t, base, owner, []byte("readable text"), "text/plain")

	cat := readPathCatalog(t, failingGetStore{ArtifactStore: base}, 0, 0)
	got := invokeFetch(t, cat, readPathCtx(t, owner), map[string]any{"ref": id})
	if got.Error == "" {
		t.Fatal("a store error produced no error")
	}
	if got.Content != "" {
		t.Fatalf("a failed read carried content: %q", got.Content)
	}
	if !strings.Contains(got.Error, "forced Get failure") {
		t.Errorf("the soft error does not carry the store's cause: %s", got.Error)
	}
}

func artifactsinmemStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem driver: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// failingGetStore wraps the REAL store and forces Get to fail. It is the
// forced failure mode, not a re-implementation of the store.
type failingGetStore struct {
	artifacts.ArtifactStore
}

func (failingGetStore) Get(context.Context, artifacts.ArtifactScope, string) ([]byte, bool, error) {
	return nil, false, errors.New("forced Get failure")
}

// readPathPlanner emits one decision and then finishes, so the run loop
// records exactly one step.
type readPathPlanner struct {
	decision planner.Decision
	emitted  bool
}

func (p *readPathPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	if !p.emitted {
		p.emitted = true
		return p.decision, nil
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// runOneReadPathStep drives the REAL run loop over one decision and
// returns the recorded step, which is what the planner reads on its next
// turn.
func runOneReadPathStep(t *testing.T, exec steering.ToolExecutor, cat tools.ToolCatalog, q identity.Quadruple, decision planner.Decision) planner.Step {
	t.Helper()
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New())
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	traj := &trajectory.Trajectory{}
	if _, err := rl.Run(context.Background(), steering.RunSpec{
		Planner:      &readPathPlanner{decision: decision},
		Base:         planner.RunContext{Quadruple: q, Goal: "read path", Trajectory: traj, Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})},
		MaxSteps:     4,
		ToolExecutor: exec,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(traj.Steps) != 1 {
		t.Fatalf("trajectory carries %d steps, want 1", len(traj.Steps))
	}
	return traj.Steps[0]
}

// readPathRefCall is the CallTool a planner emits when it names an
// artifact id for the shipped reference-consuming example tool.
func readPathRefCall(id string) planner.CallTool {
	return planner.CallTool{
		CallID: "call_rp",
		Tool:   artifactstats.ToolName,
		Args:   json.RawMessage(fmt.Sprintf(`{"artifact":%q}`, id)),
	}
}

// readPathRefCatalog registers the reference consumer plus a tool whose
// own body errors — the unclassified control.
func readPathRefCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register %s: %v", artifactstats.ToolName, err)
	}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "explodes"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("upstream 503")
		},
	}); err != nil {
		t.Fatalf("register explodes: %v", err)
	}
	return cat
}

// stepErrorClass reads the class off a step's error observation slot.
func stepErrorClass(t *testing.T, label string, obs any) string {
	t.Helper()
	m, ok := obs.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want the error-observation map", label, obs)
	}
	if s, present := m[planner.ObservationClassKey]; present {
		str, ok := s.(string)
		if !ok {
			t.Fatalf("%s carries a non-string class %#v", label, s)
		}
		return str
	}
	return ""
}

// TestE2E_ArtifactReadPath_UnresolvableRefReachesTheNextPlannerTurn is
// leg (c): a resolution failure is not a dead run. The runtime converts
// it into the step's observation, CLASSIFIED, and the planner is asked
// for its next decision holding it.
func TestE2E_ArtifactReadPath_UnresolvableRefReachesTheNextPlannerTurn(t *testing.T) {
	for name, open := range readPathDrivers() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			owner := readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-a", "rp-run-class")

			// A real artifact exists under the run's triple, so the
			// failure below is specifically "that id does not resolve"
			// and not "this stack cannot resolve anything".
			realID := readPathPut(t, store, owner, []byte("real content"), "text/plain")

			cat := readPathRefCatalog(t)
			exec := dispatch.NewToolExecutor(cat, store, nil)

			step := runOneReadPathStep(t, exec, cat, owner, readPathRefCall("id_the_model_invented"))
			want := string(planner.ObservationClassArtifactRefNotFound)
			if got := stepErrorClass(t, "Step.Observation", step.Observation); got != want {
				t.Errorf("Step.Observation class = %q, want %q (obs = %#v)", got, want, step.Observation)
			}
			if got := stepErrorClass(t, "Step.LLMObservation", step.LLMObservation); got != want {
				t.Errorf("Step.LLMObservation class = %q, want %q", got, want)
			}
			// The run reached its terminal Finish — the classified
			// observation is a turn, not an abort.
			if step.Error != "" || step.Failure != nil {
				t.Errorf("the step was marked failed (%q / %#v); the run loop must keep going", step.Error, step.Failure)
			}

			// Failure mode 3: a tool's own error is the UNCLASSIFIED
			// control, so the class is proven to distinguish rather than
			// to decorate.
			control := runOneReadPathStep(t, exec, cat, owner,
				planner.CallTool{CallID: "c", Tool: "explodes", Args: json.RawMessage(`{}`)})
			if got := stepErrorClass(t, "control Step.Observation", control.Observation); got != "" {
				t.Errorf("a tool's own error acquired the class %q", got)
			}

			// A resolvable reference on the same stack still works, so
			// the assertions above are about a boundary rather than a
			// broken wiring.
			ok := runOneReadPathStep(t, exec, cat, owner, readPathRefCall(realID))
			if m, isMap := ok.Observation.(map[string]any); isMap && m["error"] != nil {
				t.Fatalf("a resolvable reference failed: %v", m["error"])
			}

			// Identity propagation: the SAME id under another tenant is
			// classified the same way, and no bytes cross.
			foreign := readPathQuad("rp-tenant-b", "rp-user-a", "rp-session-a", "rp-run-foreign")
			cross := runOneReadPathStep(t, exec, cat, foreign, readPathRefCall(realID))
			if got := stepErrorClass(t, "cross-tenant Step.Observation", cross.Observation); got != want {
				t.Errorf("cross-tenant class = %q, want %q", got, want)
			}
			if m, isMap := cross.Observation.(map[string]any); isMap {
				if msg, _ := m["error"].(string); strings.Contains(msg, "real content") {
					t.Fatalf("SECURITY: the refusal leaked the content it refused: %s", msg)
				}
			}
		})
	}
}

// TestE2E_ArtifactReadPath_ParallelBranchCarriesTheClass is leg (d): the
// parallel path and the single-call path agree. The class is stamped on
// the failing BRANCH, leaving a healthy sibling untouched.
func TestE2E_ArtifactReadPath_ParallelBranchCarriesTheClass(t *testing.T) {
	store := artifactsinmemStore(t)
	owner := readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-a", "rp-run-par")
	realID := readPathPut(t, store, owner, []byte("real content"), "text/plain")
	cat := readPathRefCatalog(t)
	exec := dispatch.NewToolExecutor(cat, store, nil)

	good := readPathRefCall(realID)
	good.CallID = "good"
	bad := readPathRefCall("id_the_model_invented")
	bad.CallID = "bad"

	step := runOneReadPathStep(t, exec, cat, owner,
		planner.CallParallel{Branches: []planner.CallTool{good, bad}})

	for label, obs := range map[string]any{"Observation": step.Observation, "LLMObservation": step.LLMObservation} {
		agg, ok := obs.(planner.ParallelObservation)
		if !ok {
			t.Fatalf("%s = %#v, want a ParallelObservation", label, obs)
		}
		if len(agg.Branches) != 2 {
			t.Fatalf("%s carries %d branches, want 2", label, len(agg.Branches))
		}
		if agg.Branches[0].ErrorClass != "" {
			t.Errorf("%s: the succeeding branch acquired the class %q", label, agg.Branches[0].ErrorClass)
		}
		if got := agg.Branches[1].ErrorClass; got != planner.ObservationClassArtifactRefNotFound {
			t.Errorf("%s: failing branch ErrorClass = %q, want %q",
				label, got, planner.ObservationClassArtifactRefNotFound)
		}
	}
}

// TestE2E_ArtifactReadPath_NoArtifactStoreWiredIsTheOperatorClass is
// failure mode 2: a stack with no artifact store. The class exists so a
// planner does not spend its step budget retrying a misconfiguration.
func TestE2E_ArtifactReadPath_NoArtifactStoreWiredIsTheOperatorClass(t *testing.T) {
	cat := readPathRefCatalog(t)
	exec := dispatch.NewToolExecutor(cat, nil, nil)
	q := readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-a", "rp-run-nostore")

	step := runOneReadPathStep(t, exec, cat, q, readPathRefCall("anything"))
	want := string(planner.ObservationClassArtifactResolverUnavailable)
	if got := stepErrorClass(t, "Step.Observation", step.Observation); got != want {
		t.Errorf("Step.Observation class = %q, want %q (obs = %#v)", got, want, step.Observation)
	}
	if got := stepErrorClass(t, "Step.LLMObservation", step.LLMObservation); got != want {
		t.Errorf("Step.LLMObservation class = %q, want %q", got, want)
	}
}

// TestE2E_ArtifactReadPath_ConcurrentStress crosses the whole surface
// under N=32 concurrent runs over ONE shared store and ONE shared
// executor across two tenants, mixing admissible reads, refusals,
// unresolvable references and cross-tenant misses. It asserts no
// cross-talk in either direction: no content bleed, and no CLASS bleed
// (a run's refusal must not be attributed to a sibling's success).
func TestE2E_ArtifactReadPath_ConcurrentStress(t *testing.T) {
	store := artifactsinmemStore(t)
	cat := readPathCatalog(t, store, 4096, 16384)
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register %s: %v", artifactstats.ToolName, err)
	}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "explodes"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("upstream 503")
		},
	}); err != nil {
		t.Fatalf("register explodes: %v", err)
	}
	exec := dispatch.NewToolExecutor(cat, store, nil)

	a := readPathQuad("rp-tenant-a", "rp-user-a", "rp-session-a", "rp-run-stress-a")
	b := readPathQuad("rp-tenant-b", "rp-user-b", "rp-session-b", "rp-run-stress-b")

	textA := []byte("tenant A é☃𝄞 payload")
	textB := []byte("tenant B é☃𝄞 payload")
	binA := []byte("\x89PNG\r\n\x1a\n\xDE\xAD\xBE\xEF")
	idTextA := readPathPut(t, store, a, textA, "text/plain")
	idTextB := readPathPut(t, store, b, textB, "text/plain")
	idBinA := readPathPut(t, store, a, binA, "image/png")

	const n = 32
	var wg sync.WaitGroup
	problems := make(chan string, n*2)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner, want, id := a, textA, idTextA
			if i%2 == 1 {
				owner, want, id = b, textB, idTextB
			}
			run := owner
			run.RunID = owner.RunID + "-" + strconv.Itoa(i)
			ctx := readPathCtx(t, run)

			// Its own text.
			if out := invokeFetch(t, cat, ctx, map[string]any{"ref": id}); out.Content != string(want) {
				problems <- "content bleed: " + out.Content + " / " + out.Error
			}
			// Tenant A's binary refuses for A and is not found for B.
			out := invokeFetch(t, cat, ctx, map[string]any{"ref": idBinA})
			if out.Content != "" {
				problems <- "a binary artifact returned content: " + out.Content
			}
			if out.Error == "" {
				problems <- "a binary artifact produced no error"
			}
			// An unresolvable reference through the executor.
			step := runOneReadPathStep(t, exec, cat, run, readPathRefCall("id_the_model_invented"))
			m, isMap := step.Observation.(map[string]any)
			if !isMap {
				problems <- fmt.Sprintf("observation = %#v, want the error map", step.Observation)
				return
			}
			if m[planner.ObservationClassKey] != string(planner.ObservationClassArtifactRefNotFound) {
				problems <- fmt.Sprintf("class bleed: %#v", m)
			}
		}(i)
	}
	wg.Wait()
	close(problems)
	for msg := range problems {
		t.Error(msg)
	}
}

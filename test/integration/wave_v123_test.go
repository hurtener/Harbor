// wave_v123_test.go — the v1.23 wave-end E2E (CLAUDE.md §17.7 step 5).
//
// The wave gave artifacts a read side (D-347). Four phases shipped it, and
// each already has its own integration test. This file is deliberately NOT a
// fifth copy of those: it asserts the COMPOSITIONS the four phases created
// between them, which no single phase's test can reach because each one owns
// only one end of the seam.
//
//   - 208 → 209: an artifact written with a `TaskID` stamp is readable through
//     `artifacts.get` by a caller who supplies NO task, and the response
//     reports the first writer's stamp as provenance ON the ref — the exact
//     shape "the task left the read key and became metadata" takes on the
//     wire. A cross-session and a cross-tenant id answer identically to an id
//     nobody minted.
//   - 208 → 210: a tool declaring an `artifactref.Ref` resolves a reference
//     written under a SIBLING run in the same session. The reconciled key is
//     what makes the dispatch resolver's task-absent scope correct rather than
//     merely convenient.
//   - 209 → 210: the truthful-bounds field set and the operator ceiling are
//     consistent across BOTH byte paths. They agree on the artifact's truth
//     (`total_size_bytes`, the ref digest, the reassembled content) and differ
//     in exactly one thing — the ceiling bounds the Protocol window and does
//     not bound the in-process resolution — which is what makes the ceiling an
//     operator EGRESS policy rather than a runtime capability limit.
//   - 210's substitution invariant, end to end and composed with 209: the
//     resolved value reaches none of the trajectory, the raw observation, the
//     LLM observation, a canonical event payload published over a REAL bus
//     opened on the REAL pattern redactor, or a log — while the SAME bytes are
//     served, in the same test, through `artifacts.get`. The sanctioned egress
//     working is what stops every absence above from being vacuous.
//   - 209 + 210 + 208 as one store: a `artifacts.delete` on the triple makes
//     BOTH read paths refuse, loudly and consistently.
//   - 211: a connection mutation lands on the caller's own tenant's
//     registration and is refused for another tenant's — asserted over the
//     same composed mux that serves the artifact reads, so the registry scope
//     and the artifact scope are proven to read the same verified identity.
//
// Every component on every seam is real (§17.3): the in-memory artifacts
// driver, the in-memory event bus opened over `audit/drivers/patterns`, the
// in-memory state store, a real `tasks` registry, the real `protocol`
// artifacts + MCP + control surfaces, the real REST control transport over
// `httptest`, the real `tools.ToolCatalog` with the lifecycle shell live, the
// production `dispatch.ToolExecutor`, the shipped `artifactstats` consumer,
// and the real process-global `mcp.Registry` behind the real
// `mcpconsole.RegistryAccessor`.
//
// Identity propagation is asserted on every leg. The closing test is the
// §17.3 concurrency stress: N=20 workers across 2 tenants driving all three
// surfaces against ONE shared stack, with a goroutine baseline after teardown.
// Runs under -race.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
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
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/protocol"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const (
	v123TenantA = "v123-tenant-a"
	v123TenantB = "v123-tenant-b"
	v123User    = "v123-user"
	v123Session = "v123-session"

	// v123ProducerRun is the run that WRITES the artifact; v123ConsumerRun
	// is a different run in the same session that reads it. The pair is the
	// whole point of the 208 seam: before the read key was reconciled, the
	// second could not resolve what the first stored.
	v123ProducerRun = "v123-run-producer"
	v123ConsumerRun = "v123-run-consumer"

	v123ServerA = "v123-srv-a"
	v123ServerB = "v123-srv-b"
	v123Agent   = "agent-v123"

	// v123FetchDefault / v123FetchHard are the OPERATOR bounds this stack
	// configures. They are deliberately tiny so the ceiling is reachable
	// against a modest artifact instead of a megabyte one — the behaviour
	// under test is the arithmetic and its reporting, not the magnitude.
	v123FetchDefault = 64
	v123FetchHard    = 128
)

// v123Marker is the planted content. It appears nowhere else in the
// repository, so a substring search over any surface is decisive.
const v123Marker = "V123-RESOLVED-ARTIFACT-CONTENT-4e9a17dc"

// v123Body is the artifact's stored content. The marker leads, so the
// base64 rendering of the whole body also LEADS with the base64 of the
// marker — which is what lets v123AssertAbsent catch a []byte field that
// carried the content through `encoding/json` as well as a string one.
var v123Body = []byte(v123Marker + "\n" + strings.Repeat("harbor-v123-filler-line\n", 24))

// v123B64Head is the first 40 characters of the body's base64 rendering.
var v123B64Head = base64.StdEncoding.EncodeToString(v123Body)[:40]

// v123Digest is the hex SHA-256 of the stored body — the single value both
// byte paths are checked against.
var v123Digest = func() string {
	sum := sha256.Sum256(v123Body)
	return hex.EncodeToString(sum[:])
}()

// v123Stack is ONE composed runtime: one artifact store, one bus, one MCP
// registry, one catalog, one executor and one HTTP mux serving the control,
// artifacts and MCP surfaces. Sharing a single instance across every test and
// every concurrent worker is the point — N callers against N instances would
// prove nothing about isolation.
type v123Stack struct {
	store    artifacts.ArtifactStore
	bus      events.EventBus
	registry *mcpdrv.Registry
	catalog  tools.ToolCatalog
	exec     steering.ToolExecutor
	srv      *httptest.Server
	// logs captures everything the executor logs during a dispatch. The
	// slog handler serialises its own writes; the buffer is only READ after
	// every writer has been joined.
	logs *bytes.Buffer
}

// v123CarrierAdmin seats each request's OWN carrier triple as its verified
// identity, plus the admin claim the MCP connection write and the artifact
// delete require.
//
// It seats identity per-request rather than baking one in, so ONE mux serves
// every tenant: a stress across N tenants against N handler instances does not
// exercise the property being asserted. A request with no tenant header is
// passed through untouched so the transport's own carrier middleware refuses
// it — the identity-mandatory failure mode has to come from production code,
// not from this wrapper.
//
// Granting the admin claim to EVERY caller makes the cross-tenant assertions
// below stricter rather than looser: no scope claim widens an artifact byte
// read, and the connection write's scope is its caller's tenant rather than a
// claim, so each refusal here is a refusal an ADMIN received.
func v123CarrierAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Header.Get("X-Harbor-Tenant")
		if tenant == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, err := identity.WithVerified(r.Context(), identity.Identity{
			TenantID:  tenant,
			UserID:    r.Header.Get("X-Harbor-User"),
			SessionID: r.Header.Get("X-Harbor-Session"),
		})
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx = protoauth.WithScopes(ctx, []protoauth.Scope{protoauth.ScopeAdmin})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newV123Stack wires the wave's whole surface with real drivers throughout.
func newV123Stack(t *testing.T) *v123Stack {
	t.Helper()
	ctx := context.Background()

	red := auditpatterns.New()
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := artifactsinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	stateStore, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close(context.Background()) })

	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store:    stateStore,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	controlSurface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	artifactsSurface, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:                store,
		Redactor:             red,
		Bus:                  bus,
		Clock:                time.Now,
		DriverName:           "inmem",
		MaxBodyBytes:         1 << 20,
		FetchDefaultMaxBytes: v123FetchDefault,
		FetchHardMaxBytes:    v123FetchHard,
	})
	if err != nil {
		t.Fatalf("protocol.NewArtifactsSurface: %v", err)
	}

	registry := mcpdrv.NewRegistry()
	regCtx := v123Ctx(t, v123TenantA)
	for _, reg := range []struct {
		name   string
		tenant string
	}{
		{v123ServerA, v123TenantA},
		{v123ServerB, v123TenantB},
	} {
		// p211Provider is the sibling file's deterministic MCP provider. It
		// sits BEHIND the registry, not on the seam under test — the
		// registry, the accessor, the surface and the transport are all the
		// production types.
		if rerr := registry.Register(regCtx, mcpdrv.ServerRegistration{
			Provider:     &p211Provider{id: tools.ToolSourceID(reg.name)},
			Transport:    "streamable-http",
			URLOrCommand: "https://" + reg.name + ".example.com/rpc",
			InitialState: mcpdrv.ServerStateOnline,
			Owner:        toolauth.Owner{Tenant: reg.tenant, Agent: v123Agent},
		}); rerr != nil {
			t.Fatalf("register %s: %v", reg.name, rerr)
		}
	}
	accessor, err := mcpconsole.NewRegistryAccessor(registry)
	if err != nil {
		t.Fatalf("mcpconsole.NewRegistryAccessor: %v", err)
	}
	mcpSurface, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:           accessor,
		OAuth:         p211NoopOAuth{},
		Redactor:      red,
		Bus:           bus,
		AgentResolver: serve.NewAgentResolverAdapter(nil, v123Agent),
	})
	if err != nil {
		t.Fatalf("protocol.NewMCPSurface: %v", err)
	}

	mux, err := transports.NewMux(controlSurface, bus,
		transports.WithoutValidator(),
		transports.WithArtifactsSurface(artifactsSurface),
		transports.WithMCPSurface(mcpSurface),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(v123CarrierAdmin(mux))
	t.Cleanup(srv.Close)

	// The catalog carries the bus, so the universal tool-lifecycle shell
	// emits tool.invoked / .completed / .failed for every dispatch here
	// exactly as it does in production.
	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	if err := artifactstats.Register(cat); err != nil {
		t.Fatalf("register %s: %v", artifactstats.ToolName, err)
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return &v123Stack{
		store:    store,
		bus:      bus,
		registry: registry,
		catalog:  cat,
		exec:     dispatch.NewToolExecutor(cat, store, nil, dispatch.WithLogger(logger)),
		srv:      srv,
		logs:     logs,
	}
}

// v123Ctx seats a complete triple under tenant as an established identity.
func v123Ctx(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: tenant, UserID: v123User, SessionID: v123Session,
	})
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}

func v123Quad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		RunID:    run,
	}
}

// v123Scope is the wire scope map for a caller. task is deliberately a
// separate argument so a test can spell "the reader supplies NO task".
func v123Scope(tenant, user, session string) map[string]any {
	return map[string]any{"tenant": tenant, "user": user, "session": session}
}

// v123Put stores body under the quadruple's TRIPLE plus its run as the
// `TaskID` stamp — the shape a producing run writes — and returns the ref.
func v123Put(t *testing.T, store artifacts.ArtifactStore, q identity.Quadruple, body []byte) artifacts.ArtifactRef {
	t.Helper()
	ref, err := store.PutBytes(context.Background(), artifacts.ArtifactScope{
		TenantID:  q.TenantID,
		UserID:    q.UserID,
		SessionID: q.SessionID,
		TaskID:    q.RunID,
	}, body, artifacts.PutOpts{
		Namespace: "v123",
		Filename:  "planted.txt",
		MimeType:  "text/plain; charset=utf-8",
	})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref
}

// v123Dispatch runs the pass-by-reference tool call the planner would emit —
// an id, never content — under q's identity, and returns the executor's raw
// and LLM-facing observations.
func v123Dispatch(t *testing.T, st *v123Stack, q identity.Quadruple, refID string) (any, any, error) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{Quadruple: q, Trajectory: &trajectory.Trajectory{}, Catalog: tools.NewPlannerView(st.catalog, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})}
	return st.exec.ExecuteDecision(ctx, rc, planner.CallTool{
		CallID: "call_v123_1",
		Tool:   artifactstats.ToolName,
		Args:   json.RawMessage(fmt.Sprintf(`{"artifact":%q}`, refID)),
	})
}

// v123Stats decodes the example consumer's typed result, failing when the
// dispatch produced anything else.
func v123Stats(t *testing.T, raw any) artifactstats.StatsResult {
	t.Helper()
	out, ok := raw.(artifactstats.StatsResult)
	if !ok {
		t.Fatalf("observation = %T, want artifactstats.StatsResult", raw)
	}
	return out
}

// v123AssertAbsent fails when surface carries the planted content in either of
// the two shapes a Go value can reach a JSON surface in: the plain string, or
// the body's base64 rendering (what `encoding/json` emits for a []byte field).
func v123AssertAbsent(t *testing.T, label, surface string) {
	t.Helper()
	if strings.Contains(surface, v123Marker) {
		t.Errorf("%s carries the resolved value verbatim: %s", label, surface)
	}
	if strings.Contains(surface, v123B64Head) {
		t.Errorf("%s carries the resolved value base64-encoded: %s", label, surface)
	}
}

// v123Get calls artifacts.get on the shared server as the identity the scope
// names, and returns the status plus the decoded body.
func v123Get(t *testing.T, st *v123Stack, scope map[string]any, id string, extra map[string]any) (int, map[string]any) {
	t.Helper()
	payload := map[string]any{"scope": scope, "id": id}
	for k, v := range extra {
		payload[k] = v
	}
	return callArtifacts(t, st.srv.URL, methods.MethodArtifactsGet, payload)
}

// --- 208 → 209 --------------------------------------------------------------

// TestE2E_WaveV123_TaskStampedWriteServesThroughArtifactsGet is the seam the
// read-key reconciliation opened and the byte-serving method walked through: a
// producing run stamps its own id on the write, and a LATER caller — a
// different run, supplying no task at all — reads the bytes over the wire.
//
// The response's ref is where the two phases actually meet. It reports the
// FIRST writer's stamp, so the task did not vanish; it became provenance ON
// the ref rather than part of what the read resolved against.
func TestE2E_WaveV123_TaskStampedWriteServesThroughArtifactsGet(t *testing.T) {
	st := newV123Stack(t)
	producer := v123Quad(v123TenantA, v123User, v123Session, v123ProducerRun)
	ref := v123Put(t, st.store, producer, v123Body)

	// The reader supplies the TRIPLE and no task. The window is the
	// operator ceiling — this test is about WHICH artifact resolves, not
	// about how much of it one call may carry (that is the bounds test).
	scope := v123Scope(v123TenantA, v123User, v123Session)
	status, body := v123Get(t, st, scope, ref.ID, map[string]any{"max_bytes": v123FetchHard})
	if status != http.StatusOK {
		t.Fatalf("task-absent artifacts.get: status = %d, body=%v — the reconciled read key "+
			"is what makes a task-less read resolve a task-stamped write", status, body)
	}
	if got := artifactsGetB64(t, body); string(got) != string(v123Body[:v123FetchHard]) {
		t.Fatalf("content = %q, want the stored body's first %d bytes", got, v123FetchHard)
	}
	if got := artifactsGetNum(t, body, "total_size_bytes"); got != int64(len(v123Body)) {
		t.Fatalf("total_size_bytes = %d, want %d", got, len(v123Body))
	}

	wireRef, ok := body["ref"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no ref: %v", body)
	}
	wireScope, ok := wireRef["scope"].(map[string]any)
	if !ok {
		t.Fatalf("ref carries no scope: %v", wireRef)
	}
	if got := fmt.Sprint(wireScope["task"]); got != v123ProducerRun {
		t.Fatalf("ref.scope.task = %q, want the producing run %q — the stamp is provenance "+
			"the read must report, not a field the read may drop", got, v123ProducerRun)
	}
	if got := fmt.Sprint(wireRef["sha256"]); got != v123Digest {
		t.Fatalf("ref.sha256 = %q, want %q", got, v123Digest)
	}
}

// TestE2E_WaveV123_OutOfScopeIDsAreIndistinguishableOverTheWire is the failure
// mode the widening must not have introduced: the read key lost a field, the
// isolation boundary did not move. A cross-session id, a cross-tenant id and
// an id nobody ever minted answer IDENTICALLY — the difference between them is
// exactly what a prober harvests.
func TestE2E_WaveV123_OutOfScopeIDsAreIndistinguishableOverTheWire(t *testing.T) {
	st := newV123Stack(t)
	owner := v123Quad(v123TenantA, v123User, v123Session, v123ProducerRun)
	ref := v123Put(t, st.store, owner, v123Body)

	// The control: the owning triple reads it, so the refusals below are a
	// boundary rather than a broken store.
	okStatus, okBody := v123Get(t, st, v123Scope(v123TenantA, v123User, v123Session), ref.ID, nil)
	if okStatus != http.StatusOK {
		t.Fatalf("the owning triple could not read its own artifact: status=%d body=%v", okStatus, okBody)
	}

	// The baseline refusal: an id nobody minted.
	unknownStatus, unknownBody := v123Get(t, st,
		v123Scope(v123TenantA, v123User, v123Session), "v123_ffffffffffff", nil)
	if unknownStatus != http.StatusNotFound {
		t.Fatalf("unknown id: status = %d, want 404 (body=%v)", unknownStatus, unknownBody)
	}

	for _, tc := range []struct {
		name                  string
		tenant, user, session string
	}{
		{"another session", v123TenantA, v123User, "v123-session-other"},
		{"another user", v123TenantA, "v123-user-other", v123Session},
		{"another tenant", v123TenantB, v123User, v123Session},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := v123Get(t, st, v123Scope(tc.tenant, tc.user, tc.session), ref.ID, nil)
			if status != unknownStatus {
				t.Fatalf("%s answers %d while an unknown id answers %d — the difference "+
					"reveals the artifact's existence (body=%v)", tc.name, status, unknownStatus, body)
			}
			if fmt.Sprint(body["code"]) != fmt.Sprint(unknownBody["code"]) {
				t.Fatalf("%s code %v differs from the unknown-id code %v",
					tc.name, body["code"], unknownBody["code"])
			}
			v123AssertAbsent(t, tc.name+" refusal", mustJSON(t, "refusal", body))
		})
	}
}

// --- 208 → 210 --------------------------------------------------------------

// TestE2E_WaveV123_SiblingRunReferenceResolvesAtDispatch is the second seam the
// reconciled key opened. The dispatch resolver is closed over the run's TRIPLE
// with the task deliberately absent; that is only correct because the store's
// read key is the triple. A reference written by one run and named by another
// run of the same session must resolve — and the same reference named from any
// other triple must not, loudly and without leaking a byte.
func TestE2E_WaveV123_SiblingRunReferenceResolvesAtDispatch(t *testing.T) {
	st := newV123Stack(t)
	producer := v123Quad(v123TenantA, v123User, v123Session, v123ProducerRun)
	ref := v123Put(t, st.store, producer, v123Body)

	consumer := v123Quad(v123TenantA, v123User, v123Session, v123ConsumerRun)
	raw, _, err := v123Dispatch(t, st, consumer, ref.ID)
	if err != nil {
		t.Fatalf("a sibling run in the same session could not resolve the reference: %v", err)
	}
	out := v123Stats(t, raw)
	if out.SizeBytes != len(v123Body) {
		t.Fatalf("the tool measured %d bytes, want %d — it was not handed the stored content",
			out.SizeBytes, len(v123Body))
	}
	if out.SHA256 != v123Digest {
		t.Fatalf("tool digest = %q, want %q", out.SHA256, v123Digest)
	}
	if out.ArtifactID != ref.ID {
		t.Fatalf("tool saw ref %q, want %q", out.ArtifactID, ref.ID)
	}

	for _, tc := range []struct {
		name string
		q    identity.Quadruple
	}{
		{"another session", v123Quad(v123TenantA, v123User, "v123-session-other", "v123-run-x")},
		{"another user", v123Quad(v123TenantA, "v123-user-other", v123Session, "v123-run-y")},
		{"another tenant", v123Quad(v123TenantB, v123User, v123Session, "v123-run-z")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rawOut, llmOut, derr := v123Dispatch(t, st, tc.q, ref.ID)
			if derr == nil {
				t.Fatalf("an out-of-scope reference resolved: raw=%v llm=%v", rawOut, llmOut)
			}
			if rawOut != nil || llmOut != nil {
				t.Errorf("a refused dispatch produced observations: raw=%v llm=%v", rawOut, llmOut)
			}
			v123AssertAbsent(t, tc.name+" dispatch refusal", derr.Error())
		})
	}
}

// --- 209 → 210 --------------------------------------------------------------

// TestE2E_WaveV123_BoundsAgreeAcrossBothByteReadPaths is the consistency claim
// between the two byte paths the wave shipped.
//
// They must agree on the artifact's TRUTH: the size the Protocol reports, the
// size the tool measures and the digest on the ref are one artifact's, and the
// windows the Protocol serves reassemble — using only the offsets the response
// itself reports — into bytes whose digest is that same one.
//
// They must differ in exactly ONE thing: the operator ceiling bounds a
// Protocol window and does NOT bound the in-process resolution. That asymmetry
// is the substance of "the ceiling is an egress policy": it governs how much a
// remote caller may pull in one call, not how much content the runtime may
// hand a tool that never re-emits it.
func TestE2E_WaveV123_BoundsAgreeAcrossBothByteReadPaths(t *testing.T) {
	st := newV123Stack(t)
	producer := v123Quad(v123TenantA, v123User, v123Session, v123ProducerRun)
	ref := v123Put(t, st.store, producer, v123Body)
	scope := v123Scope(v123TenantA, v123User, v123Session)

	if len(v123Body) <= v123FetchHard {
		t.Fatalf("the fixture body is %d bytes, which does not exceed the %d-byte ceiling — "+
			"every bound assertion below would be vacuous", len(v123Body), v123FetchHard)
	}

	// The operator DEFAULT applies when the caller names no bound.
	dStatus, dBody := v123Get(t, st, scope, ref.ID, nil)
	if dStatus != http.StatusOK {
		t.Fatalf("default-bound get: status = %d, body=%v", dStatus, dBody)
	}
	if got := artifactsGetNum(t, dBody, "returned_bytes"); got != v123FetchDefault {
		t.Fatalf("returned_bytes with no caller bound = %d, want the operator default %d",
			got, v123FetchDefault)
	}
	if !artifactsGetTruncated(t, dBody) {
		t.Fatal("a defaulted read of an oversize artifact reported truncated=false")
	}

	// A caller asking far above the ceiling is SERVED AT the ceiling, never
	// refused — and says so through the same fields.
	cStatus, cBody := v123Get(t, st, scope, ref.ID, map[string]any{"max_bytes": 1 << 20})
	if cStatus != http.StatusOK {
		t.Fatalf("above-ceiling get: status = %d, want 200 — the ceiling bounds, it does not "+
			"refuse (body=%v)", cStatus, cBody)
	}
	if got := artifactsGetNum(t, cBody, "returned_bytes"); got != v123FetchHard {
		t.Fatalf("returned_bytes above the ceiling = %d, want the hard ceiling %d", got, v123FetchHard)
	}
	if got := artifactsGetNum(t, cBody, "total_size_bytes"); got != int64(len(v123Body)) {
		t.Fatalf("total_size_bytes = %d, want %d — a bounded read must still tell the truth "+
			"about the whole artifact", got, len(v123Body))
	}

	// Page the artifact using ONLY the offsets the responses report. A
	// window arithmetic bug shows up here as a short, long or misaligned
	// reassembly rather than as a field comparison.
	var page bytes.Buffer
	offset := int64(0)
	windows := 0
	for {
		wStatus, wBody := v123Get(t, st, scope, ref.ID, map[string]any{
			"offset": offset, "max_bytes": v123FetchHard,
		})
		if wStatus != http.StatusOK {
			t.Fatalf("window at offset %d: status = %d, body=%v", offset, wStatus, wBody)
		}
		if got := artifactsGetNum(t, wBody, "offset"); got != offset {
			t.Fatalf("window echoed offset %d, want %d", got, offset)
		}
		chunk := artifactsGetB64(t, wBody)
		if got := artifactsGetNum(t, wBody, "returned_bytes"); got != int64(len(chunk)) {
			t.Fatalf("returned_bytes = %d but the window carried %d bytes", got, len(chunk))
		}
		page.Write(chunk)
		windows++
		if windows > 32 {
			t.Fatal("the paging loop did not terminate within 32 windows")
		}
		if !artifactsGetTruncated(t, wBody) {
			// The LAST window reports truncated=false — the property a
			// `returned < total` implementation gets wrong.
			break
		}
		offset += int64(len(chunk))
		if len(chunk) == 0 {
			t.Fatal("a truncated window returned zero bytes — the loop cannot make progress")
		}
	}
	if windows < 2 {
		t.Fatalf("the artifact paged in %d window(s); the loop proved nothing about paging", windows)
	}
	reassembled := page.Bytes()
	sum := sha256.Sum256(reassembled)
	if hex.EncodeToString(sum[:]) != v123Digest {
		t.Fatalf("the reassembled windows digest to %s, want %s",
			hex.EncodeToString(sum[:]), v123Digest)
	}

	// The in-process path is NOT bounded by the operator ceiling: the same
	// artifact reaches the consumer whole, and the two surfaces agree on the
	// digest of what they served.
	consumer := v123Quad(v123TenantA, v123User, v123Session, v123ConsumerRun)
	raw, _, err := v123Dispatch(t, st, consumer, ref.ID)
	if err != nil {
		t.Fatalf("pass-by-reference dispatch: %v", err)
	}
	out := v123Stats(t, raw)
	if int64(out.SizeBytes) != artifactsGetNum(t, cBody, "total_size_bytes") {
		t.Fatalf("the tool measured %d bytes while the Protocol reports a total of %d — "+
			"the two read paths disagree about the artifact", out.SizeBytes,
			artifactsGetNum(t, cBody, "total_size_bytes"))
	}
	if out.SizeBytes <= v123FetchHard {
		t.Fatalf("the tool received %d bytes, at or under the %d-byte egress ceiling — the "+
			"ceiling has leaked into the in-process resolution", out.SizeBytes, v123FetchHard)
	}
	if out.SHA256 != v123Digest {
		t.Fatalf("the tool's digest %q differs from the served bytes' %q", out.SHA256, v123Digest)
	}
}

// --- the substitution invariant, composed with the byte-serving method ------

// TestE2E_WaveV123_SubstitutionInvariantHoldsWhileTheProtocolServesTheBytes is
// the wave's load-bearing composition. Both halves run against ONE store in
// ONE test:
//
//   - the in-process resolution hands the tool the bytes and leaves NO trace of
//     them in the trajectory, either observation, any canonical event payload
//     or envelope published over the real bus, or any log record;
//   - the Protocol read serves those same bytes to the caller who asked for
//     them.
//
// The second half is what makes the first non-vacuous at the wave level: the
// bytes demonstrably CAN leave the store through the sanctioned door, so their
// absence from the dispatch's record is a property of the dispatch rather than
// of an empty artifact.
//
// Scope note, so a later reader does not over-read this arm: it asserts the
// ARRIVAL side over the composed record. No `artifactref.Ref` reaches any of
// these surfaces in this shape — dispatch hands `Invoke` the model's own
// argument JSON unrewritten and binds on the decoded value — so a carrier
// whose `MarshalJSON` leaked content would not be caught HERE. That mechanism
// is pinned directly by the `internal/tools/artifactref` projection tests
// (verified: mutating `Ref.MarshalJSON` to emit content leaves this file
// green and those red). What this file guards is the composition the unit
// tests cannot see — that no surface the runtime actually publishes during a
// real dispatch, over a real bus and the real redactor, carries the value.
func TestE2E_WaveV123_SubstitutionInvariantHoldsWhileTheProtocolServesTheBytes(t *testing.T) {
	st := newV123Stack(t)
	producer := v123Quad(v123TenantA, v123User, v123Session, v123ProducerRun)
	ref := v123Put(t, st.store, producer, v123Body)
	consumer := v123Quad(v123TenantA, v123User, v123Session, v123ConsumerRun)

	// Subscribe BEFORE the dispatch so nothing published during it is missed.
	sub, err := st.bus.Subscribe(v123Ctx(t, v123TenantA), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	call := planner.CallTool{
		CallID: "call_v123_inv",
		Tool:   artifactstats.ToolName,
		Args:   json.RawMessage(fmt.Sprintf(`{"artifact":%q}`, ref.ID)),
	}
	ctx, err := identity.WithRun(context.Background(), consumer.Identity, consumer.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{Quadruple: consumer, Trajectory: &trajectory.Trajectory{}, Catalog: tools.NewPlannerView(st.catalog, tools.CatalogFilter{TenantID: consumer.TenantID, UserID: consumer.UserID, SessionID: consumer.SessionID})}
	raw, llmObs, err := st.exec.ExecuteDecision(ctx, rc, call)
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}

	// The routing worked — without this every absence below is vacuous.
	out := v123Stats(t, raw)
	if out.SizeBytes != len(v123Body) || out.SHA256 != v123Digest {
		t.Fatalf("the tool did not receive the stored content: %+v", out)
	}

	v123AssertAbsent(t, "the raw observation", mustJSON(t, "raw observation", raw))
	v123AssertAbsent(t, "the LLM observation", mustJSON(t, "llm observation", llmObs))

	rc.Trajectory.Steps = append(rc.Trajectory.Steps, trajectory.Step{
		Action:         call,
		Observation:    raw,
		LLMObservation: llmObs,
	})
	serialised, err := rc.Trajectory.Serialize()
	if err != nil {
		t.Fatalf("Trajectory.Serialize: %v", err)
	}
	v123AssertAbsent(t, "the trajectory", string(serialised))
	if !strings.Contains(string(serialised), ref.ID) {
		t.Errorf("the trajectory dropped the reference id the model authored: %s", serialised)
	}

	// Every canonical event the dispatch published. The lifecycle shell emits
	// at least tool.invoked and tool.completed for a successful call, so the
	// arm waits (bounded, no sleep) for that pair before draining — a fast
	// drain that saw zero events would "prove" the absence trivially.
	const wantAtLeast = 2
	seen := 0
	deadline := time.After(5 * time.Second)
collect:
	for seen < wantAtLeast {
		select {
		case ev, open := <-sub.Events():
			if !open {
				break collect
			}
			seen++
			v123AssertAbsent(t, "event "+string(ev.Type)+" payload", mustJSON(t, string(ev.Type), ev.Payload))
			v123AssertAbsent(t, "event "+string(ev.Type)+" envelope", mustJSON(t, "envelope", ev))
		case <-deadline:
			break collect
		}
	}
	// Anything already queued beyond the expected pair.
drain:
	for {
		select {
		case ev, open := <-sub.Events():
			if !open {
				break drain
			}
			seen++
			v123AssertAbsent(t, "event "+string(ev.Type)+" payload", mustJSON(t, string(ev.Type), ev.Payload))
			v123AssertAbsent(t, "event "+string(ev.Type)+" envelope", mustJSON(t, "envelope", ev))
		default:
			break drain
		}
	}
	if seen < wantAtLeast {
		t.Fatalf("observed %d lifecycle events, want at least %d; the event arm proved nothing",
			seen, wantAtLeast)
	}

	// The sanctioned egress: the SAME bytes, served to the caller who asked.
	// The marker leads the body, so the head window carries it — the window
	// bound is the operator's, and it is not what this arm is about.
	status, body := v123Get(t, st, v123Scope(v123TenantA, v123User, v123Session), ref.ID,
		map[string]any{"max_bytes": v123FetchHard})
	if status != http.StatusOK {
		t.Fatalf("artifacts.get: status = %d, body=%v", status, body)
	}
	served := artifactsGetB64(t, body)
	if !strings.Contains(string(served), v123Marker) {
		t.Fatalf("artifacts.get did not serve the planted content — every absence asserted " +
			"above would hold for an artifact nobody could read either")
	}

	v123AssertAbsent(t, "a log record", st.logs.String())
}

// TestE2E_WaveV123_DeletedArtifactRefusesOnBothReadPaths is the composed
// failure mode (§17.3 item 3). One store backs both byte paths, so a delete on
// the isolation triple — issued with no task, against an artifact stamped with
// one — must make BOTH refuse. A path that kept serving after the delete would
// be reading a copy the reconciled key was supposed to have swept.
func TestE2E_WaveV123_DeletedArtifactRefusesOnBothReadPaths(t *testing.T) {
	st := newV123Stack(t)
	producer := v123Quad(v123TenantA, v123User, v123Session, v123ProducerRun)
	ref := v123Put(t, st.store, producer, v123Body)
	consumer := v123Quad(v123TenantA, v123User, v123Session, v123ConsumerRun)
	scope := v123Scope(v123TenantA, v123User, v123Session)

	// Both paths read it first, so the refusals below are the delete's doing.
	if status, body := v123Get(t, st, scope, ref.ID, nil); status != http.StatusOK {
		t.Fatalf("pre-delete artifacts.get: status = %d, body=%v", status, body)
	}
	if _, _, err := v123Dispatch(t, st, consumer, ref.ID); err != nil {
		t.Fatalf("pre-delete dispatch: %v", err)
	}

	dStatus, dBody := callArtifacts(t, st.srv.URL, methods.MethodArtifactsDelete, map[string]any{
		"scope": scope, "id": ref.ID,
	})
	if dStatus != http.StatusOK {
		t.Fatalf("artifacts.delete: status = %d, body=%v", dStatus, dBody)
	}
	if deleted, ok := dBody["deleted"].(bool); !ok || !deleted {
		t.Fatalf("artifacts.delete reported deleted=%v — a task-less delete must sweep a "+
			"task-stamped write", dBody["deleted"])
	}

	gStatus, gBody := v123Get(t, st, scope, ref.ID, nil)
	if gStatus != http.StatusNotFound {
		t.Fatalf("post-delete artifacts.get: status = %d, want 404 (body=%v)", gStatus, gBody)
	}
	raw, llmObs, err := v123Dispatch(t, st, consumer, ref.ID)
	if err == nil {
		t.Fatalf("post-delete dispatch resolved: raw=%v llm=%v", raw, llmObs)
	}
	if !strings.Contains(err.Error(), ref.ID) {
		t.Errorf("post-delete dispatch error = %v, want it to name the reference id", err)
	}
	v123AssertAbsent(t, "the post-delete refusal", err.Error())
	if raw != nil || llmObs != nil {
		t.Errorf("a refused dispatch produced observations: raw=%v llm=%v", raw, llmObs)
	}
}

// --- 211, through the composed stack ----------------------------------------

// v123SetTrustAs drives mcp.servers.set_raw_html_trust over the SAME server
// that serves the artifact reads, as the full triple id names. Body identity
// and carrier identity agree, so the shared body-scope gate passes and the
// tenant the registry resolver reads is the caller's own verified one.
func v123SetTrustAs(t *testing.T, st *v123Stack, id identity.Identity, name string, trusted bool) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"identity": map[string]any{
			"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID,
		},
		"name":    name,
		"trusted": trusted,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := postCarrierJSON(
		st.srv.URL+"/v1/control/"+string(methods.MethodMCPServersSetRawHTMLTrust),
		body, id.TenantID, id.UserID, id.SessionID)
	if err != nil {
		t.Fatalf("POST set_raw_html_trust: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// v123SetTrust is v123SetTrustAs under the suite's shared user and session.
func v123SetTrust(t *testing.T, st *v123Stack, tenant, name string, trusted bool) (int, map[string]any) {
	t.Helper()
	return v123SetTrustAs(t, st,
		identity.Identity{TenantID: tenant, UserID: v123User, SessionID: v123Session},
		name, trusted)
}

// v123Trust reads the live flag back through the read projection, which stays
// bare-name and owner-blind.
func v123Trust(t *testing.T, st *v123Stack, name string) bool {
	t.Helper()
	view, err := st.registry.GetServer(v123Ctx(t, v123TenantB), name)
	if err != nil {
		t.Fatalf("GetServer(%q): %v", name, err)
	}
	return view.RawHTMLTrusted
}

// TestE2E_WaveV123_ConnectionWriteScopesToTheCallersOwnTenant asserts the
// owner-scoped connection write through the composed stack rather than against
// the registry alone: the same verified identity that the artifact surface
// reads is the one the registry resolver scopes on, and the refusal it gives a
// foreign tenant discloses no more than an unregistered name would.
//
// The last leg is the composition that makes this a WAVE test: the caller that
// was refused the other tenant's connection write still reads its own artifact
// on the same mux in the same breath. The refusal is a scope boundary, not a
// broken stack.
func TestE2E_WaveV123_ConnectionWriteScopesToTheCallersOwnTenant(t *testing.T) {
	st := newV123Stack(t)

	// Tenant A owns v123ServerA; tenant B's write against it is refused and
	// the live flag is untouched.
	status, body := v123SetTrust(t, st, v123TenantB, v123ServerA, true)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant connection write: status = %d, want 404 (body=%v)", status, body)
	}
	if v123Trust(t, st, v123ServerA) {
		t.Fatal("a cross-tenant write changed the owning tenant's live flag")
	}

	// The refusal discloses nothing: an unregistered name answers alike.
	aStatus, aBody := v123SetTrust(t, st, v123TenantB, "v123-nobody-registered-this", true)
	if aStatus != status || fmt.Sprint(aBody["code"]) != fmt.Sprint(body["code"]) {
		t.Fatalf("an unregistered name answers %d/%v while another tenant's registration "+
			"answers %d/%v — the difference reveals the registration's existence",
			aStatus, aBody["code"], status, body["code"])
	}

	// The owning tenant's write lands, on its own registration only.
	oStatus, oBody := v123SetTrust(t, st, v123TenantA, v123ServerA, true)
	if oStatus != http.StatusOK {
		t.Fatalf("owning-tenant write: status = %d, body=%v", oStatus, oBody)
	}
	if trusted, ok := oBody["trusted"].(bool); !ok || !trusted {
		t.Fatalf("owning-tenant response = %v", oBody)
	}
	if !v123Trust(t, st, v123ServerA) {
		t.Fatal("the owning tenant's write did not land")
	}
	if v123Trust(t, st, v123ServerB) {
		t.Fatal("tenant A's write landed on tenant B's registration")
	}

	// Composition: the refused caller is not a broken caller. Tenant B reads
	// its OWN artifact through the artifacts surface on the same mux.
	ownerB := v123Quad(v123TenantB, v123User, v123Session, v123ProducerRun)
	refB := v123Put(t, st.store, ownerB, []byte("tenant b's own bytes"))
	gStatus, gBody := v123Get(t, st, v123Scope(v123TenantB, v123User, v123Session), refB.ID, nil)
	if gStatus != http.StatusOK {
		t.Fatalf("the tenant refused a foreign connection write cannot read its own "+
			"artifact: status=%d body=%v", gStatus, gBody)
	}
}

// --- the wave concurrency stress --------------------------------------------

// TestE2E_WaveV123_ConcurrentAcrossTenantsHoldsEverySeam is the §17.3
// cross-package stress. N=20 workers across 2 tenants drive all three surfaces
// against ONE shared stack — one artifact store, one bus, one catalog, one
// executor, one registry, one mux, one listener.
//
// Each worker plants content whose LENGTH is derived from its own (tenant,
// index) pair, so a worker that resolved another worker's artifact shows up as
// a size mismatch on its own — without relying on the wrong id also failing to
// resolve. Every worker additionally attempts the connection write it owns and
// the one it does not; only the former may land.
func TestE2E_WaveV123_ConcurrentAcrossTenantsHoldsEverySeam(t *testing.T) {
	st := newV123Stack(t)
	const perTenant = 10
	tenants := []string{v123TenantA, v123TenantB}
	ownServer := map[string]string{v123TenantA: v123ServerA, v123TenantB: v123ServerB}
	otherServer := map[string]string{v123TenantA: v123ServerB, v123TenantB: v123ServerA}

	type worker struct {
		q      identity.Quadruple
		refID  string
		body   []byte
		digest string
	}
	workers := make([]worker, 0, perTenant*len(tenants))
	for ti, tenant := range tenants {
		for i := range perTenant {
			q := v123Quad(tenant,
				fmt.Sprintf("v123-user-%02d", i),
				fmt.Sprintf("v123-session-%02d", i),
				fmt.Sprintf("v123-run-%d-%02d", ti, i))
			// A length unique across BOTH tenants, so two workers at the
			// same index never carry equal-length bodies.
			body := []byte(fmt.Sprintf("%s/%02d/", tenant, i) +
				strings.Repeat("y", ti*(perTenant+2)+i+1))
			sum := sha256.Sum256(body)
			workers = append(workers, worker{
				q:      q,
				refID:  v123Put(t, st.store, q, body).ID,
				body:   body,
				digest: hex.EncodeToString(sum[:]),
			})
		}
	}

	// The baseline is taken once every fixture is seated and the server is
	// warm, so it measures the stress rather than the setup.
	if status, _ := v123Get(t, st,
		v123Scope(workers[0].q.TenantID, workers[0].q.UserID, workers[0].q.SessionID),
		workers[0].refID, nil); status != http.StatusOK {
		t.Fatalf("warm-up read: status = %d", status)
	}
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make([]error, len(workers))
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w := workers[i]
			scope := v123Scope(w.q.TenantID, w.q.UserID, w.q.SessionID)

			// The Protocol read: bounded by the operator ceiling, truthful
			// about the whole artifact.
			status, body := v123Get(t, st, scope, w.refID, nil)
			if status != http.StatusOK {
				errs[i] = fmt.Errorf("worker %d: artifacts.get status %d body %v", i, status, body)
				return
			}
			if got := artifactsGetNum(t, body, "total_size_bytes"); got != int64(len(w.body)) {
				errs[i] = fmt.Errorf("worker %d: total_size_bytes %d, want %d — CROSS-TALK",
					i, got, len(w.body))
				return
			}
			ref, ok := body["ref"].(map[string]any)
			if !ok || fmt.Sprint(ref["sha256"]) != w.digest {
				errs[i] = fmt.Errorf("worker %d: ref digest %v, want %s — CROSS-TALK",
					i, ref["sha256"], w.digest)
				return
			}

			// The in-process read: the whole artifact, resolved under this
			// worker's own triple.
			raw, _, derr := v123Dispatch(t, st, w.q, w.refID)
			if derr != nil {
				errs[i] = fmt.Errorf("worker %d: dispatch: %w", i, derr)
				return
			}
			out, okOut := raw.(artifactstats.StatsResult)
			if !okOut {
				errs[i] = fmt.Errorf("worker %d: observation %T", i, raw)
				return
			}
			if out.SizeBytes != len(w.body) || out.SHA256 != w.digest {
				errs[i] = fmt.Errorf("worker %d: tool read %d bytes / %s, want %d / %s — CROSS-TALK",
					i, out.SizeBytes, out.SHA256, len(w.body), w.digest)
				return
			}

			// The registry write, under this worker's OWN full triple:
			// the caller's own tenant's registration only.
			if s, b := v123SetTrustAs(t, st, w.q.Identity, ownServer[w.q.TenantID], true); s != http.StatusOK {
				errs[i] = fmt.Errorf("worker %d: own connection write status %d body %v", i, s, b)
				return
			}
			if s, _ := v123SetTrustAs(t, st, w.q.Identity, otherServer[w.q.TenantID], false); s != http.StatusNotFound {
				errs[i] = fmt.Errorf("worker %d: a write against the other tenant's registration answered %d, want 404", i, s)
				return
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Every write that landed was an owner's, so both flags are true and
	// neither tenant's `false` reached the other's registration.
	for _, name := range []string{v123ServerA, v123ServerB} {
		if !v123Trust(t, st, name) {
			t.Fatalf("terminal flag on %s is false — a cross-tenant write landed", name)
		}
	}
	v123AssertAbsent(t, "a log record", st.logs.String())

	// Goroutine baseline. The stress opened one connection per worker; the
	// idle ones are closed explicitly so the settle loop is waiting on the
	// runtime rather than on the keep-alive pool.
	http.DefaultClient.CloseIdleConnections()
	st.srv.CloseClientConnections()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak after %d workers: baseline=%d, after=%d",
			len(workers), baseline, runtime.NumGoroutine())
	}
}

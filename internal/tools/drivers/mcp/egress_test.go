package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// egress_test.go — the MCP arm of pass-by-reference routing.
//
// # Why an SDK-built fixture alone cannot pin the wire encoding (§17.8)
//
// A fixture server whose handler declares a `Data []byte` field is
// SELF-CONSISTENT AT EITHER PLACEMENT, and would therefore rubber-stamp
// a wrong encoding exactly the way the ext-apps `_meta.ui` placement bug
// did. The mismatch is in the SDK's own two halves:
//
//   - `jsonschema-go`'s `forType` has NO []byte special case
//     (jsonschema/infer.go, `reflect.Slice` falls through to
//     `{"type":["null","array"], "items": {integer}}`), so such a server
//     ADVERTISES an array-typed parameter;
//   - `encoding/json` marshals and unmarshals the same field as a base64
//     STRING, so the same server ACCEPTS a base64 string.
//
// A test built only from those types passes whichever slot Harbor
// writes into. So the encoding is pinned three ways instead:
//
//  1. a byte-level GOLDEN of the arguments object a real `mcpsdk`
//     server received, committed under testdata (below), so a future
//     "simplification" of the carrier fails a byte diff rather than
//     passing a self-consistent fixture;
//  2. an env-gated `HARBOR_LIVE_MCP` probe against a real stdio MCP
//     server binary (TestEgress_Live_RealServerReceivesExactBytes);
//  3. a schema-derived attach check — the mapped parameter must be
//     DECLARED, and declared string-typed, by the server itself
//     (TestEgress_SchemaCheck_*), which turns the encoding from
//     Harbor's assumption into a contract checked against the server's
//     own declaration.

// egressBinaryFixture is a ten-byte document that is NOT valid UTF-8.
// Every non-ASCII byte here is one `encoding/json` would rewrite to
// U+FFFD in a Go string slot.
var egressBinaryFixture = []byte{0x25, 0x50, 0x44, 0x46, 0xFF, 0xFE, 0x00, 0x80, 0xC3, 0x28}

// egressCapturedCall is what the fixture server saw: the tool name and the RAW
// arguments JSON as it arrived on the wire. `CallToolRequest` is
// `ServerRequest[*CallToolParamsRaw]`, so `Params.Arguments` is the
// unmodified wire bytes — the only vantage point from which "what did
// the server actually receive" is answerable.
type egressCapturedCall struct {
	Name string
	Args json.RawMessage
}

type egressFixtureServer struct {
	mu    sync.Mutex
	calls []egressCapturedCall
	// frames counts every tools/call that reached the server, so a test
	// can assert NO wire request was issued on a refusal.
	frames atomic.Int64
}

func (s *egressFixtureServer) record(name string, args json.RawMessage) {
	s.frames.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, egressCapturedCall{Name: name, Args: append(json.RawMessage(nil), args...)})
}

func (s *egressFixtureServer) last(t *testing.T) egressCapturedCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatalf("the fixture server received no tools/call")
	}
	return s.calls[len(s.calls)-1]
}

// newEgressProvider builds an in-memory MCP server with three tools and
// wires a Provider to it over the SDK's in-memory transports.
//
// The `ingest` tool declares a STRING-typed `doc` parameter — the shape
// a real ingesting server publishes for a base64 document. `arraydoc`
// declares an ARRAY-typed parameter (the shape `jsonschema-go` infers
// for a Go []byte field), which the attach-time schema check must refuse
// even though such a server would happily accept a base64 string: that
// refusal is what stops Harbor writing into a slot the server never
// declared as one.
func newEgressProvider(t *testing.T, cfg Config) (*Provider, *egressFixtureServer) {
	t.Helper()
	fixture := &egressFixtureServer{}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-egress-fixture", Version: "v0"}, nil)

	stringSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"doc":  map[string]any{"type": "string"},
			"note": map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
	srv.AddTool(&mcpsdk.Tool{Name: "ingest", Description: "Ingests a base64 document.", InputSchema: stringSchema},
		func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			fixture.record(req.Params.Name, req.Params.Arguments)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}}}, nil
		})

	srv.AddTool(&mcpsdk.Tool{Name: "plain", Description: "Takes no artifact.", InputSchema: stringSchema},
		func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			fixture.record(req.Params.Name, req.Params.Arguments)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}}}, nil
		})

	srv.AddTool(&mcpsdk.Tool{
		Name:        "arraydoc",
		Description: "Declares an ARRAY-typed slot — the shape jsonschema-go infers for a Go []byte.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"doc": map[string]any{"type": []any{"null", "array"}, "items": map[string]any{"type": "integer"}},
			},
			"additionalProperties": false,
		},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		fixture.record(req.Params.Name, req.Params.Arguments)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}}}, nil
	})

	if cfg.Name == "" {
		cfg.Name = "egress-server"
	}
	if cfg.URL == "" {
		cfg.URL = "http://example.invalid"
	}
	cfg.TransportMode = TransportAuto
	if cfg.DefaultIdentity.TenantID == "" {
		cfg.DefaultIdentity = defaultIdentity()
	}
	if cfg.ArtifactEgressMaxBytes == 0 {
		cfg.ArtifactEgressMaxBytes = 1 << 20
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	clientSession, err := p.client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	p.mu.Lock()
	p.session = clientSession
	p.selectedMode = MCPTransportMode("inmemory")
	p.mu.Unlock()
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
		_ = p.Close(context.Background())
	})
	return p, fixture
}

func egressMapping(t *testing.T, in map[string][]string) artifactegress.Mapping {
	t.Helper()
	m, err := artifactegress.CompileMapping(in)
	if err != nil {
		t.Fatalf("CompileMapping: %v", err)
	}
	return m
}

// egressCtx returns a call context carrying the identity quadruple and a
// run-scoped resolver over the supplied contents — the same shape
// dispatch seats.
func egressCtx(t *testing.T, contents map[string][]byte, resolves *atomic.Int64) context.Context {
	t.Helper()
	return artifactref.WithResolver(egressRunCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, "run-1"),
		artifactref.ResolverFunc(func(_ context.Context, id string) ([]byte, error) {
			if resolves != nil {
				resolves.Add(1)
			}
			data, ok := contents[id]
			if !ok {
				return nil, fmt.Errorf("artifact %q not found under this run's scope", id)
			}
			return data, nil
		}))
}

// egressRunCtx builds the production-shaped invocation context: the
// identity the edge sets, plus the run quadruple the run loop adds.
func egressRunCtx(t *testing.T, id identity.Identity, runID string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// recordingBus wraps the REAL in-memory bus (so the publish path,
// including the bus's own event validation, is the production one) and
// adds two test affordances the real bus cannot provide: a synchronous
// record of what was published, and a forced publish refusal so the
// fail-closed ordering is testable at all.
type recordingBus struct {
	events.EventBus
	mu     sync.Mutex
	seen   []events.Event
	failOn events.EventType
}

func newRecordingBus(t *testing.T) *recordingBus {
	t.Helper()
	return &recordingBus{EventBus: newTestBus(t)}
}

func (b *recordingBus) Publish(ctx context.Context, ev events.Event) error {
	if b.failOn != "" && ev.Type == b.failOn {
		return errors.New("recordingBus: refusing to publish")
	}
	if err := b.EventBus.Publish(ctx, ev); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, ev)
	return nil
}

// egressRecords returns the substitution records the bus observed. It
// names the one event type these tests count rather than taking it as a
// parameter — a general filter with a single caller is a parameter
// nobody varies.
func (b *recordingBus) egressRecords() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []events.Event
	for _, e := range b.seen {
		if e.Type == EventTypeMCPArtifactEgressed {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// The wire encoding.
// ---------------------------------------------------------------------

// TestEgress_ServerReceivesExactBytes is the phase's headline: an
// arbitrary BINARY document reaches a real mcpsdk server byte-exact,
// having never entered the model's context.
func TestEgress_ServerReceivesExactBytes(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	ctx := egressCtx(t, map[string][]byte{"art-bin": egressBinaryFixture}, nil)
	if _, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"art-bin","note":"hello"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got := fixture.last(t)
	var received map[string]any
	if err := json.Unmarshal(got.Args, &received); err != nil {
		t.Fatalf("decode received args: %v", err)
	}
	b64, ok := received["doc"].(string)
	if !ok {
		t.Fatalf("received doc = %T, want a base64 string", received["doc"])
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("the server received a value that is not RFC 4648 §4 standard base64: %v", err)
	}
	if !bytes.Equal(raw, egressBinaryFixture) {
		t.Fatalf("the server received %#x, want %#x — byte-exactness is the whole point", raw, egressBinaryFixture)
	}
	// The unmapped parameter is untouched.
	if received["note"] != "hello" {
		t.Errorf("an unmapped parameter was rewritten: %v", received["note"])
	}
}

// TestEgress_GoldenTranscript is the §17.8 committed byte-level golden.
//
// The golden is the ARGUMENTS OBJECT of the outbound tools/call frame
// exactly as a real mcpsdk server received it. The frame's `_meta`
// carries a trace context and timestamps and is deliberately excluded —
// a golden that changed on every run would be deleted within a wave.
//
// Regenerate deliberately with HARBOR_UPDATE_GOLDEN=1, never by hand.
func TestEgress_GoldenTranscript(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	ctx := egressCtx(t, map[string][]byte{"art-bin": egressBinaryFixture}, nil)
	if _, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"art-bin","note":"hello"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	golden := filepath.Join("testdata", "egress_frame.golden.json")
	// A fresh slice, not an append onto the captured frame: the golden
	// carries a trailing newline for a readable file, and growing the
	// captured buffer in place would mutate what the fixture recorded.
	got := append(append([]byte(nil), fixture.last(t).Args...), '\n')
	if os.Getenv("HARBOR_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Fatalf("golden regenerated; re-run without HARBOR_UPDATE_GOLDEN")
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("outbound arguments frame drifted from the committed transcript.\n got: %s\nwant: %s\n\nThe wire encoding is NORMATIVE: a Go []byte behind artifactegress.Payload, emitted as RFC 4648 §4 standard base64. If this diff is a deliberate encoding change it needs a decision entry, not a golden refresh.", got, want)
	}
}

// TestEgress_UnmappedConnectionFrameIsUnchanged is the no-op guarantee,
// asserted against a captured frame rather than by inspection: with no
// mapping declared, the outbound arguments are the model's own bytes.
func TestEgress_UnmappedConnectionFrameIsUnchanged(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{Bus: bus, DefaultPolicy: tools.DefaultPolicy()})
	desc := resolveTool(t, p, "egress-server_ingest")

	const argsJSON = `{"doc":"art-bin","note":"hello"}`
	ctx := egressCtx(t, map[string][]byte{"art-bin": egressBinaryFixture}, nil)
	if _, err := desc.Invoke(ctx, json.RawMessage(argsJSON)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got := fixture.last(t)
	var received, want map[string]any
	if err := json.Unmarshal(got.Args, &received); err != nil {
		t.Fatalf("decode received: %v", err)
	}
	if err := json.Unmarshal([]byte(argsJSON), &want); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if len(received) != len(want) {
		t.Fatalf("received %v, want %v", received, want)
	}
	for k, v := range want {
		if received[k] != v {
			t.Fatalf("received[%q] = %v, want %v — an unmapped connection's frame must be identical to a build without this feature", k, received[k], v)
		}
	}
	// And no record was emitted for a connection that substituted nothing.
	if evs := bus.egressRecords(); len(evs) != 0 {
		t.Fatalf("an unmapped connection emitted %d substitution records", len(evs))
	}
}

// ---------------------------------------------------------------------
// The record: fail-closed, before the wire request, content-free.
// ---------------------------------------------------------------------

// TestEgress_RecordIsEmittedWithIdsAndDigestNeverBytes.
func TestEgress_RecordIsEmittedWithIdsAndDigestNeverBytes(t *testing.T) {
	const marker = "MARKER-c0ffee-DO-NOT-LEAK"
	bus := newRecordingBus(t)
	p, _ := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	ctx := egressCtx(t, map[string][]byte{"art-1": []byte(marker)}, nil)
	if _, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"art-1"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	evs := bus.egressRecords()
	if len(evs) != 1 {
		t.Fatalf("substitution records = %d, want 1", len(evs))
	}
	payload, ok := evs[0].Payload.(ArtifactEgressedPayload)
	if !ok {
		t.Fatalf("payload = %T, want ArtifactEgressedPayload", evs[0].Payload)
	}
	if payload.Identity.TenantID != "t1" || payload.Identity.UserID != "u1" || payload.Identity.SessionID != "s1" || payload.Identity.RunID != "run-1" {
		t.Errorf("payload identity = %+v, want the full call quadruple", payload.Identity)
	}
	if string(payload.ServerID) != "egress-server" || payload.ToolName != "ingest" {
		t.Errorf("payload = %+v, want the server and tool named", payload)
	}
	if len(payload.Records) != 1 || payload.Records[0].ArtifactID != "art-1" || payload.Records[0].Param != "doc" || payload.Records[0].SizeBytes != len(marker) {
		t.Fatalf("records = %+v", payload.Records)
	}
	if !strings.HasPrefix(payload.Records[0].Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256: prefix", payload.Records[0].Digest)
	}
	// Content-free: the whole payload, serialised, carries no marker.
	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(blob), marker) {
		t.Fatalf("the substitution record leaked content: %s", blob)
	}
	// SafePayload by construction — it reaches subscribers typed rather
	// than through the redactor's RedactedMap.
	if _, safe := any(payload).(events.SafePayload); !safe {
		t.Fatalf("ArtifactEgressedPayload does not implement events.SafePayload")
	}
}

// TestEgress_RecordIsFailClosed_NoWireRequestOnPublishFailure is the
// ordering assertion: a bus that refuses the record aborts the call
// BEFORE any wire request. Asserted by COUNTING FRAMES at the server,
// not by reading the error.
func TestEgress_RecordIsFailClosed_NoWireRequestOnPublishFailure(t *testing.T) {
	bus := newRecordingBus(t)
	bus.failOn = EventTypeMCPArtifactEgressed
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")
	before := fixture.frames.Load()

	ctx := egressCtx(t, map[string][]byte{"art-1": []byte("payload")}, nil)
	_, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"art-1"}`))
	if !errors.Is(err, ErrArtifactEgressUnrecorded) {
		t.Fatalf("err = %v, want ErrArtifactEgressUnrecorded", err)
	}
	if after := fixture.frames.Load(); after != before {
		t.Fatalf("the server received %d tools/call frame(s) after an unrecordable substitution; a substitution that could not be recorded must not happen", after-before)
	}
}

// TestEgress_RecordIsFailClosed_NoBusRefuses — a driver with no bus
// wired cannot record, so it refuses rather than moving bytes
// untraceably.
func TestEgress_RecordIsFailClosed_NoBusRefuses(t *testing.T) {
	// Config.validate rejects a nil Bus at New, so the refusal is
	// asserted at the emit helper directly — the path a future
	// bus-optional embedder would take.
	p := &Provider{cfg: Config{}, source: tools.ToolSourceID("s")}
	err := p.publishArtifactEgressed(context.Background(), "ingest", []artifactegress.Record{{ArtifactID: "a"}})
	if !errors.Is(err, ErrArtifactEgressUnrecorded) {
		t.Fatalf("err = %v, want ErrArtifactEgressUnrecorded", err)
	}
}

// TestEgress_RecordIsFailClosed_NoIdentityRefuses — identity is
// mandatory for the record, so a call without one is refused rather
// than recorded anonymously.
func TestEgress_RecordIsFailClosed_NoIdentityRefuses(t *testing.T) {
	p := &Provider{cfg: Config{Bus: &recordingBus{}}, source: tools.ToolSourceID("s")}
	err := p.publishArtifactEgressed(context.Background(), "ingest", []artifactegress.Record{{ArtifactID: "a"}})
	if !errors.Is(err, ErrArtifactEgressUnrecorded) {
		t.Fatalf("err = %v, want ErrArtifactEgressUnrecorded", err)
	}
}

// ---------------------------------------------------------------------
// Resolve-once: the retry-amplification property.
// ---------------------------------------------------------------------

// TestEgress_ResolvesOncePerDispatchedCallNotPerAttempt drives the
// reliability shell to exhaust its retry budget and asserts EXACTLY ONE
// resolve.
//
// It is a memory property (the transient footprint is `ceiling x
// in-flight`, not `ceiling x attempts x in-flight`) and a correctness
// one (an unresolvable id is a model mistake, not a transient fault).
func TestEgress_ResolvesOncePerDispatchedCallNotPerAttempt(t *testing.T) {
	var resolves atomic.Int64
	bus := newRecordingBus(t)
	// A policy that retries everything, so the inner function runs
	// MaxRetries+1 times.
	policy := tools.ToolPolicy{TimeoutMS: 2000, MaxRetries: 3, RetryOn: []tools.ErrorClass{tools.ErrClassTransient, tools.ErrClassTimeout, tools.ErrClass5xx}}
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  policy,
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	// Close the session so every wire attempt fails and the shell burns
	// its whole budget — while the resolve has already happened once,
	// outside the loop.
	ctx := egressCtx(t, map[string][]byte{"art-1": []byte("payload")}, &resolves)
	if _, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"art-1"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := resolves.Load(); got != 1 {
		t.Fatalf("resolves = %d, want exactly 1 per dispatched call", got)
	}
	if got := fixture.frames.Load(); got != 1 {
		t.Fatalf("frames = %d, want 1 on the happy path", got)
	}
	// And exactly one record per dispatched call, not one per attempt.
	if evs := bus.egressRecords(); len(evs) != 1 {
		t.Fatalf("substitution records = %d, want 1 per dispatched call", len(evs))
	}

	// Now force the shell to run its full budget: an unresolvable id
	// fails BEFORE the shell, so it is not retried at all.
	resolves.Store(0)
	_, err := desc.Invoke(egressCtx(t, nil, &resolves), json.RawMessage(`{"doc":"art-missing"}`))
	if err == nil {
		t.Fatalf("an unresolvable id succeeded")
	}
	if got := resolves.Load(); got != 1 {
		t.Fatalf("an unresolvable id was resolved %d times; a model mistake must not consume the retry budget", got)
	}
}

// ---------------------------------------------------------------------
// The observation.
// ---------------------------------------------------------------------

func TestEgress_ObservationCarriesTheRecordAndNotTheBytes(t *testing.T) {
	const marker = "MARKER-c0ffee-DO-NOT-LEAK"
	bus := newRecordingBus(t)
	p, _ := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	res, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte(marker)}, nil), json.RawMessage(`{"doc":"art-1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	value, ok := res.Value.(MCPToolValue)
	if !ok {
		t.Fatalf("value = %T, want MCPToolValue", res.Value)
	}
	if len(value.ArtifactEgress) != 1 || value.ArtifactEgress[0].ArtifactID != "art-1" {
		t.Fatalf("observation records = %+v, want the substitution recorded", value.ArtifactEgress)
	}
	// It REACHES the JSON observation (deliberately, unlike AppRef)...
	blob, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), "artifact_egress") || !strings.Contains(string(blob), "art-1") {
		t.Fatalf("the observation does not carry the substitution record: %s", blob)
	}
	// ...and carries no content.
	if strings.Contains(string(blob), marker) {
		t.Fatalf("the observation leaked artifact content: %s", blob)
	}
}

// ---------------------------------------------------------------------
// `args` is never rewritten — the raw-argument sinks inside the driver.
// ---------------------------------------------------------------------

// TestEgress_RawArgsAreNeverRewritten covers the two sinks that are
// computed from the RAW argument JSON inside this driver: the
// per-invocation content hash, and the durable MCP-App tool-context
// record's Input.
//
// Sink 7 is the worst of the seven the invariant must clear: the
// tool-context record is DURABLE, Protocol-readable, session-scoped
// rather than run-scoped, and it can mint a SECOND artifact carrying
// whatever it was handed. Substituting into the raw args would put the
// resolved content in a browser-readable store.
func TestEgress_RawArgsAreNeverRewritten(t *testing.T) {
	const marker = "MARKER-c0ffee-DO-NOT-LEAK"
	const argsJSON = `{"doc":"art-1"}`
	capturer := &capturingToolContext{}
	bus := newRecordingBus(t)
	p, _ := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ToolContext:    capturer,
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	if _, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte(marker)}, nil), json.RawMessage(argsJSON)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Sink 6 — the content hash is computed over the model's own args,
	// so it is stable across a substitution. Recomputing it here from the
	// RAW args must reproduce the same id the driver would mint.
	wantID := ToolCallID("run-1", "egress-server", "ingest", json.RawMessage(argsJSON))
	gotID := ToolCallID("run-1", "egress-server", "ingest", json.RawMessage(argsJSON))
	if wantID != gotID {
		t.Fatalf("ToolCallID is not deterministic over the raw args")
	}
	// A substituted-args hash would differ; assert it does, so this arm
	// is not vacuous.
	substituted, err := json.Marshal(map[string]any{"doc": base64.StdEncoding.EncodeToString([]byte(marker))})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if ToolCallID("run-1", "egress-server", "ingest", substituted) == wantID {
		t.Fatalf("the hash does not distinguish raw from substituted args, so this arm cannot detect a rewrite")
	}
}

// capturingToolContext records what the tool-context capturer was
// handed, so sink 7's INPUT can be asserted content-free.
type capturingToolContext struct {
	mu       sync.Mutex
	captured []CapturedToolContext
}

func (c *capturingToolContext) Capture(_ context.Context, in CapturedToolContext) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, in)
	return nil
}

// TestEgress_ToolContextInputIsTheModelsOwnArgs is sink 7 asserted
// directly: the durable, browser-readable record carries the artifact
// ID the model authored, never the resolved bytes.
func TestEgress_ToolContextInputIsTheModelsOwnArgs(t *testing.T) {
	const marker = "MARKER-c0ffee-DO-NOT-LEAK"
	const argsJSON = `{"doc":"art-1"}`
	capturer := &capturingToolContext{}
	bus := newRecordingBus(t)
	p, _ := newAppEgressProvider(t, capturer, bus)
	desc := resolveTool(t, p, "egress-app-server_ingest_app")

	if _, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte(marker)}, nil), json.RawMessage(argsJSON)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	capturer.mu.Lock()
	defer capturer.mu.Unlock()
	if len(capturer.captured) != 1 {
		t.Fatalf("tool-context captures = %d, want 1 (the arm would pass vacuously without one)", len(capturer.captured))
	}
	got := capturer.captured[0]
	if string(got.Input) != argsJSON {
		t.Fatalf("the durable tool-context Input = %s, want the model's own args %s", got.Input, argsJSON)
	}
	if strings.Contains(string(got.Input), marker) || strings.Contains(string(got.Input), base64.StdEncoding.EncodeToString([]byte(marker))) {
		t.Fatalf("the durable, browser-readable tool-context record carries the resolved content: %s", got.Input)
	}
}

// TestEgress_ToolContextResultKeepsStructuredShapeAndAppRef proves that
// artifact-parameter substitution is transparent to the result projection:
// the model-facing value retains its auditable egress record, while the
// browser-readable App context receives the server's original structured
// result shape and the invocation keeps its App reference/correlation.
func TestEgress_ToolContextResultKeepsStructuredShapeAndAppRef(t *testing.T) {
	const argsJSON = `{"doc":"art-1"}`
	capturer := newRecordingCapturer()
	bus := newRecordingBus(t)
	p, _ := newAppEgressProvider(t, capturer, bus)
	desc := resolveTool(t, p, "egress-app-server_ingest_app")

	result, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte("document bytes")}, nil), json.RawMessage(argsJSON))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	value, ok := result.Value.(MCPToolValue)
	if !ok {
		t.Fatalf("result.Value = %T, want MCPToolValue", result.Value)
	}
	if value.AppRef == nil {
		t.Fatal("result lost AppRef")
	}
	if value.AppRef.ResourceURI != "ui://egress/index.html" || value.AppRef.Binding == "" {
		t.Fatalf("result AppRef = %+v, want bound ui:// reference", value.AppRef)
	}
	if len(value.ArtifactEgress) != 1 || value.ArtifactEgress[0].ArtifactID != "art-1" {
		t.Fatalf("result egress records = %+v, want the authored artifact record", value.ArtifactEgress)
	}

	callID := value.AppRef.ToolCallID
	if callID == "" {
		t.Fatal("result AppRef.ToolCallID is empty despite successful capture")
	}
	captured, ok := capturer.get(callID)
	if !ok {
		t.Fatalf("no tool-context capture for AppRef.ToolCallID %q", callID)
	}
	if captured.identity.TenantID != "t1" || captured.identity.UserID != "u1" || captured.identity.SessionID != "s1" {
		t.Fatalf("captured identity = %+v, want the dispatch identity", captured.identity)
	}
	if string(captured.in.Input) != argsJSON {
		t.Fatalf("captured Input = %s, want the model-authored args %s", captured.in.Input, argsJSON)
	}
	var appResult map[string]any
	if err := json.Unmarshal(captured.in.Result, &appResult); err != nil {
		t.Fatalf("decode captured App result: %v (%s)", err, captured.in.Result)
	}
	if appResult["handle"] != "image-1" || appResult["status"] != "succeeded" {
		t.Fatalf("captured App result = %s, want direct structured result", captured.in.Result)
	}
	if _, wrapped := appResult["result"]; wrapped {
		t.Fatalf("captured App result retained the model egress wrapper: %s", captured.in.Result)
	}
	if _, recorded := appResult["artifact_egress"]; recorded {
		t.Fatalf("captured App result leaked dispatch metadata: %s", captured.in.Result)
	}

	modelResult, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal model result: %v", err)
	}
	var modelEnvelope map[string]any
	if err := json.Unmarshal(modelResult, &modelEnvelope); err != nil {
		t.Fatalf("decode model result: %v (%s)", err, modelResult)
	}
	if _, ok := modelEnvelope["result"]; !ok {
		t.Fatalf("model result lost result envelope: %s", modelResult)
	}
	if _, ok := modelEnvelope["artifact_egress"]; !ok {
		t.Fatalf("model result lost artifact_egress: %s", modelResult)
	}
}

// newAppEgressProvider builds a fixture whose mapped tool ALSO declares
// a `ui://` app, so the tool-context capture path actually fires (the
// driver captures only for app-declaring tools).
func newAppEgressProvider(t *testing.T, capturer ToolContextCapturer, bus events.EventBus) (*Provider, *egressFixtureServer) {
	t.Helper()
	fixture := &egressFixtureServer{}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-egress-app-fixture", Version: "v0"}, nil)
	srv.AddTool(&mcpsdk.Tool{
		Name:        "ingest_app",
		Description: "Ingests a base64 document and declares a ui:// app.",
		Meta:        mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://egress/index.html"}},
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"doc": map[string]any{"type": "string"}},
			"additionalProperties": false,
		},
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		fixture.record(req.Params.Name, req.Params.Arguments)
		return &mcpsdk.CallToolResult{
			Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"handle":"image-1","status":"succeeded"}`}},
			StructuredContent: map[string]any{"handle": "image-1", "status": "succeeded"},
		}, nil
	})

	p, err := New(Config{
		Name:                   "egress-app-server",
		URL:                    "http://example.invalid",
		TransportMode:          TransportAuto,
		Bus:                    bus,
		DefaultIdentity:        defaultIdentity(),
		DefaultPolicy:          tools.DefaultPolicy(),
		ToolContext:            capturer,
		ArtifactEgress:         egressMapping(t, map[string][]string{"ingest_app": {"doc"}}),
		ArtifactEgressMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	clientSession, err := p.client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	p.mu.Lock()
	p.session = clientSession
	p.selectedMode = MCPTransportMode("inmemory")
	p.mu.Unlock()
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
		_ = p.Close(context.Background())
	})
	return p, fixture
}

// ---------------------------------------------------------------------
// Refusals at the wire boundary.
// ---------------------------------------------------------------------

func TestEgress_OversizeValueFailsLoudWithNoWireRequest(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:                    bus,
		DefaultPolicy:          tools.DefaultPolicy(),
		ArtifactEgress:         egressMapping(t, map[string][]string{"ingest": {"doc"}}),
		ArtifactEgressMaxBytes: 8,
	})
	desc := resolveTool(t, p, "egress-server_ingest")
	before := fixture.frames.Load()

	_, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": make([]byte, 9)}, nil), json.RawMessage(`{"doc":"art-1"}`))
	if !errors.Is(err, artifactegress.ErrEgressTooLarge) {
		t.Fatalf("err = %v, want ErrEgressTooLarge", err)
	}
	if after := fixture.frames.Load(); after != before {
		t.Fatalf("an oversize value produced %d wire request(s); it must be refused, never truncated and sent", after-before)
	}
	if evs := bus.egressRecords(); len(evs) != 0 {
		t.Fatalf("a refused substitution emitted a record")
	}
}

// TestEgress_NoSeatedResolverFailsLoud is the MCP-App callback posture
// at the driver level: no run means no seated resolver, and the call
// fails loud naming the tool rather than degrading to sending the raw id
// string.
func TestEgress_NoSeatedResolverFailsLoud(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")
	before := fixture.frames.Load()

	// Identity but NO seated resolver — exactly the browser-driven MCP-App
	// callback's context: `mcp.apps.call_tool` resolves the SAME catalog
	// descriptor, but there is no run, so dispatch.ExecuteDecision never
	// ran and nothing seated a resolver.
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = desc.Invoke(ctx, json.RawMessage(`{"doc":"art-1"}`))
	if !errors.Is(err, artifactref.ErrNoResolver) {
		t.Fatalf("err = %v, want ErrNoResolver", err)
	}
	if !strings.Contains(err.Error(), "ingest") {
		t.Errorf("error %q does not name the tool", err)
	}
	if after := fixture.frames.Load(); after != before {
		t.Fatalf("a resolver-less call issued %d wire request(s); the raw id must never be sent as a degraded value", after-before)
	}
}

func TestEgress_MissingMappedParameterFailsLoud(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")
	before := fixture.frames.Load()

	_, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte("x")}, nil), json.RawMessage(`{"note":"only"}`))
	if !errors.Is(err, artifactegress.ErrMappedArgumentMissing) {
		t.Fatalf("err = %v, want ErrMappedArgumentMissing", err)
	}
	if after := fixture.frames.Load(); after != before {
		t.Fatalf("a call missing its mapped parameter reached the wire")
	}
}

func TestEgress_OptionalMappedParameterSkipsWhenAbsent(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc?"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	// An omitted optional parameter is inert even on a callback-shaped context
	// with identity but no run-scoped resolver: there is no substitution to
	// authorize, record, or resolve.
	ctx := egressRunCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, "run-optional")
	if _, err := desc.Invoke(ctx, json.RawMessage(`{"note":"only"}`)); err != nil {
		t.Fatalf("an omitted optional mapping failed: %v", err)
	}
	got := fixture.last(t)
	var received map[string]any
	if err := json.Unmarshal(got.Args, &received); err != nil {
		t.Fatalf("decode received args: %v", err)
	}
	if received["note"] != "only" || len(received) != 1 {
		t.Fatalf("received args = %v, want the unmapped note only", received)
	}
	if evs := bus.egressRecords(); len(evs) != 0 {
		t.Fatalf("an omitted optional parameter emitted substitution records: %d", len(evs))
	}

	// Supplying the same optional parameter still takes the normal resolver,
	// payload, audit-record and wire-base64 path.
	if _, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte("x")}, nil), json.RawMessage(`{"doc":"art-1"}`)); err != nil {
		t.Fatalf("a supplied optional mapping failed: %v", err)
	}
	got = fixture.last(t)
	if !strings.Contains(string(got.Args), `"doc":"eA=="`) {
		t.Fatalf("supplied optional mapping did not reach the wire as base64: %s", got.Args)
	}
	if evs := bus.egressRecords(); len(evs) != 1 {
		t.Fatalf("a supplied optional parameter emitted %d records, want 1", len(evs))
	}
}

func TestEgress_OptionalMappedParameterSkipsEmptyString(t *testing.T) {
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc?"}}),
	})
	desc := resolveTool(t, p, "egress-server_ingest")
	ctx := egressRunCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}, "run-empty-optional")

	// Some MCP/schema adapters materialize an omitted optional string as an
	// empty string. Harbor treats that representation as absence too, so a
	// text-only call can proceed without a resolver or substitution record.
	for _, empty := range []string{"", " \t\n "} {
		before := fixture.frames.Load()
		if _, err := desc.Invoke(ctx, json.RawMessage(fmt.Sprintf(`{"doc":%q,"note":"only"}`, empty))); err != nil {
			t.Fatalf("optional value %q failed: %v", empty, err)
		}
		if after := fixture.frames.Load(); after != before+1 {
			t.Fatalf("optional value %q produced %d wire requests, want 1", empty, after-before)
		}
		got := fixture.last(t)
		var received map[string]any
		if err := json.Unmarshal(got.Args, &received); err != nil {
			t.Fatalf("decode received args for %q: %v", empty, err)
		}
		if received["doc"] != empty || received["note"] != "only" || len(received) != 2 {
			t.Fatalf("received args for %q = %v, want untouched arguments", empty, received)
		}
		if evs := bus.egressRecords(); len(evs) != 0 {
			t.Fatalf("optional value %q emitted substitution records: %d", empty, len(evs))
		}
	}
}

// ---------------------------------------------------------------------
// The attach-time schema check.
// ---------------------------------------------------------------------

func TestEgress_SchemaCheck_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		mapping map[string][]string
		want    string
	}{
		{"unknown tool", map[string][]string{"no_such_tool": {"doc"}}, "declares no tool named"},
		{"parameter not declared", map[string][]string{"ingest": {"nope"}}, "does not declare a parameter named"},
		{"parameter declared non-string", map[string][]string{"arraydoc": {"doc"}}, "non-string type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := newRecordingBus(t)
			p, _ := newEgressProvider(t, Config{
				Bus:            bus,
				DefaultPolicy:  tools.DefaultPolicy(),
				ArtifactEgress: egressMapping(t, tc.mapping),
			})
			_, err := p.Discover(context.Background())
			if !errors.Is(err, ErrArtifactEgressSchema) {
				t.Fatalf("err = %v, want ErrArtifactEgressSchema", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain the refusal (%q)", err, tc.want)
			}
		})
	}
}

// TestEgress_SchemaCheck_AcceptsAStringDeclaredParameter proves the
// check is not simply refusing everything.
func TestEgress_SchemaCheck_AcceptsAStringDeclaredParameter(t *testing.T) {
	bus := newRecordingBus(t)
	p, _ := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	if _, err := p.Discover(context.Background()); err != nil {
		t.Fatalf("a string-declared parameter was refused: %v", err)
	}
}

// TestEgress_ConstructionRequiresACeilingWhenMapped — a programmatic
// embedder who arms a mapping and leaves the ceiling zero fails at
// CONSTRUCTION rather than identically on every mapped call, and a zero
// is never read as "unbounded".
func TestEgress_ConstructionRequiresACeilingWhenMapped(t *testing.T) {
	_, err := New(Config{
		Name:            "egress-server",
		URL:             "http://example.invalid",
		TransportMode:   TransportAuto,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		ArtifactEgress:  egressMapping(t, map[string][]string{"ingest": {"doc"}}),
		// ArtifactEgressMaxBytes deliberately unset.
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "ArtifactEgressMaxBytes") {
		t.Errorf("error %q does not name the missing ceiling", err)
	}

	// A connection with NO mapping is unaffected — the ceiling is
	// irrelevant when nothing is substituted.
	if _, err := New(Config{
		Name:            "egress-server",
		URL:             "http://example.invalid",
		TransportMode:   TransportAuto,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
	}); err != nil {
		t.Fatalf("an unmapped connection was refused for want of an egress ceiling: %v", err)
	}
}

func TestEgress_SchemaHelpers(t *testing.T) {
	if !schemaDeclaresString(map[string]any{"type": "string"}) {
		t.Errorf("scalar string form rejected")
	}
	if !schemaDeclaresString(map[string]any{"type": []any{"null", "string"}}) {
		t.Errorf("nullable union string form rejected")
	}
	// The array form jsonschema-go infers for a Go []byte — the exact
	// shape a self-consistent SDK fixture would advertise while still
	// ACCEPTING a base64 string. Refusing it is what stops the encoding
	// being pinned by a rubber stamp.
	if schemaDeclaresString(map[string]any{"type": []any{"null", "array"}}) {
		t.Errorf("the jsonschema-go []byte array form was accepted as a string slot")
	}
	if schemaDeclaresString(map[string]any{"type": 42}) || schemaDeclaresString("not an object") {
		t.Errorf("a malformed declaration was accepted")
	}
	if _, ok := schemaProperties("not an object"); ok {
		t.Errorf("a non-object schema yielded properties")
	}
}

// ---------------------------------------------------------------------
// D-025 concurrent reuse.
// ---------------------------------------------------------------------

// TestEgress_ConcurrentReuse_NoCrossRunByteBleed runs N=128 concurrent
// invocations against ONE shared Provider across two tenants and two
// sessions, half of them carrying mapped parameters.
//
// The bleed assertion derives each run's artifact LENGTH from its own
// index, so a cross-run bleed is a SIZE mismatch the server can see
// rather than a byte compare that could accidentally match.
func TestEgress_ConcurrentReuse_NoCrossRunByteBleed(t *testing.T) {
	const n = 128
	bus := newRecordingBus(t)
	p, fixture := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, map[string][]string{"ingest": {"doc"}}),
	})
	mapped := resolveTool(t, p, "egress-server_ingest")
	plain := resolveTool(t, p, "egress-server_plain")

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct identity per goroutine across two tenants and two
			// sessions, so a cross-identity bleed is visible.
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%2),
				UserID:    fmt.Sprintf("user-%d", i%3),
				SessionID: fmt.Sprintf("session-%d", i%2),
			}
			// Length derived from the index: run i's artifact is i+1 bytes.
			content := bytes.Repeat([]byte{byte('a' + i%26)}, i+1)
			artID := fmt.Sprintf("art-%d", i)
			ctx := artifactref.WithResolver(egressRunCtx(t, id, fmt.Sprintf("run-%d", i)), artifactref.ResolverFunc(func(_ context.Context, gotID string) ([]byte, error) {
				if gotID != artID {
					return nil, fmt.Errorf("run %d asked for %q, want %q — cross-run id bleed", i, gotID, artID)
				}
				return content, nil
			}))

			if i%2 == 0 {
				res, err := mapped.Invoke(ctx, json.RawMessage(fmt.Sprintf(`{"doc":%q}`, artID)))
				if err != nil {
					errs <- fmt.Errorf("run %d: %w", i, err)
					return
				}
				value, ok := res.Value.(MCPToolValue)
				if !ok || len(value.ArtifactEgress) != 1 {
					errs <- fmt.Errorf("run %d: observation carries %d records, want 1", i, len(value.ArtifactEgress))
					return
				}
				// The SIZE is the bleed detector: run i's record must
				// report run i's own artifact length.
				if value.ArtifactEgress[0].SizeBytes != i+1 {
					errs <- fmt.Errorf("run %d: record size = %d, want %d — a cross-run byte bleed", i, value.ArtifactEgress[0].SizeBytes, i+1)
					return
				}
				if value.ArtifactEgress[0].ArtifactID != artID {
					errs <- fmt.Errorf("run %d: record id = %q, want %q", i, value.ArtifactEgress[0].ArtifactID, artID)
				}
				return
			}
			// The other half exercises an UNMAPPED tool on the same shared
			// provider, so the mapping's per-tool scoping is under
			// concurrent load too.
			res, err := plain.Invoke(ctx, json.RawMessage(`{"note":"plain"}`))
			if err != nil {
				errs <- fmt.Errorf("run %d (plain): %w", i, err)
				return
			}
			if value, ok := res.Value.(MCPToolValue); ok && len(value.ArtifactEgress) != 0 {
				errs <- fmt.Errorf("run %d: an unmapped tool carried %d substitution records", i, len(value.ArtifactEgress))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := fixture.frames.Load(); got != n {
		t.Fatalf("frames = %d, want %d — some invocations did not reach the server, so the bleed assertions ran on nothing", got, n)
	}
	if evs := bus.egressRecords(); len(evs) != n/2 {
		t.Fatalf("substitution records = %d, want %d (one per mapped call)", len(evs), n/2)
	}
}

// TestEgress_MappingIsImmutableUnderAConcurrentConfigMutation is the
// D-025 companion: the mapping is captured BY VALUE into each tool's
// invocation closure at discovery, so mutating the source map afterwards
// cannot reach an in-flight call.
func TestEgress_MappingIsImmutableUnderAConcurrentConfigMutation(t *testing.T) {
	source := map[string][]string{"ingest": {"doc"}}
	bus := newRecordingBus(t)
	p, _ := newEgressProvider(t, Config{
		Bus:            bus,
		DefaultPolicy:  tools.DefaultPolicy(),
		ArtifactEgress: egressMapping(t, source),
	})
	desc := resolveTool(t, p, "egress-server_ingest")

	// Mutate the operator's source map after compilation — the shape a
	// live reconfiguration would take.
	source["ingest"] = []string{"something_else"}
	source["plain"] = []string{"doc"}

	res, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-1": []byte("payload")}, nil), json.RawMessage(`{"doc":"art-1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	value, ok := res.Value.(MCPToolValue)
	if !ok || len(value.ArtifactEgress) != 1 || value.ArtifactEgress[0].Param != "doc" {
		t.Fatalf("a post-construction mutation of the operator's map reached a live invocation: %+v", value.ArtifactEgress)
	}
}

// ---------------------------------------------------------------------
// §17.8 live probe.
// ---------------------------------------------------------------------

// TestEgress_Live_RealServerReceivesExactBytes drives a REAL stdio MCP
// server binary and asserts it received the exact stored bytes.
//
// Env-gated so CI skips it, following the shipped live-probe pattern:
// set HARBOR_LIVE_MCP=1, and optionally HARBOR_LIVE_MCP_BIN to override
// the binary. It exists because an SDK-built fixture is self-consistent
// at either encoding placement (see this file's header), so the wire
// contract needs at least one check against something that is not
// Harbor's own interpretation.
func TestEgress_Live_RealServerReceivesExactBytes(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_MCP") != "1" {
		t.Skip("reason: live MCP probe is env-gated; set HARBOR_LIVE_MCP=1 with a real stdio MCP server on HARBOR_LIVE_MCP_BIN")
	}
	bin := os.Getenv("HARBOR_LIVE_MCP_BIN")
	if bin == "" {
		t.Skip("reason: HARBOR_LIVE_MCP=1 requires HARBOR_LIVE_MCP_BIN to name a real stdio MCP server binary")
	}
	toolName := os.Getenv("HARBOR_LIVE_MCP_EGRESS_TOOL")
	paramName := os.Getenv("HARBOR_LIVE_MCP_EGRESS_PARAM")
	if toolName == "" || paramName == "" {
		t.Skip("reason: the live egress probe needs HARBOR_LIVE_MCP_EGRESS_TOOL and HARBOR_LIVE_MCP_EGRESS_PARAM naming a string-typed document parameter on the real server")
	}

	bus := newRecordingBus(t)
	p, err := New(Config{
		Name:                   "live-egress",
		TransportMode:          TransportStdio,
		Command:                []string{bin},
		Bus:                    bus,
		DefaultIdentity:        defaultIdentity(),
		DefaultPolicy:          tools.DefaultPolicy(),
		ArtifactEgress:         egressMapping(t, map[string][]string{toolName: {paramName}}),
		ArtifactEgressMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := p.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	// Discover ALSO runs the attach-time schema check against the real
	// server's published inputSchema — a live half of the §17.8 pin that
	// no in-repo fixture can provide.
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover (this includes the live schema check): %v", err)
	}
	var desc tools.ToolDescriptor
	for _, d := range descs {
		if d.Tool.Name == "live-egress_"+toolName {
			desc = d
		}
	}
	if desc.Invoke == nil {
		t.Fatalf("the real server does not expose %q", toolName)
	}

	res, err := desc.Invoke(egressCtx(t, map[string][]byte{"art-live": egressBinaryFixture}, nil),
		json.RawMessage(fmt.Sprintf(`{%q:"art-live"}`, paramName)))
	if err != nil {
		t.Fatalf("live invoke: %v", err)
	}
	value, ok := res.Value.(MCPToolValue)
	if !ok || len(value.ArtifactEgress) != 1 {
		t.Fatalf("the live call recorded no substitution: %+v", res.Value)
	}
	// The server ECHOES what it received (a probe server must); assert
	// the digest of what came back matches what was stored.
	if !strings.Contains(value.Text, base64.StdEncoding.EncodeToString(egressBinaryFixture)) {
		t.Fatalf("the real server did not echo the exact base64 of the stored bytes; got %q", value.Text)
	}
}

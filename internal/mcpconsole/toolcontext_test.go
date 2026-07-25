package mcpconsole_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// newToolCtxStore builds a ToolContextStore over fresh inmem state +
// artifact + bus drivers with the given heavy threshold.
func newToolCtxStore(t *testing.T, threshold int) *mcpconsole.ToolContextStore {
	t.Helper()
	tc, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State:     newAppsState(t),
		Store:     newAppsStore(t),
		Bus:       newAppsBus(t),
		Threshold: threshold,
	})
	if err != nil {
		t.Fatalf("NewToolContextStore: %v", err)
	}
	return tc
}

func idCtxFor(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestToolContextStore_NilDepsRejected(t *testing.T) {
	if _, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{Store: newAppsStore(t), Bus: newAppsBus(t)}); err == nil {
		t.Error("nil State accepted")
	}
	if _, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{State: newAppsState(t), Bus: newAppsBus(t)}); err == nil {
		t.Error("nil Store accepted")
	}
	if _, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{State: newAppsState(t), Store: newAppsStore(t)}); err == nil {
		t.Error("nil Bus accepted")
	}
}

// TestToolContextStore_CaptureLoadInline proves a small input + result round-
// trips inline (no offload) and the read projects the captured values.
func TestToolContextStore_CaptureLoadInline(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	ctx := idCtx(t)
	in := mcp.CapturedToolContext{
		ServerID:   "srv-a",
		ToolCallID: "call-1",
		Tool:       "weather",
		Input:      json.RawMessage(`{"city":"NYC"}`),
		Result:     json.RawMessage(`{"temp":21}`),
		IsError:    false,
	}
	if err := tc.Capture(ctx, in); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	row, err := tc.Load(ctx, "srv-a", "call-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if row.Tool != "weather" || row.IsError {
		t.Errorf("row meta wrong: %+v", row)
	}
	if string(row.Input.Inline) != `{"city":"NYC"}` || row.Input.Artifact != nil {
		t.Errorf("input projection wrong: %+v", row.Input)
	}
	if string(row.Result.Inline) != `{"temp":21}` || row.Result.Artifact != nil {
		t.Errorf("result projection wrong: %+v", row.Result)
	}
}

// TestToolContextStore_CaptureLoadHeavyOffload proves a result at or above
// the heavy threshold offloads to the ArtifactStore by reference (a loud
// `mcp.resource_offloaded` event), and the read projects the reference — the
// StateRecord never carries the bulky bytes.
func TestToolContextStore_CaptureLoadHeavyOffload(t *testing.T) {
	store := newAppsStore(t)
	bus := newAppsBus(t)
	tc, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State: newAppsState(t), Store: store, Bus: bus, Threshold: 1024,
	})
	if err != nil {
		t.Fatalf("NewToolContextStore: %v", err)
	}
	ctx := idCtx(t)
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: "t-1", User: "u-1", Session: "s-1",
		Types: []events.EventType{mcp.EventTypeMCPResourceOffloaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	heavy := json.RawMessage(`{"blob":"` + strings.Repeat("A", 2048) + `"}`)
	in := mcp.CapturedToolContext{
		ServerID: "srv-a", ToolCallID: "call-heavy", Tool: "render",
		Input: json.RawMessage(`{}`), Result: heavy,
	}
	if err := tc.Capture(ctx, in); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	row, err := tc.Load(ctx, "srv-a", "call-heavy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if row.Result.Artifact == nil || row.Result.Artifact.ID == "" {
		t.Fatalf("heavy result not offloaded: %+v", row.Result)
	}
	if len(row.Result.Inline) != 0 {
		t.Fatalf("heavy result also inlined (%d bytes)", len(row.Result.Inline))
	}
	if string(row.Input.Inline) != `{}` {
		t.Errorf("small input should ride inline: %+v", row.Input)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != mcp.EventTypeMCPResourceOffloaded {
			t.Fatalf("event type %q", ev.Type)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.resource_offloaded event — the loud bypass is not emitted")
	}
}

// TestToolContextStore_UnknownIDNotFound proves an unknown (serverID,
// toolCallID) returns a not-found-marked error that the Protocol surface maps
// to CodeNotFound.
func TestToolContextStore_UnknownIDNotFound(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	_, err := tc.Load(idCtx(t), "srv-a", "nope")
	if err == nil {
		t.Fatal("unknown id: want error, got nil")
	}
	// Assert the SENTINEL, not the message text. The Protocol edge classifies
	// not-found by errors.Is, so a text assertion tests something the edge no
	// longer reads — it passed with the sentinel wrap deleted, while
	// `mcp.apps.tool_context` silently regressed 404 → 500 and killed the
	// renderer's "no longer available" MISS path.
	if !errors.Is(err, protocol.ErrAccessorNotFound) {
		t.Errorf("unknown id: err does not wrap protocol.ErrAccessorNotFound (the edge will "+
			"classify this CodeRuntimeError, not CodeNotFound): %v", err)
	}
}

// TestToolContextStore_CrossIdentityNotFound is the isolation guard: a
// context captured under identity A is NOT loadable under identity B — the
// StateStore filters by the (tenant, user, session) triple, so a
// cross-identity read is not found by construction (existence is never
// revealed across identities).
func TestToolContextStore_CrossIdentityNotFound(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	ctxA := idCtxFor(t, "tenant-a", "user-a", "sess-a")
	ctxB := idCtxFor(t, "tenant-b", "user-b", "sess-b")
	in := mcp.CapturedToolContext{
		ServerID: "srv-a", ToolCallID: "shared-id", Tool: "weather",
		Input: json.RawMessage(`{}`), Result: json.RawMessage(`{"temp":1}`),
	}
	if err := tc.Capture(ctxA, in); err != nil {
		t.Fatalf("Capture A: %v", err)
	}
	// A reads its own.
	if _, err := tc.Load(ctxA, "srv-a", "shared-id"); err != nil {
		t.Fatalf("A should read its own context: %v", err)
	}
	// B with the SAME (serverID, toolCallID) must not find it.
	if _, err := tc.Load(ctxB, "srv-a", "shared-id"); err == nil {
		t.Fatal("cross-identity read found a context — isolation breach")
	} else if !errors.Is(err, protocol.ErrAccessorNotFound) {
		// Same reasoning as above, and load-bearing for isolation: a
		// cross-identity read must be INDISTINGUISHABLE at the wire from an
		// unknown id, which means both must reach the edge as the sentinel.
		t.Errorf("cross-identity read returned wrong error: %v", err)
	}
}

// TestToolContextStore_MissingIdentityFailsClosed proves Capture and Load
// fail closed when ctx carries no identity (CLAUDE.md §6).
func TestToolContextStore_MissingIdentityFailsClosed(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	in := mcp.CapturedToolContext{ServerID: "srv-a", ToolCallID: "x", Tool: "t", Input: json.RawMessage(`{}`), Result: json.RawMessage(`{}`)}
	if err := tc.Capture(context.Background(), in); err == nil {
		t.Error("Capture without identity accepted")
	}
	if _, err := tc.Load(context.Background(), "srv-a", "x"); err == nil {
		t.Error("Load without identity accepted")
	}
}

// TestAppsAccessor_ToolContext_ConcurrentReuse drives N>=128 concurrent
// capture+read round-trips through ONE shared ToolContextStore + AppsAccessor,
// each under a distinct identity + tool-call id, and asserts the D-025
// guarantees: no data races (the -race gate), no id collisions, no context
// bleed (each identity reads back only its own capture), and no goroutine
// leak. The store + accessor are immutable compiled artifacts; per-call
// identity rides ctx.
func TestAppsAccessor_ToolContext_ConcurrentReuse(t *testing.T) {
	tc := newToolCtxStore(t, 1024)
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    newAppsRegistry(t, nil),
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: tc,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}

	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := idCtxFor(t, "t-conc", "u-conc", fmt.Sprintf("s-%d", i))
			callID := fmt.Sprintf("call-%d", i)
			wantResult := fmt.Sprintf(`{"n":%d}`, i)
			if cerr := tc.Capture(ctx, mcp.CapturedToolContext{
				ServerID: "srv-a", ToolCallID: callID, Tool: "weather",
				Input: json.RawMessage(`{}`), Result: json.RawMessage(wantResult),
			}); cerr != nil {
				errs[i] = cerr
				return
			}
			// Read back through the AppsAccessor seam (the Protocol read path).
			row, rerr := acc.ToolContext(ctx, "srv-a", callID)
			if rerr != nil {
				errs[i] = rerr
				return
			}
			if string(row.Result.Inline) != wantResult {
				errs[i] = fmt.Errorf("goroutine %d: result bleed: got %s want %s", i, row.Result.Inline, wantResult)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d now=%d", baseline, runtime.NumGoroutine())
}

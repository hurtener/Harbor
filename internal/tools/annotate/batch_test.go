package annotate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/annotate"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// Count the actual storage seams while delegating behavior to real drivers.
type countingPolicyState struct {
	state.StateStore
	lists   atomic.Int64
	loads   atomic.Int64
	listErr error
}

func (s *countingPolicyState) ListKindForIdentity(ctx context.Context, id identity.Quadruple, prefix string) ([]state.StateRecord, error) {
	s.lists.Add(1)
	if prefix != "tools.approval.policy." {
		return nil, fmt.Errorf("unexpected policy prefix %q", prefix)
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.StateStore.ListKindForIdentity(ctx, id, prefix)
}

func (s *countingPolicyState) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.loads.Add(1)
	return s.StateStore.Load(ctx, id, kind)
}

type countingHistory struct {
	events.EventBus
	events.HistoryReplayer
	windows atomic.Int64
}

func (b *countingHistory) Window(ctx context.Context, before uint64, limit int, f events.Filter) ([]events.Event, error) {
	b.windows.Add(1)
	return b.HistoryReplayer.Window(ctx, before, limit, f)
}

type countingOAuth struct {
	mu    sync.Mutex
	calls map[tools.ToolSourceID]int
}

func (o *countingOAuth) Status(_ context.Context, id identity.Identity, source tools.ToolSourceID) prototypes.ToolOAuthStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls[source]++
	// The test checks that a batch never retains another session's status.
	if id.SessionID == "unbound" {
		return prototypes.ToolOAuthRequired
	}
	return oauthState(source)
}

func (*countingOAuth) Revoke(context.Context, identity.Identity, tools.ToolSourceID) (int64, error) {
	return 0, nil
}

func oauthState(source tools.ToolSourceID) prototypes.ToolOAuthStatus {
	switch source {
	case "bound":
		return prototypes.ToolOAuthBound
	case "expired":
		return prototypes.ToolOAuthExpired
	case "required":
		return prototypes.ToolOAuthRequired
	default:
		return prototypes.ToolOAuthNotApplicable
	}
}

type batchCatalogView struct{ tools.ToolCatalog }

func (v batchCatalogView) CatalogView(_ context.Context, id identity.Identity, _ string) (tools.PlannerCatalogView, error) {
	return tools.NewPlannerView(v.ToolCatalog, tools.CatalogFilter{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	}), nil
}

func (v batchCatalogView) ConfigurationCatalogView(ctx context.Context, id identity.Identity, agent string) (tools.PlannerCatalogView, error) {
	return v.CatalogView(ctx, id, agent)
}

type batchFixture struct {
	projector *toolsprotocol.CatalogProjector
	annotator *annotate.Annotator
	store     *countingPolicyState
	bus       *countingHistory
	oauth     *countingOAuth
	names     []string
}

func newBatchFixture(t *testing.T, st state.StateStore, count int) batchFixture {
	t.Helper()
	var tls []tools.Tool
	var names []string
	sources := []tools.ToolSourceID{"bound", "expired", "required", "no-oauth"}
	for i := range count {
		name := fmt.Sprintf("tool_%03d", i)
		names = append(names, name)
		tls = append(tls, tools.Tool{Name: name, Source: sources[i%len(sources)], Transport: tools.TransportMCP})
	}
	cat := newCatalog(t, tls...)
	store := &countingPolicyState{StateStore: st}
	ps, err := approval.NewStatePolicyStore(store)
	if err != nil {
		t.Fatal(err)
	}
	realBus := newBus(t)
	bus := &countingHistory{EventBus: realBus, HistoryReplayer: realBus.(events.HistoryReplayer)}
	oauth := &countingOAuth{calls: make(map[tools.ToolSourceID]int)}
	a := mkAnnotator(t, annotate.Deps{Catalog: cat, Approval: ps, Events: bus, OAuth: oauth})
	p, err := toolsprotocol.NewCatalogProjector(cat, toolsprotocol.WithAnnotator(a), toolsprotocol.WithCatalogViewResolver(batchCatalogView{cat}))
	if err != nil {
		t.Fatal(err)
	}
	return batchFixture{p, a, store, bus, oauth, names}
}

func batchState(t *testing.T, driver string) state.StateStore {
	t.Helper()
	var st state.StateStore
	var err error
	if driver == "postgres" {
		dsn := os.Getenv("HARBOR_TEST_PG_DSN")
		if dsn == "" {
			dsn = os.Getenv("HARBOR_PG_DSN")
		}
		if dsn == "" {
			t.Skip("HARBOR_TEST_PG_DSN / HARBOR_PG_DSN not set; skipping Postgres batch projection")
		}
		st, err = statepostgres.New(config.StateConfig{DSN: dsn})
	} else {
		st, err = stateinmem.New(config.StateConfig{})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func TestCatalogBatch_List100_BoundedReadsAndGoldenEquality(t *testing.T) {
	for _, driver := range []string{"inmem", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := newBatchFixture(t, batchState(t, driver), 100)
			ctx := context.Background()
			id := identity.Identity{TenantID: string(state.NewEventID()), UserID: "user", SessionID: "session"}
			policies := []prototypes.ToolApprovalPolicy{prototypes.ToolApprovalAuto, prototypes.ToolApprovalGated, prototypes.ToolApprovalDenied}
			for i, name := range f.names {
				// Leave a quarter of the tools with no pinned override.
				if i%4 != 3 {
					if err := f.annotator.SetApprovalPolicy(ctx, id, name, policies[i%3]); err != nil {
						t.Fatal(err)
					}
				}
				publish(t, f.bus, tools.EventTypeToolInvoked, id, tools.ToolInvokedPayload{ToolName: name}, baseTime.Add(-time.Hour))
				typ := tools.EventTypeToolCompleted
				var payload events.EventPayload = tools.ToolCompletedPayload{ToolName: name}
				if i%2 == 0 {
					typ = tools.EventTypeToolFailed
					payload = tools.ToolFailedPayload{ToolName: name}
				}
				publish(t, f.bus, typ, id, payload, baseTime.Add(-time.Minute))
				// Durable payload representation, newest timestamp published
				// before an older invocation to pin max-time rather than order.
				publish(t, f.bus, tools.EventTypeToolInvoked, id, struct {
					events.Sealed
					ToolName string `json:"ToolName"`
				}{ToolName: name}, baseTime)
				publish(t, f.bus, tools.EventTypeToolInvoked, id, tools.ToolInvokedPayload{ToolName: name}, baseTime.Add(-2*time.Hour))
			}
			other := id
			other.SessionID = "foreign"
			publish(t, f.bus, tools.EventTypeToolInvoked, other, tools.ToolInvokedPayload{ToolName: f.names[0]}, baseTime.Add(time.Hour))
			for _, configuration := range []bool{false, true} {
				f.store.lists.Store(0)
				f.store.loads.Store(0)
				f.bus.windows.Store(0)
				f.oauth.calls = make(map[tools.ToolSourceID]int)
				var rows []prototypes.Tool
				var err error
				if configuration {
					rows, err = f.projector.ListConfigurationTools(ctx, id, "agent")
				} else {
					rows, err = f.projector.ListTools(ctx, id)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 100 || f.store.lists.Load() != 1 || f.store.loads.Load() != 0 || f.bus.windows.Load() != 1 {
					t.Fatalf("configuration=%t: rows=%d lists=%d loads=%d windows=%d, want 100/1/0/1", configuration, len(rows), f.store.lists.Load(), f.store.loads.Load(), f.bus.windows.Load())
				}
				if len(f.oauth.calls) != 4 {
					t.Fatalf("OAuth sources = %v, want four", f.oauth.calls)
				}
				for source, calls := range f.oauth.calls {
					if calls != 1 {
						t.Fatalf("OAuth %s calls = %d, want one", source, calls)
					}
				}
				for i, row := range rows {
					perTool, err := f.projector.GetTool(ctx, id, row.ID)
					if err != nil {
						t.Fatal(err)
					}
					golden, _ := json.Marshal(perTool)
					got, _ := json.Marshal(row)
					if string(got) != string(golden) {
						t.Fatalf("batch differs from per-tool projection:\n%s\n%s", got, golden)
					}
					wantPolicy := prototypes.ToolApprovalAuto
					if i%4 != 3 {
						wantPolicy = policies[i%3]
					}
					if row.ApprovalPolicy != wantPolicy || row.OAuthStatus != oauthState(tools.ToolSourceID(row.Owner)) || !row.LastUsedAt.Equal(baseTime) {
						t.Fatalf("incorrect annotations: %+v", row)
					}
				}
			}
		})
	}
}

func TestCatalogBatch_Describe_OnlyRequestedTool(t *testing.T) {
	f := newBatchFixture(t, batchState(t, "inmem"), 100)
	for _, configuration := range []bool{false, true} {
		f.store.loads.Store(0)
		f.bus.windows.Store(0)
		f.oauth.calls = make(map[tools.ToolSourceID]int)
		var err error
		if configuration {
			_, err = f.projector.DescribeConfigurationTool(context.Background(), idA(), f.names[0], "agent")
		} else {
			_, err = f.projector.DescribeTool(context.Background(), idA(), f.names[0], "agent")
		}
		if err != nil {
			t.Fatal(err)
		}
		if f.store.lists.Load() != 0 || f.store.loads.Load() != 1 || f.bus.windows.Load() != 1 || !reflect.DeepEqual(f.oauth.calls, map[tools.ToolSourceID]int{"bound": 1}) {
			t.Fatalf("describe read unrelated annotations: lists=%d loads=%d windows=%d OAuth=%v", f.store.lists.Load(), f.store.loads.Load(), f.bus.windows.Load(), f.oauth.calls)
		}
	}
}

func TestCatalogBatch_EmptyAndFailedList_NoAnnotations(t *testing.T) {
	f := newBatchFixture(t, batchState(t, "inmem"), 0)
	if rows, err := f.projector.ListConfigurationTools(context.Background(), idA(), "agent"); err != nil || len(rows) != 0 {
		t.Fatalf("empty list = %v, %v", rows, err)
	}
	if f.store.lists.Load() != 0 || f.bus.windows.Load() != 0 || len(f.oauth.calls) != 0 {
		t.Fatal("empty list fetched annotations")
	}
	f = newBatchFixture(t, batchState(t, "inmem"), 1)
	f.store.listErr = state.ErrStoreClosed
	if rows, err := f.projector.ListConfigurationTools(context.Background(), idA(), "agent"); !errors.Is(err, state.ErrStoreClosed) || rows != nil {
		t.Fatalf("failed policy read = %v, %v; want fail closed", rows, err)
	}
	if f.bus.windows.Load() != 0 || len(f.oauth.calls) != 0 {
		t.Fatal("read annotations after approval failure")
	}
}

func TestCatalogBatch_ConcurrentListAndSetApprovalPolicy_Isolated(t *testing.T) {
	for _, driver := range []string{"inmem", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			f := newBatchFixture(t, batchState(t, driver), 4)
			svc, err := toolsprotocol.NewService(f.projector, toolsprotocol.WithBus(f.bus), toolsprotocol.WithRedactor(patterns.New()), toolsprotocol.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
			if err != nil {
				t.Fatal(err)
			}
			const n = 100
			tenant := string(state.NewEventID())
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range n {
				id := identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("user-%d", i), SessionID: fmt.Sprintf("session-%d", i)}
				if i%2 == 0 {
					id.SessionID = "unbound"
				}
				scope := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
				wantPolicy := prototypes.ToolApprovalGated
				if i%2 == 0 {
					wantPolicy = prototypes.ToolApprovalDenied
				}
				if err := f.annotator.SetApprovalPolicy(context.Background(), id, f.names[0], wantPolicy); err != nil {
					t.Fatal(err)
				}
				publish(t, f.bus, tools.EventTypeToolInvoked, id, tools.ToolInvokedPayload{ToolName: f.names[0]}, baseTime.Add(time.Duration(i)*time.Second))
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, err := svc.SetApprovalPolicy(context.Background(), prototypes.ToolSetApprovalPolicyRequest{Identity: scope, ID: f.names[1], Policy: wantPolicy}, true)
					if err != nil {
						t.Errorf("set_approval_policy: %v", err)
					}
				}()
				go func() {
					defer wg.Done()
					<-start
					rows, err := f.projector.ListConfigurationTools(context.Background(), id, "agent")
					if err != nil {
						t.Errorf("concurrent list: %v", err)
						return
					}
					if len(rows) != 4 {
						t.Errorf("concurrent list rows = %d", len(rows))
						return
					}
					wantOAuth := prototypes.ToolOAuthBound
					if id.SessionID == "unbound" {
						wantOAuth = prototypes.ToolOAuthRequired
					}
					if rows[0].ApprovalPolicy != wantPolicy || rows[0].OAuthStatus != wantOAuth || !rows[0].LastUsedAt.Equal(baseTime.Add(time.Duration(i)*time.Second)) {
						t.Errorf("cross-identity annotations for %s: %+v", id.UserID, rows[0])
					}
					if rows[1].ApprovalPolicy != prototypes.ToolApprovalAuto && rows[1].ApprovalPolicy != wantPolicy {
						t.Errorf("unexpected racing policy: %s", rows[1].ApprovalPolicy)
					}
				}()
			}
			close(start)
			wg.Wait()
		})
	}
}

func TestCatalogBatch_Requests_RevalidateAnnotations(t *testing.T) {
	f := newBatchFixture(t, batchState(t, "inmem"), 1)
	ctx := context.Background()
	id := idA()
	rows, err := f.projector.ListConfigurationTools(ctx, id, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ApprovalPolicy != prototypes.ToolApprovalAuto || !rows[0].LastUsedAt.IsZero() {
		t.Fatalf("initial annotations = %+v", rows[0])
	}
	if err := f.annotator.SetApprovalPolicy(ctx, id, f.names[0], prototypes.ToolApprovalDenied); err != nil {
		t.Fatal(err)
	}
	publish(t, f.bus, tools.EventTypeToolInvoked, id, tools.ToolInvokedPayload{ToolName: f.names[0]}, baseTime)
	rows, err = f.projector.ListConfigurationTools(ctx, id, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ApprovalPolicy != prototypes.ToolApprovalDenied || !rows[0].LastUsedAt.Equal(baseTime) || f.oauth.calls["bound"] != 2 {
		t.Fatalf("annotations were cached across requests: %+v; OAuth reads=%d", rows[0], f.oauth.calls["bound"])
	}
}

func TestAnnotator_Batch_ConcurrentRowsAndCancellation(t *testing.T) {
	f := newBatchFixture(t, batchState(t, "inmem"), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.annotator.Batch(ctx, idA()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch = %v", err)
	}
	if _, err := f.annotator.Batch(context.Background(), identity.Identity{TenantID: "t"}); err == nil {
		t.Fatal("incomplete identity accepted")
	}
	if f.store.lists.Load() != 0 || f.bus.windows.Load() != 0 {
		t.Fatal("invalid or cancelled batch read storage")
	}
	batch, err := f.annotator.Batch(context.Background(), idA())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			got := batch(context.Background(), f.names[0])
			if got.OAuthStatus != prototypes.ToolOAuthBound || got.ApprovalPolicy != prototypes.ToolApprovalAuto || !got.LastUsedAt.IsZero() {
				t.Errorf("concurrent row = %+v", got)
			}
		})
	}
	wg.Wait()
	if f.oauth.calls["bound"] != 1 {
		t.Fatalf("concurrent OAuth reads = %d, want one", f.oauth.calls["bound"])
	}
}

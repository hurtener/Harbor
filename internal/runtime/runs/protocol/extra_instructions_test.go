// The additive-guidance two-producer join (D-365).
//
// `ExtraInstructions` is the ONE field ComposeLLMOverrides JOINS instead of
// resolving last-writer-wins. The tenant-wide block is admin-set; the session
// block is not. Replacement would hand a non-admin caller a silent delete on
// an admin-set compliance block, so the run-level value composes BELOW the
// tenant value and there is no run-level clear.
//
// Sibling of overrides_test.go (same `protocol_test` package — the helpers
// newService / wireReq / strPtr / f64Ptr / newTestBus / testTenant... are
// shared).
package protocol_test

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
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

const (
	tenantAdditive  = "TENANT-COMPLIANCE-BLOCK: cite every source."
	sessionAdditive = "SESSION-ADDITIVE: answer in the imperative mood."
)

// TestComposeLLMOverrides_ExtraInstructionsJoinTable is the four-cell
// {tenant set, unset} × {session set, unset} table. The "both set" cell is the
// precedence decision: tenant FIRST, blank-line separated. Turning the join
// into `out.ExtraInstructions = session.ExtraInstructions` fails the "both
// set" cell; swapping the order fails it on ordering.
func TestComposeLLMOverrides_ExtraInstructionsJoinTable(t *testing.T) {
	t.Run("neither set: nil", func(t *testing.T) {
		got := runsprotocol.ComposeLLMOverrides(
			&runsprotocol.PendingOverride{Model: strPtr("m")}, nil, &planner.LLMOverrides{})
		if got.ExtraInstructions != nil {
			t.Fatalf("ExtraInstructions = %q, want nil", *got.ExtraInstructions)
		}
	})

	t.Run("tenant only: passes through BYTE-IDENTICALLY", func(t *testing.T) {
		tenant := &planner.LLMOverrides{ExtraInstructions: strPtr(tenantAdditive)}
		// With no session layer at all — today's shape.
		noSession := runsprotocol.ComposeLLMOverrides(nil, nil, tenant)
		// And with a session layer that sets OTHER fields but no guidance —
		// the shape a Playground temperature tweak produces.
		withSession := runsprotocol.ComposeLLMOverrides(
			&runsprotocol.PendingOverride{Temperature: f64Ptr(0.4)}, nil, tenant)
		for name, got := range map[string]*planner.LLMOverrides{
			"no session layer": noSession, "session sets other fields": withSession,
		} {
			if got.ExtraInstructions == nil || *got.ExtraInstructions != tenantAdditive {
				t.Fatalf("%s: ExtraInstructions = %v, want the tenant block verbatim", name, got.ExtraInstructions)
			}
			// Byte-identical is stricter than equal: the tenant's own string
			// must be passed through, not re-derived by a join that happens to
			// produce the same bytes today.
			if got.ExtraInstructions != tenant.ExtraInstructions {
				t.Errorf("%s: the tenant pointer was replaced; an absent session contribution must leave the tenant value untouched", name)
			}
		}
	})

	t.Run("session only: the session block verbatim", func(t *testing.T) {
		got := runsprotocol.ComposeLLMOverrides(
			&runsprotocol.PendingOverride{ExtraInstructions: strPtr(sessionAdditive)}, nil, nil)
		if got.ExtraInstructions == nil || *got.ExtraInstructions != sessionAdditive {
			t.Fatalf("ExtraInstructions = %v, want the session block verbatim", got.ExtraInstructions)
		}
	})

	t.Run("both set: tenant FIRST, blank-line joined", func(t *testing.T) {
		tenant := &planner.LLMOverrides{ExtraInstructions: strPtr(tenantAdditive)}
		got := runsprotocol.ComposeLLMOverrides(
			&runsprotocol.PendingOverride{ExtraInstructions: strPtr(sessionAdditive)}, nil, tenant)
		want := tenantAdditive + "\n\n" + sessionAdditive
		if got.ExtraInstructions == nil {
			t.Fatal("ExtraInstructions = nil; want the two blocks joined")
		}
		if *got.ExtraInstructions != want {
			t.Fatalf("ExtraInstructions =\n%q\nwant\n%q", *got.ExtraInstructions, want)
		}
		// The tenant's OWN string must be untouched — a join that appended in
		// place would corrupt the record every other run reads.
		if *tenant.ExtraInstructions != tenantAdditive {
			t.Errorf("the tenant record was mutated in place: %q", *tenant.ExtraInstructions)
		}
	})
}

// TestComposeLLMOverrides_ExtraInstructionsNoRunLevelClear is the no-clear
// property. A session-level empty string, a whitespace-only string, and a
// session-level value alongside a tenant value each leave the ADMIN-set tenant
// text present in the result. There is no run-level clear, because
// `runs.set_overrides` is not admin-gated and the tenant block is.
func TestComposeLLMOverrides_ExtraInstructionsNoRunLevelClear(t *testing.T) {
	clearAttempts := map[string]*string{
		"empty string":        strPtr(""),
		"whitespace only":     strPtr("   \n\t  "),
		"a real contribution": strPtr(sessionAdditive),
	}
	for name, attempt := range clearAttempts {
		t.Run(name, func(t *testing.T) {
			tenant := &planner.LLMOverrides{ExtraInstructions: strPtr(tenantAdditive)}
			got := runsprotocol.ComposeLLMOverrides(
				&runsprotocol.PendingOverride{ExtraInstructions: attempt}, nil, tenant)
			if got.ExtraInstructions == nil {
				t.Fatal("the admin-set tenant block was CLEARED by a session-level set")
			}
			if !strings.Contains(*got.ExtraInstructions, tenantAdditive) {
				t.Fatalf("the admin-set tenant block is gone: %q", *got.ExtraInstructions)
			}
		})
	}

	t.Run("a whitespace-only session value leaves no dangling separator", func(t *testing.T) {
		tenant := &planner.LLMOverrides{ExtraInstructions: strPtr(tenantAdditive)}
		got := runsprotocol.ComposeLLMOverrides(
			&runsprotocol.PendingOverride{ExtraInstructions: strPtr("  \n ")}, nil, tenant)
		if *got.ExtraInstructions != tenantAdditive {
			t.Fatalf("ExtraInstructions = %q, want the tenant block with no trailing separator", *got.ExtraInstructions)
		}
	})
}

// TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk runs N=128 goroutines
// against ONE shared Service + Store, each in its own session with its own
// distinguishable block, all reading ONE shared tenant record.
// ComposeLLMOverrides is a pure function over its arguments; the test's value
// is proving the join allocates a FRESH string per call and never appends into
// a caller's *planner.LLMOverrides in place — the D-025 failure that would
// corrupt a shared tenant record across runs (CLAUDE.md §5).
func TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	const n = 128
	baseline := runtime.NumGoroutine()

	svc, store := newService(t)
	// ONE shared tenant record, read by every goroutine — the object a
	// mutating join would corrupt.
	shared := &planner.LLMOverrides{ExtraInstructions: strPtr(tenantAdditive)}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("session-join-%04d", i)
			mine := fmt.Sprintf("SESSION-BLOCK-%04d", i)
			req := prototypes.RunSetOverridesRequest{
				Identity: prototypes.IdentityScope{
					Tenant: testTenant, User: testUser, Session: session,
				},
				Overrides: prototypes.RunOverrides{
					SessionID: session, ExtraInstructions: strPtr(mine),
				},
			}
			if _, err := svc.SetOverrides(context.Background(), req); err != nil {
				t.Errorf("%s: SetOverrides: %v", session, err)
				return
			}
			po, ok := store.Consume(identity.Identity{
				TenantID: testTenant, UserID: testUser, SessionID: session,
			})
			if !ok {
				t.Errorf("%s: nothing recorded", session)
				return
			}
			got := runsprotocol.ComposeLLMOverrides(&po, nil, shared)
			want := tenantAdditive + "\n\n" + mine
			if got.ExtraInstructions == nil || *got.ExtraInstructions != want {
				t.Errorf("%s: composed = %v, want exactly its own two segments", session, got.ExtraInstructions)
				return
			}
			// No OTHER goroutine's block may appear in this result.
			for _, other := range []int{(i + 1) % n, (i + 7) % n} {
				if other == i {
					continue
				}
				if strings.Contains(*got.ExtraInstructions, fmt.Sprintf("SESSION-BLOCK-%04d", other)) {
					t.Errorf("%s: cross-talk — another goroutine's block leaked in", session)
				}
			}
		}()
	}
	wg.Wait()

	// The shared tenant record is byte-unchanged after 128 joins.
	if *shared.ExtraInstructions != tenantAdditive {
		t.Errorf("the shared tenant record was mutated by the join: %q", *shared.ExtraInstructions)
	}
	// No goroutine leak: the join starts none, so the baseline holds.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: %d goroutines, baseline %d", got, baseline)
	}
}

// TestSetOverrides_ExtraInstructionsCopiedByValue proves `validate` copies the
// caller's string rather than aliasing its pointer, and that an empty /
// whitespace-only value is ACCEPTED (a no-op, not an error and not a clear).
func TestSetOverrides_ExtraInstructionsCopiedByValue(t *testing.T) {
	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}

	t.Run("copied by value", func(t *testing.T) {
		svc, store := newService(t)
		caller := sessionAdditive
		req := wireReq(prototypes.RunOverrides{SessionID: testSession, ExtraInstructions: &caller})
		if _, err := svc.SetOverrides(context.Background(), req); err != nil {
			t.Fatalf("SetOverrides: %v", err)
		}
		// The caller mutates its own request struct afterwards.
		caller = "MUTATED-AFTER-THE-CALL"
		po, ok := store.Peek(id)
		if !ok || po.ExtraInstructions == nil {
			t.Fatal("nothing recorded")
		}
		if *po.ExtraInstructions != sessionAdditive {
			t.Fatalf("stored value = %q, want the value at call time (the header was aliased)", *po.ExtraInstructions)
		}
	})

	for name, v := range map[string]string{"empty": "", "whitespace only": "  \n "} {
		t.Run("accepted as a no-op: "+name, func(t *testing.T) {
			svc, store := newService(t)
			if _, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
				SessionID: testSession, ExtraInstructions: strPtr(v),
			})); err != nil {
				t.Fatalf("SetOverrides: %v — an empty value must be accepted, not refused", err)
			}
			po, ok := store.Peek(id)
			if !ok || po.ExtraInstructions == nil || *po.ExtraInstructions != v {
				t.Fatalf("stored = %v, want the empty value recorded verbatim", po.ExtraInstructions)
			}
		})
	}
}

// TestSetOverrides_ExtraInstructionsAuditFlag proves the emitted audit payload
// carries the boolean flag exactly when the field was set, and that the VALUE
// never appears anywhere in the marshalled event.
func TestSetOverrides_ExtraInstructionsAuditFlag(t *testing.T) {
	const sentinel = "SESSION-GUIDANCE-SENTINEL-DO-NOT-EMIT"

	for name, set := range map[string]bool{"field set": true, "field unset": false} {
		t.Run(name, func(t *testing.T) {
			bus := newTestBus(t)
			defer func() { _ = bus.Close(context.Background()) }()
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Tenant: testTenant, User: testUser, Session: testSession,
				Types: []events.EventType{events.EventTypeRunOverridesSet},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			o := prototypes.RunOverrides{SessionID: testSession, Temperature: f64Ptr(0.5)}
			if set {
				o.ExtraInstructions = strPtr(sentinel)
			}
			svc, _ := newService(t, runsprotocol.WithBus(bus))
			if _, err := svc.SetOverrides(context.Background(), wireReq(o)); err != nil {
				t.Fatalf("SetOverrides: %v", err)
			}

			select {
			case ev := <-sub.Events():
				payload, ok := ev.Payload.(events.RunOverridesSetPayload)
				if !ok {
					t.Fatalf("payload type = %T", ev.Payload)
				}
				if payload.SetExtraInstructions != set {
					t.Fatalf("SetExtraInstructions = %v, want %v", payload.SetExtraInstructions, set)
				}
				// The VALUE must not ride the bus (CLAUDE.md §7). Marshal the
				// whole event and substring-scan it.
				blob, err := json.Marshal(ev)
				if err != nil {
					t.Fatalf("marshal event: %v", err)
				}
				if strings.Contains(string(blob), sentinel) {
					t.Fatalf("the additive-guidance TEXT reached the bus: %s", blob)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for runs.overrides_set event")
			}
		})
	}
}

// TestSetOverrides_IdentityAndCrossSessionRefusedWithExtraInstructions re-runs
// the two refusals with the NEW field set: a new field must not open a new
// door (CLAUDE.md §6).
func TestSetOverrides_IdentityAndCrossSessionRefusedWithExtraInstructions(t *testing.T) {
	t.Run("incomplete identity", func(t *testing.T) {
		svc, store := newService(t)
		for name, idscope := range map[string]prototypes.IdentityScope{
			"missing tenant":  {User: testUser, Session: testSession},
			"missing user":    {Tenant: testTenant, Session: testSession},
			"missing session": {Tenant: testTenant, User: testUser},
		} {
			_, err := svc.SetOverrides(context.Background(), prototypes.RunSetOverridesRequest{
				Identity: idscope,
				Overrides: prototypes.RunOverrides{
					SessionID: testSession, ExtraInstructions: strPtr(sessionAdditive),
				},
			})
			if !errors.Is(err, runsprotocol.ErrIdentityRequired) {
				t.Fatalf("%s: error = %v, want ErrIdentityRequired", name, err)
			}
		}
		if _, ok := store.Peek(identity.Identity{
			TenantID: testTenant, UserID: testUser, SessionID: testSession,
		}); ok {
			t.Error("a refused request must not record an override")
		}
	})

	t.Run("cross-session target", func(t *testing.T) {
		svc, store := newService(t)
		_, err := svc.SetOverrides(context.Background(), wireReq(prototypes.RunOverrides{
			SessionID: "some-other-session", ExtraInstructions: strPtr(sessionAdditive),
		}))
		if !errors.Is(err, runsprotocol.ErrCrossSessionScope) {
			t.Fatalf("error = %v, want ErrCrossSessionScope", err)
		}
		for name, id := range map[string]identity.Identity{
			"caller's own session": {TenantID: testTenant, UserID: testUser, SessionID: testSession},
			"the named session":    {TenantID: testTenant, UserID: testUser, SessionID: "some-other-session"},
		} {
			if _, ok := store.Peek(id); ok {
				t.Errorf("%s: a refused cross-session set recorded an override", name)
			}
		}
	})
}

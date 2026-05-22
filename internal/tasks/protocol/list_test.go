package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

// newListService builds a tasks/protocol.Service over a fresh registry.
func newListService(t *testing.T) (*tasksprotocol.Service, tasks.TaskRegistry, events.EventBus) {
	t.Helper()
	reg, bus := newTestRegistry(t)
	proj, err := tasksprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(proj, tasksprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, reg, bus
}

func scopeOf(tenant, user, session string) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: tenant, User: user, Session: session}
}

func TestList_FilterMatrix(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")

	// Seed a known mix.
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusRunning, "alpha task", "find widgets")
	seedTask(t, reg, id, tasks.KindBackground, tasks.StatusPending, "beta job", "index docs")
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusFailed, "gamma task", "scrape page")
	seedTask(t, reg, id, tasks.KindBackground, tasks.StatusComplete, "delta job", "summarise")

	ctx := context.Background()

	t.Run("no filter returns all", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scopeOf("t1", "u1", "s1")}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(resp.Rows) < 4 {
			t.Fatalf("want >=4 rows, got %d", len(resp.Rows))
		}
	})

	t.Run("status facet honored", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scopeOf("t1", "u1", "s1"),
			Filter:   prototypes.TaskFilter{Statuses: []prototypes.TaskStatus{prototypes.TaskStatusRunning}},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range resp.Rows {
			if r.Status != prototypes.TaskStatusRunning {
				t.Errorf("status filter leaked %q", r.Status)
			}
		}
	})

	t.Run("kind facet honored", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scopeOf("t1", "u1", "s1"),
			Filter:   prototypes.TaskFilter{Kinds: []prototypes.TaskKind{prototypes.TaskKindBackground}},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range resp.Rows {
			if r.Kind != prototypes.TaskKindBackground {
				t.Errorf("kind filter leaked %q", r.Kind)
			}
		}
	})

	t.Run("free-text search honored", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scopeOf("t1", "u1", "s1"),
			Filter:   prototypes.TaskFilter{Search: "gamma"},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(resp.Rows) != 1 || resp.Rows[0].Description != "gamma task" {
			t.Fatalf("search 'gamma' want 1 row, got %d", len(resp.Rows))
		}
	})

	t.Run("aggregates match the seed", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scopeOf("t1", "u1", "s1")}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if resp.Aggregates.Running < 1 || resp.Aggregates.Failed < 1 ||
			resp.Aggregates.Pending < 1 || resp.Aggregates.Complete < 1 {
			t.Fatalf("aggregates missing a status bucket: %+v", resp.Aggregates)
		}
	})

	t.Run("error-class facet honored", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scopeOf("t1", "u1", "s1"),
			Filter:   prototypes.TaskFilter{ErrorClasses: []string{"tool_timeout"}},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range resp.Rows {
			if r.ErrorClass != "tool_timeout" {
				t.Errorf("error-class filter leaked %q", r.ErrorClass)
			}
		}
	})
}

func TestList_CursorPagination(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")
	for range 7 {
		seedTask(t, reg, id, tasks.KindForeground, tasks.StatusPending, "task", "q")
	}
	ctx := context.Background()
	scope := scopeOf("t1", "u1", "s1")

	page1, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scope, PageSize: 3}, false)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Rows) != 3 || page1.Cursor.NextPageToken == "" {
		t.Fatalf("page 1: want 3 rows + a cursor, got %d rows cursor=%q", len(page1.Rows), page1.Cursor.NextPageToken)
	}
	page2, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scope, PageSize: 3, Cursor: page1.Cursor}, false)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Rows) != 3 {
		t.Fatalf("page 2: want 3 rows, got %d", len(page2.Rows))
	}
	// No row appears on both pages.
	seen := map[string]bool{}
	for _, r := range page1.Rows {
		seen[r.ID] = true
	}
	for _, r := range page2.Rows {
		if seen[r.ID] {
			t.Errorf("row %q appeared on both pages", r.ID)
		}
	}
}

func TestList_IdentityMandatory(t *testing.T) {
	svc, _, _ := newListService(t)
	ctx := context.Background()
	for _, tc := range []struct {
		scope prototypes.IdentityScope
		name  string
	}{
		{name: "missing tenant", scope: scopeOf("", "u1", "s1")},
		{name: "missing user", scope: scopeOf("t1", "", "s1")},
		{name: "missing session", scope: scopeOf("t1", "u1", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.List(ctx, prototypes.TaskListRequest{Identity: tc.scope}, false)
			if !errors.Is(err, tasksprotocol.ErrIdentityRequired) {
				t.Fatalf("want ErrIdentityRequired, got %v", err)
			}
		})
	}
}

func TestList_CrossTenant_RequiresAdmin(t *testing.T) {
	svc, _, bus := newListService(t)
	ctx := context.Background()

	req := prototypes.TaskListRequest{
		Identity: scopeOf("t1", "u1", "s1"),
		Filter: prototypes.TaskFilter{
			Identities: []prototypes.IdentityScope{
				{Tenant: "t1"},
				{Tenant: "t2"},
			},
		},
	}

	t.Run("non-admin cross-tenant fails closed", func(t *testing.T) {
		_, err := svc.List(ctx, req, false)
		if !errors.Is(err, tasksprotocol.ErrScopeMismatch) {
			t.Fatalf("want ErrScopeMismatch, got %v", err)
		}
	})

	t.Run("admin cross-tenant succeeds + emits audit", func(t *testing.T) {
		sub, err := bus.Subscribe(ctx, events.Filter{
			Tenant: "t1", User: "u1", Session: "s1", Admin: true,
			Types: []events.EventType{events.EventTypeAdminScopeUsed},
		})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Cancel()

		if _, err := svc.List(ctx, req, true); err != nil {
			t.Fatalf("admin List: %v", err)
		}
		select {
		case ev := <-sub.Events():
			if ev.Type != events.EventTypeAdminScopeUsed {
				t.Fatalf("want admin_scope_used, got %q", ev.Type)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no audit.admin_scope_used event observed within 2s")
		}
	})
}

func TestList_RejectsBadPageSizeAndCursor(t *testing.T) {
	svc, _, _ := newListService(t)
	ctx := context.Background()
	scope := scopeOf("t1", "u1", "s1")

	if _, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scope, PageSize: 9999}, false); !errors.Is(err, tasksprotocol.ErrInvalidRequest) {
		t.Fatalf("oversized page size: want ErrInvalidRequest, got %v", err)
	}
	if _, err := svc.List(ctx, prototypes.TaskListRequest{
		Identity: scope,
		Cursor:   prototypes.TaskListCursor{NextPageToken: "!!!not-base64!!!"},
	}, false); !errors.Is(err, tasksprotocol.ErrInvalidRequest) {
		t.Fatalf("malformed cursor: want ErrInvalidRequest, got %v", err)
	}
}

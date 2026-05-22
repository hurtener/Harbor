package tasks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"

	// Side-effect: register the inprocess driver so the
	// registered-drivers / Open paths have something to resolve.
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// TestRegisteredDrivers_IncludesInprocess pins the side-effect
// registration. cmd/harbor blank-imports the driver; the registry
// must return at least "inprocess".
func TestRegisteredDrivers_IncludesInprocess(t *testing.T) {
	got := tasks.RegisteredDrivers()
	found := false
	for _, n := range got {
		if n == "inprocess" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RegisteredDrivers=%v, want to include %q", got, "inprocess")
	}
}

// TestOpen_HonoursCfgDriver checks that Open dispatches by name.
func TestOpen_HonoursCfgDriver(t *testing.T) {
	// Open with an unknown driver → wrapped ErrUnknownDriver.
	_, err := tasks.Open(context.Background(), tasks.Dependencies{
		Cfg: config.TasksConfig{Driver: "no-such-driver"},
	})
	if !errors.Is(err, tasks.ErrUnknownDriver) {
		t.Fatalf("Open with unknown driver: err=%v, want errors.Is ErrUnknownDriver", err)
	}
}

// TestOpen_DefaultsToInprocess covers the empty-driver default.
// Pass nil deps — Open delegates to the factory, which will fail
// the deps validation; we only care that the dispatch reached the
// factory.
func TestOpen_DefaultsToInprocess(t *testing.T) {
	_, err := tasks.Open(context.Background(), tasks.Dependencies{
		// Cfg.Driver = "" → DefaultDriver lookup.
	})
	// We expect a failure inside the factory (nil StateStore) — NOT
	// ErrUnknownDriver. This proves the empty-driver path resolved
	// to "inprocess".
	if err == nil {
		t.Fatal("Open with empty driver should fail (no deps); got nil")
	}
	if errors.Is(err, tasks.ErrUnknownDriver) {
		t.Fatalf("empty-driver Open routed to ErrUnknownDriver; expected to reach the inprocess factory: %v", err)
	}
}

// TestValidateRequest_Cases pins ValidateRequest's accept/reject
// behaviour for the boundary cases other tests rely on.
func TestValidateRequest_Cases(t *testing.T) {
	good := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	cases := []struct {
		wantErr error
		name    string
		req     tasks.SpawnRequest
	}{
		{
			name: "ok foreground",
			req: tasks.SpawnRequest{
				Identity: good,
				Kind:     tasks.KindForeground,
			},
			wantErr: nil,
		},
		{
			name: "ok background",
			req: tasks.SpawnRequest{
				Identity: good,
				Kind:     tasks.KindBackground,
			},
			wantErr: nil,
		},
		{
			name:    "missing identity",
			req:     tasks.SpawnRequest{Kind: tasks.KindForeground},
			wantErr: tasks.ErrIdentityRequired,
		},
		{
			name: "empty kind",
			req: tasks.SpawnRequest{
				Identity: good,
				Kind:     "",
			},
			wantErr: tasks.ErrInvalidRequest,
		},
		{
			name: "unknown kind",
			req: tasks.SpawnRequest{
				Identity: good,
				Kind:     "weird",
			},
			wantErr: tasks.ErrInvalidRequest,
		},
		{
			name: "unknown propagate",
			req: tasks.SpawnRequest{
				Identity:          good,
				Kind:              tasks.KindForeground,
				PropagateOnCancel: "bogus",
			},
			wantErr: tasks.ErrInvalidRequest,
		},
		{
			name: "ok cascade",
			req: tasks.SpawnRequest{
				Identity:          good,
				Kind:              tasks.KindForeground,
				PropagateOnCancel: tasks.PropagateCascade,
			},
			wantErr: nil,
		},
		{
			name: "ok isolate",
			req: tasks.SpawnRequest{
				Identity:          good,
				Kind:              tasks.KindForeground,
				PropagateOnCancel: tasks.PropagateIsolate,
			},
			wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tasks.ValidateRequest(tc.req)
			if tc.wantErr == nil {
				if got != nil {
					t.Errorf("ValidateRequest=%v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.wantErr) {
				t.Errorf("ValidateRequest=%v, want errors.Is %v", got, tc.wantErr)
			}
		})
	}
}

// TestValidateToolRequest_RequiresToolName covers the SpawnTool
// boundary.
func TestValidateToolRequest_RequiresToolName(t *testing.T) {
	good := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	if err := tasks.ValidateToolRequest(tasks.SpawnToolRequest{Identity: good}); !errors.Is(err, tasks.ErrInvalidRequest) {
		t.Errorf("missing tool_name: err=%v, want ErrInvalidRequest", err)
	}
	if err := tasks.ValidateToolRequest(tasks.SpawnToolRequest{
		Identity: good,
		ToolName: "calculator.add",
	}); err != nil {
		t.Errorf("valid tool request: err=%v, want nil", err)
	}
}

// TestSentinelErrors_Distinct ensures each sentinel is its own
// singleton (errors.Is on one does NOT match another). Guards
// against a future maintainer accidentally aliasing.
func TestSentinelErrors_Distinct(t *testing.T) {
	all := []error{
		tasks.ErrNotFound,
		tasks.ErrInvalidTransition,
		tasks.ErrIdempotencyConflict,
		tasks.ErrIdentityRequired,
		tasks.ErrUnknownDriver,
		tasks.ErrRegistryClosed,
		tasks.ErrInvalidRequest,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel %v aliases %v (errors.Is)", a, b)
			}
		}
	}
}

// TestCtxHelpers_RoundTrip exercises WithRegistry / From / MustFrom.
func TestCtxHelpers_RoundTrip(t *testing.T) {
	if _, ok := tasks.From(context.Background()); ok {
		t.Errorf("From on bare ctx returned ok=true")
	}
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("MustFrom on bare ctx did not panic")
		}
	}()
	_ = tasks.MustFrom(context.Background())
}

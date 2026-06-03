package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
)

// fakeController is an in-test Controller — a deterministic fixture for
// the fleet-control unit tests (NOT a production default; it lives in a
// _test.go file per CLAUDE.md §13). The real production controller is the
// in-process *registry.Registry, exercised against a real registry in the
// handler + smoke tests.
type fakeController struct {
	called     string // the last command invoked
	gotID      string
	gotReason  string
	sawControl bool // whether the ctx carried the registry control-scope claim
	err        error
}

func (f *fakeController) record(ctx context.Context, cmd, id, reason string) error {
	f.called = cmd
	f.gotID = id
	f.gotReason = reason
	// The Service MUST attach the registry control-scope claim before
	// invoking the controller — the registry's own HasControlScope gate
	// requires it (D-066).
	f.sawControl = registry.HasControlScope(ctx)
	return f.err
}

func (f *fakeController) Pause(ctx context.Context, id, reason string) error {
	return f.record(ctx, "pause", id, reason)
}
func (f *fakeController) Drain(ctx context.Context, id, reason string) error {
	return f.record(ctx, "drain", id, reason)
}
func (f *fakeController) Restart(ctx context.Context, id, reason string) error {
	return f.record(ctx, "restart", id, reason)
}
func (f *fakeController) ForceStop(ctx context.Context, id, reason string) error {
	return f.record(ctx, "force_stop", id, reason)
}
func (f *fakeController) Deregister(ctx context.Context, id string) error {
	return f.record(ctx, "deregister", id, "")
}

// controlID is a documented dummy identity triple — no secrets.
var controlScope = prototypes.IdentityScope{Tenant: "t-ctl", User: "u-ctl", Session: "s-ctl"}

func newControlService(t *testing.T, ctrl agentsprotocol.Controller) *agentsprotocol.Service {
	t.Helper()
	opts := []agentsprotocol.Option{}
	if ctrl != nil {
		opts = append(opts, agentsprotocol.WithController(ctrl))
	}
	svc, err := agentsprotocol.NewService(&fakeProjector{}, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestService_Control_WithoutScope_FailsClosed(t *testing.T) {
	fc := &fakeController{}
	svc := newControlService(t, fc)
	// controlScoped=false — the gate must fail closed BEFORE touching the
	// controller (a leaked read-only token can never drive a control verb).
	_, err := svc.Pause(context.Background(),
		prototypes.AgentControlRequest{Identity: controlScope, ID: "agent-1"}, false)
	if !errors.Is(err, agentsprotocol.ErrControlScopeRequired) {
		t.Fatalf("Pause(controlScoped=false) err = %v, want ErrControlScopeRequired", err)
	}
	if fc.called != "" {
		t.Fatalf("controller invoked despite missing scope: %q", fc.called)
	}
}

func TestService_Control_NoController_Unsupported(t *testing.T) {
	svc := newControlService(t, nil) // no WithController
	_, err := svc.Drain(context.Background(),
		prototypes.AgentControlRequest{Identity: controlScope, ID: "agent-1"}, true)
	if !errors.Is(err, agentsprotocol.ErrControlUnsupported) {
		t.Fatalf("Drain(no controller) err = %v, want ErrControlUnsupported", err)
	}
}

func TestService_Control_EmptyID(t *testing.T) {
	svc := newControlService(t, &fakeController{})
	_, err := svc.Pause(context.Background(),
		prototypes.AgentControlRequest{Identity: controlScope, ID: "  "}, true)
	if !errors.Is(err, agentsprotocol.ErrInvalidRequest) {
		t.Fatalf("Pause(empty id) err = %v, want ErrInvalidRequest", err)
	}
}

func TestService_Control_IdentityMandatory(t *testing.T) {
	svc := newControlService(t, &fakeController{})
	_, err := svc.Pause(context.Background(),
		prototypes.AgentControlRequest{ID: "agent-1"}, true) // no identity
	if !errors.Is(err, agentsprotocol.ErrIdentityRequired) {
		t.Fatalf("Pause(no identity) err = %v, want ErrIdentityRequired", err)
	}
}

func TestService_Control_NotFound(t *testing.T) {
	fc := &fakeController{err: registry.ErrAgentNotFound}
	svc := newControlService(t, fc)
	_, err := svc.ForceStop(context.Background(),
		prototypes.AgentControlRequest{Identity: controlScope, ID: "ghost"}, true)
	if !errors.Is(err, agentsprotocol.ErrAgentNotFound) {
		t.Fatalf("ForceStop(unknown) err = %v, want ErrAgentNotFound", err)
	}
}

func TestService_Control_HappyPath_AllVerbs(t *testing.T) {
	// The four non-deregister verbs report the agent's ACTUAL post-control
	// status, re-read from the projector (here the fake returns "active");
	// deregister reports the terminal "deregistered" without a re-read.
	cases := []struct {
		name    string
		invoke  func(*agentsprotocol.Service, prototypes.AgentControlRequest) (prototypes.AgentControlResponse, error)
		command string
		status  prototypes.AgentStatus
	}{
		{"pause", func(s *agentsprotocol.Service, r prototypes.AgentControlRequest) (prototypes.AgentControlResponse, error) {
			return s.Pause(context.Background(), r, true)
		}, "pause", prototypes.AgentStatusActive},
		{"drain", func(s *agentsprotocol.Service, r prototypes.AgentControlRequest) (prototypes.AgentControlResponse, error) {
			return s.Drain(context.Background(), r, true)
		}, "drain", prototypes.AgentStatusActive},
		{"restart", func(s *agentsprotocol.Service, r prototypes.AgentControlRequest) (prototypes.AgentControlResponse, error) {
			return s.Restart(context.Background(), r, true)
		}, "restart", prototypes.AgentStatusActive},
		{"force_stop", func(s *agentsprotocol.Service, r prototypes.AgentControlRequest) (prototypes.AgentControlResponse, error) {
			return s.ForceStop(context.Background(), r, true)
		}, "force_stop", prototypes.AgentStatusActive},
		{"deregister", func(s *agentsprotocol.Service, r prototypes.AgentControlRequest) (prototypes.AgentControlResponse, error) {
			return s.Deregister(context.Background(), r, true)
		}, "deregister", prototypes.AgentStatusDeregistered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeController{}
			// The projector re-read returns "active" so the four non-
			// deregister verbs echo the real post-control status.
			fp := &fakeProjector{getResp: prototypes.AgentGetResponse{
				Agent: prototypes.Agent{ID: "agent-1", Status: prototypes.AgentStatusActive},
			}}
			svc, err := agentsprotocol.NewService(fp, agentsprotocol.WithController(fc))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			req := prototypes.AgentControlRequest{Identity: controlScope, ID: "agent-1", Reason: "maintenance"}
			resp, err := tc.invoke(svc, req)
			if err != nil {
				t.Fatalf("%s err = %v, want nil", tc.name, err)
			}
			if resp.Command != tc.command {
				t.Fatalf("%s resp.Command = %q, want %q", tc.name, resp.Command, tc.command)
			}
			if resp.Status != tc.status {
				t.Fatalf("%s resp.Status = %q, want %q", tc.name, resp.Status, tc.status)
			}
			if resp.AgentID != "agent-1" {
				t.Fatalf("%s resp.AgentID = %q, want agent-1", tc.name, resp.AgentID)
			}
			if fc.called != tc.command {
				t.Fatalf("%s invoked controller %q, want %q", tc.name, fc.called, tc.command)
			}
			if fc.gotID != "agent-1" {
				t.Fatalf("%s controller got id %q, want agent-1", tc.name, fc.gotID)
			}
			if !fc.sawControl {
				t.Fatalf("%s: ctx did not carry the registry control-scope claim", tc.name)
			}
			// Deregister carries no reason to the registry (registry.Deregister
			// takes no reason); the other four pass it through.
			if tc.command == "deregister" {
				if fc.gotReason != "" {
					t.Fatalf("deregister passed a reason %q, want empty", fc.gotReason)
				}
			} else if fc.gotReason != "maintenance" {
				t.Fatalf("%s controller got reason %q, want maintenance", tc.name, fc.gotReason)
			}
		})
	}
}

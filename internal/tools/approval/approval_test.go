package approval

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestErrToolRejected_ErrorAndIs(t *testing.T) {
	e := &ErrToolRejected{
		Tool:   "summarize",
		Reason: "policy: deny-all",
		Identity: identity.Identity{
			TenantID: "t1", UserID: "u1", SessionID: "s1",
		},
	}
	if !errors.Is(e, ErrToolRejectedSentinel) {
		t.Fatal("errors.Is against sentinel: want match")
	}
	if e.Error() == "" {
		t.Fatal("Error() empty")
	}
	// nil-receiver path
	var nilErr *ErrToolRejected
	if nilErr.Error() != "approval: <nil ErrToolRejected>" {
		t.Fatalf("nil Error: %q", nilErr.Error())
	}
}

func TestErrToolRejected_Error_NoReason(t *testing.T) {
	e := &ErrToolRejected{Tool: "x"}
	got := e.Error()
	want := "approval: tool x rejected"
	if got != want {
		t.Fatalf("Error = %q want %q", got, want)
	}
}

func TestIsValidDecision(t *testing.T) {
	cases := []struct {
		d    ApprovalDecision
		want bool
	}{
		{DecisionApprove, true},
		{DecisionReject, true},
		{DecisionPending, false},
		{ApprovalDecision("garbage"), false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsValidDecision(c.d); got != c.want {
			t.Errorf("IsValidDecision(%q) = %v, want %v", c.d, got, c.want)
		}
	}
}

func TestApprovalRequest_Validate(t *testing.T) {
	good := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	cases := []struct {
		name    string
		req     *ApprovalRequest
		wantErr error
	}{
		{
			name:    "nil",
			req:     nil,
			wantErr: errors.New("approval: nil ApprovalRequest"),
		},
		{
			name: "empty tool name",
			req: &ApprovalRequest{
				Identity: good,
			},
			wantErr: errors.New("approval: ApprovalRequest.Tool.Name empty"),
		},
		{
			name: "missing identity",
			req: &ApprovalRequest{
				Tool: tools.Tool{Name: "summarize"},
			},
			wantErr: ErrIdentityRequired,
		},
		{
			name: "happy",
			req: &ApprovalRequest{
				Tool:     tools.Tool{Name: "summarize"},
				Identity: good,
			},
			wantErr: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.req.Validate()
			if c.wantErr == nil {
				if got != nil {
					t.Fatalf("Validate: got %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Validate: got nil, want %v", c.wantErr)
			}
			// For ErrIdentityRequired we use errors.Is via Join.
			if errors.Is(c.wantErr, ErrIdentityRequired) {
				if !errors.Is(got, ErrIdentityRequired) {
					t.Fatalf("Validate: got %v, want errors.Is(ErrIdentityRequired)", got)
				}
				return
			}
			// Plain string match for the rest.
			if got.Error() != c.wantErr.Error() {
				t.Fatalf("Validate: got %q, want %q", got.Error(), c.wantErr.Error())
			}
		})
	}
}

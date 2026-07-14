package protocol_test

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// failingRedactor refuses every payload — used to force the audit emit to
// fail so the fail-closed compensate ordering can be observed.
type failingRedactor struct{}

func (failingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, stderrors.New("redactor refused (forced test failure)")
}

// TestSetRawHTMLTrust_AuditEmitFailure_LeavesTrustUnchanged pins the D-300
// audit-ordering fix: when the audit emit fails, the trust toggle is
// COMPENSATED (reverted) so an un-auditable admin write is never left
// observably applied. The call surfaces a runtime error; the trust flag is
// unchanged.
func TestSetRawHTMLTrust_AuditEmitFailure_LeavesTrustUnchanged(t *testing.T) {
	stub := &stubMCP{servers: map[string]protocol.MCPServerRow{
		"github-server": {Name: "github-server", RawHTMLTrusted: false},
	}}
	bus := newMCPBus(t)
	s, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:      stub,
		OAuth:    &stubOAuth{},
		Redactor: failingRedactor{}, // forces the emit to fail
		Bus:      bus,
		Clock:    func() time.Time { return time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}

	adminCtx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	_, err = s.Dispatch(adminCtx, methods.MethodMCPServersSetRawHTMLTrust,
		&types.MCPServerSetRawHTMLTrustRequest{Identity: validScope(), Name: "github-server", Trusted: true})
	if err == nil {
		t.Fatal("set_raw_html_trust succeeded despite a failed audit emit — the write must fail closed")
	}
	var perr *protoerrors.Error
	if !stderrors.As(err, &perr) || perr.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("want CodeRuntimeError, got %v", err)
	}

	// The mutation must have been reverted (compensated), so the trust flag
	// is unchanged from its prior (false) value.
	if got := stub.servers["github-server"].RawHTMLTrusted; got != false {
		t.Fatalf("trust flag = %v after a failed audit emit; want unchanged (false) — the toggle must be reverted", got)
	}
}

// TestSetRawHTMLTrust_AuditSucceeds_TrustApplied is the positive control:
// with a working redactor the toggle applies and stays applied.
func TestSetRawHTMLTrust_AuditSucceeds_TrustApplied(t *testing.T) {
	stub := &stubMCP{servers: map[string]protocol.MCPServerRow{
		"github-server": {Name: "github-server", RawHTMLTrusted: false},
	}}
	bus := newMCPBus(t)
	s, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:      stub,
		OAuth:    &stubOAuth{},
		Redactor: patterns.New(),
		Bus:      bus,
		Clock:    func() time.Time { return time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}
	adminCtx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	if _, err := s.Dispatch(adminCtx, methods.MethodMCPServersSetRawHTMLTrust,
		&types.MCPServerSetRawHTMLTrustRequest{Identity: validScope(), Name: "github-server", Trusted: true}); err != nil {
		t.Fatalf("set_raw_html_trust: %v", err)
	}
	if got := stub.servers["github-server"].RawHTMLTrusted; got != true {
		t.Fatalf("trust flag = %v; want applied (true)", got)
	}
}

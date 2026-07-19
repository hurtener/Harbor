package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestClassifyError_InsufficientScope_Permanent proves ClassifyError maps a
// *tools.ErrInsufficientScope (even wrapped) to ErrClassPermanent — closing
// the retry-storm bug where an unclassified 4xx-shaped error falls through to
// the conservative ErrClassTransient default.
func TestClassifyError_InsufficientScope_Permanent(t *testing.T) {
	t.Parallel()
	scopeErr := &tools.ErrInsufficientScope{
		Source:         "gcal",
		ToolName:       "list_events",
		RequiredScopes: []string{"read:calendar"},
		Origin:         "https://mcp.example.com",
	}
	if got := tools.ClassifyError(scopeErr, false); got != tools.ErrClassPermanent {
		t.Fatalf("ClassifyError(direct) = %s, want permanent", got)
	}
	wrapped := errors.Join(errors.New("mcp: call failed"), scopeErr)
	if got := tools.ClassifyError(wrapped, true); got != tools.ErrClassPermanent {
		t.Fatalf("ClassifyError(wrapped) = %s, want permanent", got)
	}
}

// TestRunWithPolicy_InsufficientScope_OneAttempt proves the reliability shell
// makes exactly ONE attempt for an *ErrInsufficientScope (permanent class),
// never retrying a shortfall retrying can never fix.
func TestRunWithPolicy_InsufficientScope_OneAttempt(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	_, err := tools.RunWithPolicy(context.Background(), json.RawMessage(`{}`),
		func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			attempts.Add(1)
			return tools.ToolResult{}, &tools.ErrInsufficientScope{ToolName: "t", Origin: "https://x"}
		},
		nil, nil,
		tools.ToolPolicy{MaxRetries: 5}, // would retry 5x if the class were retryable
	)
	var scopeErr *tools.ErrInsufficientScope
	if !errors.As(err, &scopeErr) {
		t.Fatalf("err = %v, want *ErrInsufficientScope", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (permanent class stops the loop)", n)
	}
}

// TestCatalogLifecycle_ScopeShortfall_OnToolFailed proves the universal
// lifecycle shell enriches tool.failed with the structured ScopeShortfall
// when the terminal error is an *ErrInsufficientScope, and leaves it nil
// otherwise.
func TestCatalogLifecycle_ScopeShortfall_OnToolFailed(t *testing.T) {
	t.Parallel()
	scopeErr := &tools.ErrInsufficientScope{
		Source:             "gcal",
		ToolName:           "list_events",
		DownstreamResource: "mcp.example.com",
		RequiredScopes:     []string{"read:calendar", "write:calendar"},
		GrantedScopes:      []string{"read:calendar"},
		WWWAuthenticate:    `Bearer error="insufficient_scope", scope="read:calendar write:calendar"`,
		Origin:             "https://mcp.example.com",
	}

	cases := []struct {
		name          string
		retErr        error
		wantShortfall bool
	}{
		{"insufficient_scope", scopeErr, true},
		{"plain_failure", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bus := newLifecycleBus(t)
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Admin: true,
				Types: []events.EventType{tools.EventTypeToolFailed},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			cat := tools.NewCatalog(tools.WithCatalogBus(bus))
			registerInvoke(t, cat, "tool", tools.TransportMCP, func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, tc.retErr
			})
			desc, _ := cat.Resolve("tool")
			_, _ = desc.Invoke(lifecycleCtx(t), json.RawMessage(`{}`))

			evs := collectTypes(t, sub, 1)
			if len(evs) == 0 {
				t.Fatalf("no tool.failed emitted")
			}
			p, ok := evs[0].Payload.(tools.ToolFailedPayload)
			if !ok {
				t.Fatalf("payload type %T", evs[0].Payload)
			}
			if tc.wantShortfall {
				if p.ScopeShortfall == nil {
					t.Fatalf("ScopeShortfall nil, want populated")
				}
				if p.ScopeShortfall.DownstreamResource != "mcp.example.com" {
					t.Errorf("DownstreamResource = %q", p.ScopeShortfall.DownstreamResource)
				}
				if len(p.ScopeShortfall.RequiredScopes) != 2 || len(p.ScopeShortfall.GrantedScopes) != 1 {
					t.Errorf("scopes required=%v granted=%v", p.ScopeShortfall.RequiredScopes, p.ScopeShortfall.GrantedScopes)
				}
				if p.ScopeShortfall.WWWAuthenticate == "" || p.ScopeShortfall.Origin == "" {
					t.Errorf("missing verbatim header/origin: %+v", p.ScopeShortfall)
				}
			} else if p.ScopeShortfall != nil {
				t.Fatalf("ScopeShortfall populated for a plain failure: %+v", p.ScopeShortfall)
			}
		})
	}
}

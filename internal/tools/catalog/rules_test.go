package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

// ruleProbe reports whether wrap gated d: a gated descriptor's Invoke
// first consults the gate (deny-all parks; the probe ctx has no
// identity, so the wrapper fails closed with ErrIdentityRequired before
// reaching the tool), an ungated one runs the tool directly.
func ruleGated(t *testing.T, wrap func(tools.ToolDescriptor) tools.ToolDescriptor, tool tools.Tool) bool {
	t.Helper()
	ran := false
	d := wrap(tools.ToolDescriptor{Tool: tool, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		ran = true
		return tools.ToolResult{}, nil
	}})
	_, err := d.Invoke(context.Background(), nil)
	if ran {
		return false
	}
	if !errors.Is(err, approval.ErrIdentityRequired) {
		t.Fatalf("%s: unexpected invoke error %v", tool.Name, err)
	}
	return true
}

func TestBuildApprovalRules_Matching(t *testing.T) {
	t.Parallel()
	bus := newRulesBus(t)
	rules := []config.ToolApprovalRuleConfig{
		{Tools: []string{"crm_delete_*"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}},
		{Sources: []string{"files"}, Annotations: []string{"not_read_only"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}},
		{Annotations: []string{"destructive"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	entries := []config.ToolEntryConfig{{Name: "crm_delete_pinned", LoadingMode: "deferred"}}
	built, err := catalog.BuildApprovalRules(rules, entries, catalog.Deps{
		Coordinator: pauseresume.New(), Bus: bus, Redactor: patternsAudit.New(), Authorizer: approval.NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatalf("BuildApprovalRules: %v", err)
	}
	if len(built.Gates) != 3 {
		t.Fatalf("gates = %d, want one per rule", len(built.Gates))
	}
	mcp := tools.TransportMCP
	cases := []struct {
		tool tools.Tool
		want bool
	}{
		{tools.Tool{Name: "crm_delete_record", Source: "crm", Transport: mcp}, true},
		{tools.Tool{Name: "crm_read_record", Source: "crm", Transport: mcp, ReadOnlyHint: true}, false},
		{tools.Tool{Name: "crm_delete_pinned", Source: "crm", Transport: mcp}, false}, // named in entries[]
		{tools.Tool{Name: "files_write", Source: "files", Transport: mcp}, true},
		{tools.Tool{Name: "files_list", Source: "files", Transport: mcp, ReadOnlyHint: true}, false},
		{tools.Tool{Name: "web_purge", Source: "web", Transport: mcp, DestructiveHint: true}, true},
		{tools.Tool{Name: "web_fetch", Source: "web", Transport: mcp}, false},
		// Annotation selectors match MCP tools only.
		{tools.Tool{Name: "local_purge", DestructiveHint: true, Transport: tools.TransportInProcess}, false},
	}
	for _, tc := range cases {
		if got := ruleGated(t, built.Wrap, tc.tool); got != tc.want {
			t.Errorf("%s gated = %v, want %v", tc.tool.Name, got, tc.want)
		}
	}
}

func TestBuildApprovalRules_EmptyAndMisconfigured(t *testing.T) {
	t.Parallel()
	built, err := catalog.BuildApprovalRules(nil, nil, catalog.Deps{})
	if err != nil || built.Wrap != nil || len(built.Gates) != 0 {
		t.Fatalf("no rules = %+v, %v", built, err)
	}
	_, err = catalog.BuildApprovalRules([]config.ToolApprovalRuleConfig{{Tools: []string{"x"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}}}, nil, catalog.Deps{})
	if !errors.Is(err, catalog.ErrCoordinatorRequired) {
		t.Fatalf("missing deps err = %v", err)
	}
}

func newRulesBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 64,
		IdleTimeout: 2 * time.Second, DropWindow: 50 * time.Millisecond,
	}, patternsAudit.New())
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// Owner-scoped copies of a connection (`<name>~u-…` / `~a-…`) cannot
// bypass a rule written for the configured name, by source or tool glob.
func TestBuildApprovalRules_MatchesLogicalConnectionName(t *testing.T) {
	t.Parallel()
	built, err := catalog.BuildApprovalRules([]config.ToolApprovalRuleConfig{
		{Sources: []string{"mail"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}},
		{Tools: []string{"crm_delete_*"}, Approval: config.ToolApprovalConfig{Policy: "deny-all"}},
	}, nil, catalog.Deps{
		Coordinator: pauseresume.New(), Bus: newRulesBus(t), Redactor: patternsAudit.New(), Authorizer: approval.NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcp := tools.TransportMCP
	for _, tc := range []struct {
		tool tools.Tool
		want bool
	}{
		{tools.Tool{Name: "mail~u-0a1b2c_send", Source: "mail~u-0a1b2c", Transport: mcp}, true},
		{tools.Tool{Name: "mail~a-0a1b2c_send", Source: "mail~a-0a1b2c", Transport: mcp}, true},
		{tools.Tool{Name: "crm~u-99_delete_record", Source: "crm~u-99", Transport: mcp}, true},
		{tools.Tool{Name: "crm~u-99_read_record", Source: "crm~u-99", Transport: mcp}, false},
		{tools.Tool{Name: "mailer_send", Source: "mailer", Transport: mcp}, false},
	} {
		if got := built.Matches(tc.tool); got != tc.want {
			t.Errorf("%s matched = %v, want %v", tc.tool.Name, got, tc.want)
		}
		if got := ruleGated(t, built.Wrap, tc.tool); got != tc.want {
			t.Errorf("%s gated = %v, want %v", tc.tool.Name, got, tc.want)
		}
	}
}

package catalog

import (
	"fmt"
	"path"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// Approval rules gate tools that no `tools.entries[]` row can name in
// advance — tools an MCP server discovers at runtime — by name / source
// glob and by declared annotation. Each rule owns one ApprovalGate
// (gates key pending approvals by pause token, so one gate serves every
// tool the rule matches). The returned wrapper is installed on the
// catalog with tools.WithRegistrationGate, so it applies to every
// registration, including MCP refreshes and Protocol-installed
// connections. A tool named in any entries[] row is governed by that
// row and skipped here, so a tool is never gated twice and the
// entries[] wrapper order (approval outside OAuth) is unchanged.

// RuleGates is the result of BuildApprovalRules.
type RuleGates struct {
	// Wrap is the catalog registration wrapper (nil when there are no
	// rules).
	Wrap func(tools.ToolDescriptor) tools.ToolDescriptor
	// Gates maps a stable key per rule ("approval_rules[i]") to its
	// gate, for the steering approval bridge and shutdown.
	Gates map[string]*approval.ApprovalGate
	// Matches reports whether a rule gates t (nil when there are no
	// rules). Surfaces that invoke descriptors outside the planner
	// catalog (rendered-App callbacks) consult it so a rule-gated tool
	// is never run ungated.
	Matches func(tools.Tool) bool
}

type compiledRule struct {
	cfg  config.ToolApprovalRuleConfig
	gate *approval.ApprovalGate
}

// BuildApprovalRules compiles tools.approval_rules into per-rule gates
// and the catalog registration wrapper. deps supplies the gate
// collaborators (Coordinator, Bus, Redactor and Authorizer are
// mandatory when rules exist). entries is the
// tools.entries list whose names the rules never touch.
func BuildApprovalRules(rules []config.ToolApprovalRuleConfig, entries []config.ToolEntryConfig, deps Deps) (RuleGates, error) {
	out := RuleGates{Gates: map[string]*approval.ApprovalGate{}}
	if len(rules) == 0 {
		return out, nil
	}
	switch {
	case deps.Coordinator == nil:
		return RuleGates{}, ErrCoordinatorRequired
	case deps.Bus == nil:
		return RuleGates{}, ErrBusRequired
	case deps.Redactor == nil:
		return RuleGates{}, ErrRedactorRequired
	case deps.Authorizer == nil:
		return RuleGates{}, ErrAuthorizerRequired
	}
	compiled := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		approvalCfg := r.Approval
		policy, err := resolveApprovalPolicy(&approvalCfg)
		if err != nil {
			return RuleGates{}, fmt.Errorf("approval_rules[%d]: %w", i, err)
		}
		gate, err := approval.NewApprovalGate(approval.GateDeps{
			Policy:      policy,
			Coordinator: deps.Coordinator,
			Bus:         deps.Bus,
			Redactor:    deps.Redactor,
			Authorizer:  deps.Authorizer,
		})
		if err != nil {
			return RuleGates{}, fmt.Errorf("approval_rules[%d]: approval.NewApprovalGate: %w", i, err)
		}
		out.Gates[fmt.Sprintf("approval_rules[%d]", i)] = gate
		compiled = append(compiled, compiledRule{cfg: r, gate: gate})
	}
	named := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		named[e.Name] = struct{}{}
	}
	match := func(t tools.Tool) *compiledRule {
		if _, ok := named[t.Name]; ok {
			return nil
		}
		for i := range compiled {
			if ruleMatches(compiled[i].cfg, t) {
				return &compiled[i]
			}
		}
		return nil
	}
	out.Matches = func(t tools.Tool) bool { return match(t) != nil }
	out.Wrap = func(d tools.ToolDescriptor) tools.ToolDescriptor {
		r := match(d.Tool)
		if r == nil {
			return d
		}
		// The tagged policy matches against the tool's own tags.
		return WrapWithApproval(d, r.gate, ApprovalWrapperOptions{
			Tags: append([]string(nil), d.Tool.Tags...),
		})
	}
	return out, nil
}

// ruleMatches reports whether t satisfies every selector the rule sets
// (a selector list matches when any of its values matches).
//
// An owner-scoped connection registers under a physical source id
// (`<name>~u-<digest>` / `<name>~a-<digest>`) and its tools under
// `<physical>_<tool>`; rules also match the logical connection name and
// `<logical>_<tool>`, so a rule written for the configured name cannot be
// bypassed by an owner-scoped copy of the connection.
func ruleMatches(r config.ToolApprovalRuleConfig, t tools.Tool) bool {
	logical := tools.LogicalSourceName(t)
	if len(r.Tools) > 0 && !anyGlob(r.Tools, t.Name) &&
		(logical == string(t.Source) || !anyGlob(r.Tools, logical+"_"+tools.ReadableToolName(t))) {
		return false
	}
	if len(r.Sources) > 0 && (t.Source == "" || (!anyGlob(r.Sources, string(t.Source)) && !anyGlob(r.Sources, logical))) {
		return false
	}
	// Annotation selectors read MCP-declared hints, so they only match
	// MCP tools; other transports declare no annotations.
	if len(r.Annotations) > 0 && (t.Transport != tools.TransportMCP || !anyAnnotation(r.Annotations, t)) {
		return false
	}
	return true
}

func anyGlob(patterns []string, name string) bool {
	for _, p := range patterns {
		// Patterns are validated at config load; a malformed one
		// matches nothing.
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

func anyAnnotation(selectors []string, t tools.Tool) bool {
	for _, s := range selectors {
		switch s {
		case "destructive":
			if t.DestructiveHint && !t.ReadOnlyHint {
				return true
			}
		case "not_read_only":
			if !t.ReadOnlyHint {
				return true
			}
		}
	}
	return false
}

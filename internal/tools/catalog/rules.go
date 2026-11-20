package catalog

import (
	"context"
	"fmt"
	"path"
	"slices"

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
	// Matches reports whether the first matching boot rule requires approval.
	// App callbacks consult it before invoking tools outside the planner
	// catalog. It is nil when there are no rules.
	Matches func(tools.Tool) bool
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
	rules = slices.Clone(rules)
	gates := make([]*approval.ApprovalGate, len(rules))
	policies := make([]approval.ApprovalPolicy, len(rules))
	for i, r := range rules {
		approvalCfg := r.Approval
		policy, err := resolveApprovalPolicy(&approvalCfg)
		if err != nil {
			return RuleGates{}, fmt.Errorf("approval_rules[%d]: %w", i, err)
		}
		policies[i] = policy
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
		gates[i] = gate
	}
	named := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		named[e.Name] = struct{}{}
	}
	match := func(t tools.Tool) int {
		if _, ok := named[t.Name]; ok {
			return -1
		}
		return firstApprovalRule(rules, t)
	}
	out.Matches = func(t tools.Tool) bool {
		i := match(t)
		return i >= 0 && configuredApprovalRequired(policies[i], t, t.Tags)
	}
	out.Wrap = func(d tools.ToolDescriptor) tools.ToolDescriptor {
		i := match(d.Tool)
		if i < 0 {
			return d
		}
		// The tagged policy matches against the tool's own tags.
		d.ConfiguredApprovalRequired = d.ConfiguredApprovalRequired || configuredApprovalRequired(policies[i], d.Tool, d.Tool.Tags)
		return WrapWithApproval(d, gates[i], ApprovalWrapperOptions{Tags: d.Tool.Tags})
	}
	return out, nil
}

// ApprovalRuleRequiresApproval evaluates the first rule that selects the tool.
// Callers without an interactive approval flow use the verdict before invoking.
func ApprovalRuleRequiresApproval(rules []config.ToolApprovalRuleConfig, t tools.Tool) (bool, error) {
	i := firstApprovalRule(rules, t)
	if i < 0 {
		return false, nil
	}
	policy, err := resolveApprovalPolicy(&rules[i].Approval)
	if err != nil {
		return false, fmt.Errorf("approval rule %d: %w", i, err)
	}
	return configuredApprovalRequired(policy, t, t.Tags), nil
}

func configuredApprovalRequired(policy approval.ApprovalPolicy, tool tools.Tool, tags []string) bool {
	// The bundled policies are static and do not inspect request identity,
	// arguments, or context. Fail closed if a policy cannot give a verdict.
	required, _, err := policy.ShouldApprove(context.Background(), &approval.ApprovalRequest{Tool: tool, Tags: tags})
	return required || err != nil
}

func firstApprovalRule(rules []config.ToolApprovalRuleConfig, t tools.Tool) int {
	for i, rule := range rules {
		if ruleMatches(rule, t) {
			return i
		}
	}
	return -1
}

type revisionApprovalPolicyKey struct{}
type revisionApprovalPolicy struct{}

func (revisionApprovalPolicy) ShouldApprove(ctx context.Context, req *approval.ApprovalRequest) (bool, string, error) {
	policy, ok := ctx.Value(revisionApprovalPolicyKey{}).(approval.ApprovalPolicy)
	if !ok {
		return false, "", approval.ErrPolicyRequired
	}
	required, reason, err := policy.ShouldApprove(ctx, req)
	if err == nil && required && tools.IsUnattendedToolInvocation(ctx) {
		return false, "", approval.ErrInteractiveApprovalUnavailable
	}
	return required, reason, err
}

// NewRevisionApprovalGate constructs the single gate shared by active agent
// revisions. Each invocation supplies the policy from its own run snapshot.
func NewRevisionApprovalGate(deps Deps) (*approval.ApprovalGate, error) {
	gate, err := approval.NewApprovalGate(approval.GateDeps{
		Policy:      revisionApprovalPolicy{},
		Coordinator: deps.Coordinator,
		Bus:         deps.Bus,
		Redactor:    deps.Redactor,
		Authorizer:  deps.Authorizer,
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: revision approvals: %w", err)
	}
	return gate, nil
}

// CompileRevisionApprovalRules snapshots the ordered rules for one run.
// The revision gate only adds approval when the boot policy does not ask.
// The revision gate runs before any OAuth wrapper and tool invocation.
func CompileRevisionApprovalRules(rules []config.ToolApprovalRuleConfig, gate *approval.ApprovalGate) (func(tools.ToolDescriptor) tools.ToolDescriptor, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	if gate == nil {
		return nil, fmt.Errorf("catalog: revision approval gate is required")
	}
	snapshot := slices.Clone(rules)
	policies := make([]approval.ApprovalPolicy, len(snapshot))
	for i := range snapshot {
		r := &snapshot[i]
		policy, err := resolveApprovalPolicy(&r.Approval)
		if err != nil {
			return nil, fmt.Errorf("approval rule %d: %w", i, err)
		}
		policies[i] = policy
		r.Tools = slices.Clone(r.Tools)
		r.Sources = slices.Clone(r.Sources)
		r.Annotations = slices.Clone(r.Annotations)
	}
	return func(d tools.ToolDescriptor) tools.ToolDescriptor {
		if d.ConfiguredApprovalRequired || d.Invoke == nil {
			return d
		}
		if i := firstApprovalRule(snapshot, d.Tool); i >= 0 {
			return WrapWithApproval(d, gate, ApprovalWrapperOptions{Tags: d.Tool.Tags, policy: policies[i]})
		}
		return d
	}, nil
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

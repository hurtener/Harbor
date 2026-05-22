package approval

import (
	"context"
)

// AlwaysDenyPolicy returns Required=true with a fixed Reason for every
// request — every gated call routes through the approver. Useful in
// tests; also a reasonable default for a high-security operator
// posture where every tool call goes through a human.
//
// Because policies are configured at gate construction (the §13
// amendment forbids a nil default), the operator's deliberate choice
// of AlwaysDeny is fail-safe: the worst case is "every call asks an
// approver," not "every call silently fires."
type AlwaysDenyPolicy struct {
	// Reason is the operator-facing classification carried on
	// `tool.approval_requested`. Empty defaults to "policy: deny-all".
	Reason string
}

// ShouldApprove implements ApprovalPolicy.
func (p AlwaysDenyPolicy) ShouldApprove(_ context.Context, _ *ApprovalRequest) (bool, string, error) {
	r := p.Reason
	if r == "" {
		r = "policy: deny-all"
	}
	return true, r, nil
}

// AlwaysApprovePolicy returns Required=false for every request — the
// gate short-circuits and the caller proceeds immediately, no pause,
// no bus emit.
//
// AlwaysApprove is the test-grade policy that exists for unit tests
// of the gate's "no approval needed" short-circuit path. The §13
// amendment forbids it as a production default — the binary's gate
// configuration MUST be an explicit operator choice, not a stub
// fallback. The constructor accepts AlwaysApprove only because a
// human operator can legitimately want it (a dev-loop sandbox where
// every tool is allowed); the constructor's invariant is NOT "the
// policy must require approval" but "the policy must be explicit."
type AlwaysApprovePolicy struct{}

// ShouldApprove implements ApprovalPolicy.
func (AlwaysApprovePolicy) ShouldApprove(_ context.Context, _ *ApprovalRequest) (bool, string, error) {
	return false, "", nil
}

// TaggedPolicy is the V1 production reference: approval is required
// when the request's Tags slice contains any tag in RequireTags. An
// empty RequireTags list means "approve nothing" (the gate
// short-circuits every call) — explicit operator config, not a stub.
//
// Operators reach for TaggedPolicy when their workflow is "tools
// marked `sensitive` or `write:prod` go through the approver." More
// complex flows (per-args predicates, per-identity rules) are post-V1
// and live behind a different policy type; the ApprovalPolicy
// interface is the seam.
type TaggedPolicy struct {
	Reason      string
	RequireTags []string
}

// ShouldApprove implements ApprovalPolicy.
func (p TaggedPolicy) ShouldApprove(_ context.Context, req *ApprovalRequest) (bool, string, error) {
	if req == nil {
		// Nil request is a programmer error caught upstream; defensive
		// here so a misuse does not panic.
		return false, "", nil
	}
	if len(p.RequireTags) == 0 {
		return false, "", nil
	}
	need := make(map[string]struct{}, len(p.RequireTags))
	for _, t := range p.RequireTags {
		need[t] = struct{}{}
	}
	for _, have := range req.Tags {
		if _, ok := need[have]; ok {
			r := p.Reason
			if r == "" {
				r = "policy: tagged"
			}
			return true, r, nil
		}
	}
	return false, "", nil
}

// Compile-time assertions: the bundled policies satisfy ApprovalPolicy.
var (
	_ ApprovalPolicy = AlwaysDenyPolicy{}
	_ ApprovalPolicy = AlwaysApprovePolicy{}
	_ ApprovalPolicy = TaggedPolicy{}
)

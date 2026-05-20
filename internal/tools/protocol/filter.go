package protocol

import (
	"fmt"
	"strings"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// validateFilter rejects a ToolFilter that carries an unknown enum
// value on any facet axis. An unknown value fails loud with
// ErrInvalidRequest rather than silently matching nothing — a silent
// no-match would mask a Console-side bug (CLAUDE.md §13 "fail loudly").
func validateFilter(f prototypes.ToolFilter) error {
	for _, t := range f.Transports {
		if !prototypes.IsValidToolTransport(t) {
			return fmt.Errorf("%w: unknown transport facet %q", ErrInvalidRequest, t)
		}
	}
	for _, o := range f.OAuthStatuses {
		if !prototypes.IsValidToolOAuthStatus(o) {
			return fmt.Errorf("%w: unknown oauth-status facet %q", ErrInvalidRequest, o)
		}
	}
	for _, p := range f.ApprovalPolicies {
		if !prototypes.IsValidToolApprovalPolicy(p) {
			return fmt.Errorf("%w: unknown approval-policy facet %q", ErrInvalidRequest, p)
		}
	}
	return nil
}

// filterMatches reports whether tool t passes filter f. An empty facet
// slice matches every value on that axis (the "no filter on this
// axis" semantics). The free-text Search is a case-insensitive
// substring match over the tool's Name + Version.
func filterMatches(f prototypes.ToolFilter, t prototypes.Tool) bool {
	if len(f.Scopes) > 0 && !containsString(f.Scopes, t.Scope) {
		return false
	}
	if len(f.Transports) > 0 && !containsTransport(f.Transports, t.Transport) {
		return false
	}
	if len(f.OAuthStatuses) > 0 && !containsOAuthStatus(f.OAuthStatuses, t.OAuthStatus) {
		return false
	}
	if len(f.ApprovalPolicies) > 0 && !containsApprovalPolicy(f.ApprovalPolicies, t.ApprovalPolicy) {
		return false
	}
	if len(f.ReliabilityTiers) > 0 && !containsString(f.ReliabilityTiers, t.ReliabilityTier) {
		return false
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		needle := strings.ToLower(s)
		hay := strings.ToLower(t.Name + " " + t.Version)
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	return true
}

// computeAggregates folds the filtered tool slice into the four
// catalog counters the Tools-page right-rail overview card renders.
func computeAggregates(tools []prototypes.Tool) prototypes.ToolAggregates {
	var agg prototypes.ToolAggregates
	agg.Total = int64(len(tools))
	for _, t := range tools {
		if !t.LastUsedAt.IsZero() {
			agg.Active++
		}
		if t.ApprovalPolicy == prototypes.ToolApprovalGated {
			agg.PendingApproval++
		}
		if t.OAuthStatus == prototypes.ToolOAuthRequired || t.OAuthStatus == prototypes.ToolOAuthExpired {
			agg.AwaitingOAuth++
		}
	}
	return agg
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsTransport(set []prototypes.ToolTransport, v prototypes.ToolTransport) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsOAuthStatus(set []prototypes.ToolOAuthStatus, v prototypes.ToolOAuthStatus) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func containsApprovalPolicy(set []prototypes.ToolApprovalPolicy, v prototypes.ToolApprovalPolicy) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

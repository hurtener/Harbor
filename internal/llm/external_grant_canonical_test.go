package llm

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExternalGrant_CanonicalDecodePreservesExactAuthority(t *testing.T) {
	t.Parallel()
	grant, _ := validTopUpPair()
	grant.PolicyGeneration = 9007199254740993127
	body, err := MarshalCanonicalExternalGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalCanonicalExternalGrant(body)
	if err != nil || decoded != grant {
		t.Fatalf("canonical round trip changed authority: %v", err)
	}
	// Parsing establishes byte canonicality only, never signature authority.
	for name, data := range map[string][]byte{
		"invalid":             []byte(`{"private":"PRIVATE-CONTENT",`),
		"unknown":             bytes.Replace(body, []byte(`{"version":`), []byte(`{"unknown":true,"version":`), 1),
		"duplicate":           bytes.Replace(body, []byte(`{"version":`), []byte(`{"version":1,"version":`), 1),
		"trailing object":     append(append([]byte(nil), body...), []byte(`{}`)...),
		"trailing whitespace": append(append([]byte(nil), body...), '\n'),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := UnmarshalCanonicalExternalGrant(data)
			if !errors.Is(err, ErrExternalGrantInvalid) || got != (ExternalGrant{}) {
				t.Fatalf("non-canonical authority accepted: %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE-CONTENT") {
				t.Fatal("parse error exposed payload")
			}
		})
	}
}

func TestExternalGrant_RenewalLineageRejectsRewindAndScopeChange(t *testing.T) {
	t.Parallel()
	root, descendant := validTopUpPair()
	root.Lease.ConsumedUnits = 2
	descendant.Lease.ConsumedUnits = 3
	descendant.Lease.Epoch += 3 // A lineage check permits multiple intervening renewals.
	if err := ValidateExternalGrantRenewalLineage(root, descendant); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ExternalGrant){
		"tenant":        func(g *ExternalGrant) { g.TenantID = "other" },
		"user":          func(g *ExternalGrant) { g.UserID = "other" },
		"session":       func(g *ExternalGrant) { g.SessionID = "other" },
		"run":           func(g *ExternalGrant) { g.LogicalRunID = "other" },
		"agent":         func(g *ExternalGrant) { g.AgentID = "other" },
		"route":         func(g *ExternalGrant) { g.RouteID = "other" },
		"epoch":         func(g *ExternalGrant) { g.Lease.Epoch = root.Lease.Epoch },
		"capacity":      func(g *ExternalGrant) { g.Lease.TokenUnits = root.Lease.TokenUnits - 1 },
		"consumption":   func(g *ExternalGrant) { g.Lease.ConsumedUnits = root.Lease.ConsumedUnits - 1 },
		"overdrawn":     func(g *ExternalGrant) { g.Lease.ConsumedUnits = g.Lease.TokenUnits + 1 },
		"issued":        func(g *ExternalGrant) { g.IssuedAt = root.IssuedAt.Add(-time.Second) },
		"expires":       func(g *ExternalGrant) { g.ExpiresAt = root.ExpiresAt.Add(-time.Second) },
		"lease expires": func(g *ExternalGrant) { g.Lease.ExpiresAt = root.Lease.ExpiresAt.Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := descendant
			mutate(&changed)
			if err := ValidateExternalGrantRenewalLineage(root, changed); !errors.Is(err, ErrExternalGrantInvalid) {
				t.Fatalf("invalid renewal lineage accepted: %v", err)
			}
		})
	}
}

package governance

import (
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// TestConfigFromOperator_Golden pins the tier-policy projection.
func TestConfigFromOperator_Golden(t *testing.T) {
	t.Parallel()
	got := ConfigFromOperator(config.GovernanceConfig{
		RepairAttempts: 3,
		DefaultTier:    "standard",
		IdentityTiers: map[string]config.GovernanceTierConfig{
			"standard": {
				BudgetCeilingUSD: 12.5,
				RateLimit: config.GovernanceRateLimitConfig{
					Capacity:       100,
					RefillTokens:   10,
					RefillInterval: time.Minute,
				},
				MaxTokens: 8192,
			},
			"premium": {BudgetCeilingUSD: 100},
		},
	})
	if got.DefaultTier != "standard" {
		t.Errorf("DefaultTier = %q, want standard", got.DefaultTier)
	}
	if len(got.IdentityTiers) != 2 {
		t.Fatalf("IdentityTiers len = %d, want 2", len(got.IdentityTiers))
	}
	std := got.IdentityTiers["standard"]
	if std.BudgetCeilingUSD != 12.5 || std.MaxTokens != 8192 {
		t.Errorf("standard tier = %+v", std)
	}
	wantRL := RateLimitConfig{Capacity: 100, RefillTokens: 10, RefillInterval: time.Minute}
	if std.RateLimit != wantRL {
		t.Errorf("standard RateLimit = %+v, want %+v", std.RateLimit, wantRL)
	}
	if got.IdentityTiers["premium"].BudgetCeilingUSD != 100 {
		t.Errorf("premium tier = %+v", got.IdentityTiers["premium"])
	}
	// Runtime-injection seams stay zero (wired post-projection by the
	// caller when needed).
	if got.Resolver != nil || got.Clock != nil {
		t.Error("Resolver / Clock must stay nil — they are Go-level seams, not YAML projections")
	}
}

// TestConfigFromOperator_EmptyTiersStayLatent pins the D-044 latent
// default: an empty yaml block projects to an empty (but non-nil) tier
// map and an empty DefaultTier — no enforcement.
func TestConfigFromOperator_EmptyTiersStayLatent(t *testing.T) {
	t.Parallel()
	got := ConfigFromOperator(config.GovernanceConfig{})
	if got.DefaultTier != "" {
		t.Errorf("DefaultTier = %q, want empty", got.DefaultTier)
	}
	if len(got.IdentityTiers) != 0 {
		t.Errorf("IdentityTiers = %v, want empty", got.IdentityTiers)
	}
}

// TestConfigFromOperator_FieldParity — the reflection gate (Phase
// 110c): every `config.GovernanceConfig` (and tier sub-struct) field
// is projected or explicitly excluded with a reason. Closes the
// D-155/B3 silent-field-drop class for the governance seam.
func TestConfigFromOperator_FieldParity(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, typ reflect.Type, projected map[string]bool, excluded map[string]string) {
		t.Helper()
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			_, isProjected := projected[f.Name]
			_, isExcluded := excluded[f.Name]
			switch {
			case isProjected && isExcluded:
				t.Errorf("%s.%s listed as both projected and excluded", typ.Name(), f.Name)
			case !isProjected && !isExcluded:
				t.Errorf("%s.%s is neither projected by ConfigFromOperator nor excluded with a reason — map it or exclude it explicitly (D-155/B3 class)", typ.Name(), f.Name)
			}
		}
		for name := range projected {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("projected field %s.%s no longer exists — update the parity sets", typ.Name(), name)
			}
		}
		for name := range excluded {
			if _, ok := typ.FieldByName(name); !ok {
				t.Errorf("excluded field %s.%s no longer exists — update the parity sets", typ.Name(), name)
			}
		}
	}
	t.Run("GovernanceConfig", func(t *testing.T) {
		t.Parallel()
		check(t, reflect.TypeOf(config.GovernanceConfig{}), map[string]bool{
			"DefaultTier":   true,
			"IdentityTiers": true,
		}, map[string]string{
			"RepairAttempts": "LLM repair-loop knob consumed by the retry surface, not a tier-policy field — governance.Config carries no equivalent",
		})
	})
	t.Run("GovernanceTierConfig", func(t *testing.T) {
		t.Parallel()
		check(t, reflect.TypeOf(config.GovernanceTierConfig{}), map[string]bool{
			"BudgetCeilingUSD": true,
			"RateLimit":        true,
			"MaxTokens":        true,
		}, map[string]string{})
	})
	t.Run("GovernanceRateLimitConfig", func(t *testing.T) {
		t.Parallel()
		check(t, reflect.TypeOf(config.GovernanceRateLimitConfig{}), map[string]bool{
			"Capacity":       true,
			"RefillTokens":   true,
			"RefillInterval": true,
		}, map[string]string{})
	})
}

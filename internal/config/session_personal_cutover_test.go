package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

func TestValidateSessionPersonalCutover_RefusesMalformedDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []config.SessionPersonalCutoverTenant
	}{
		{name: "empty tenant", entries: []config.SessionPersonalCutoverTenant{{Epoch: "e", RosterDigest: "d"}}},
		{name: "empty epoch", entries: []config.SessionPersonalCutoverTenant{{TenantID: "t", RosterDigest: "d"}}},
		{name: "empty digest", entries: []config.SessionPersonalCutoverTenant{{TenantID: "t", Epoch: "e"}}},
		{name: "duplicate", entries: []config.SessionPersonalCutoverTenant{{TenantID: "t", Epoch: "e", RosterDigest: "d"}, {TenantID: "t", Epoch: "e2", RosterDigest: "d2"}}},
		{name: "whitespace token", entries: []config.SessionPersonalCutoverTenant{{TenantID: "tenant ", Epoch: "e", RosterDigest: "d"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			cfg.Skills.SessionPersonalCutover.Tenants = tc.entries
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.session_personal_cutover") {
				t.Fatalf("Validate = %v, want a cutover declaration error", err)
			}
		})
	}
}

func TestValidateSessionPersonalCutover_RefusesOverBoundAndNonASCIIDeclarations(t *testing.T) {
	t.Run("over bound", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Skills.SessionPersonalCutover.Tenants = make([]config.SessionPersonalCutoverTenant, 257)
		for i := range cfg.Skills.SessionPersonalCutover.Tenants {
			cfg.Skills.SessionPersonalCutover.Tenants[i] = config.SessionPersonalCutoverTenant{
				TenantID:     fmt.Sprintf("tenant-%d", i),
				Epoch:        "epoch",
				RosterDigest: "digest",
			}
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.session_personal_cutover.tenants") {
			t.Fatalf("Validate = %v, want over-bound cutover declaration error", err)
		}
	})

	t.Run("non ASCII token", func(t *testing.T) {
		cfg := mustLoadValid(t)
		cfg.Skills.SessionPersonalCutover.Tenants = []config.SessionPersonalCutoverTenant{{
			TenantID:     "tenant",
			Epoch:        "epoch-\u00e9",
			RosterDigest: "digest",
		}}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.session_personal_cutover.tenants[0].epoch") {
			t.Fatalf("Validate = %v, want non-ASCII cutover declaration error", err)
		}
	})
}

func TestValidateSessionPersonalCutover_TenantIDsAreCaseSensitive(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Skills.Driver = "localdb"
	cfg.Skills.DSN = ":memory:"
	cfg.Skills.SessionPersonalCutover.Tenants = []config.SessionPersonalCutoverTenant{
		{TenantID: "TenantA", Epoch: "e1", RosterDigest: "d1"},
		{TenantID: "tenanta", Epoch: "e2", RosterDigest: "d2"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("case-distinct opaque tenant declarations must not alias: %v", err)
	}
}

func TestValidateSessionPersonalCutover_AcceptsUndrainedStaticDeclaration(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Skills.Driver = "localdb"
	cfg.Skills.DSN = ":memory:"
	cfg.Skills.SessionPersonalCutover.Tenants = []config.SessionPersonalCutoverTenant{{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: false}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateSessionPersonalCutover_RequiresSkillStore(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Skills = config.SkillsConfig{SessionPersonalCutover: config.SessionPersonalCutoverConfig{Tenants: []config.SessionPersonalCutoverTenant{{
		TenantID:     "tenant",
		Epoch:        "epoch",
		RosterDigest: "digest",
	}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "skills.driver") {
		t.Fatalf("Validate = %v, want skills.driver error for an unwired cutover", err)
	}
}

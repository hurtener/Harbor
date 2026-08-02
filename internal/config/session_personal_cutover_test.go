package config_test

import (
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
		{name: "case canonical duplicate", entries: []config.SessionPersonalCutoverTenant{{TenantID: "Tenant", Epoch: "e", RosterDigest: "d"}, {TenantID: "tenant", Epoch: "e2", RosterDigest: "d2"}}},
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

func TestValidateSessionPersonalCutover_AcceptsUndrainedStaticDeclaration(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.Skills.SessionPersonalCutover.Tenants = []config.SessionPersonalCutoverTenant{{TenantID: "tenant", Epoch: "epoch", RosterDigest: "digest", LegacyWritersDrained: false}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

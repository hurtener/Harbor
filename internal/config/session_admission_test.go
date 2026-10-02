package config_test

import (
	"strings"
	"testing"
)

func TestSessionAdmission_ExplicitFleetPrerequisites(t *testing.T) {
	for _, test := range []struct {
		name, audience string
		drained        bool
		want           string
	}{
		{"disabled", "", false, ""},
		{"unacknowledged", "harbor-scoped", false, "identity.session_admission_legacy_writers_drained"},
		{"enabled", "harbor-scoped", true, ""},
		{"whitespace", " scoped ", true, "identity.scoped_token_audience"},
		{"internal whitespace", "scoped audience", true, "identity.scoped_token_audience"},
		{"over limit", strings.Repeat("s", 2049), true, "identity.scoped_token_audience"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := mustLoadValid(t)
			cfg.State.Driver = "sqlite"
			cfg.State.DSN = "admission.sqlite"
			cfg.Identity.ScopedTokenAudience = test.audience
			cfg.Identity.SessionAdmissionLegacyWritersDrained = test.drained
			err := cfg.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation=%v want %s", err, test.want)
			}
		})
	}
	cfg := mustLoadValid(t)
	cfg.State.Driver = "sqlite"
	cfg.State.DSN = "admission.sqlite"
	cfg.Identity.ScopedTokenAudience = cfg.Identity.Audience
	cfg.Identity.SessionAdmissionLegacyWritersDrained = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "identity.scoped_token_audience") {
		t.Fatalf("accepted overlapping audiences: %v", err)
	}
}

func TestSessionAdmission_RejectsVolatileProductionState(t *testing.T) {
	cfg := mustLoadValid(t)
	cfg.State.Driver = "inmem"
	cfg.Identity.ScopedTokenAudience = "harbor-scoped"
	cfg.Identity.SessionAdmissionLegacyWritersDrained = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "persistent sqlite or postgres") {
		t.Fatalf("volatile admission was accepted: %v", err)
	}
}

package projection_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

// TestActiveNamingPolicy_Precedence proves the D-289 resolution: agent-config
// naming section (when present) over the yaml default over off, with defaults
// applied, and the opt-in invariant (config-free = off).
func TestActiveNamingPolicy_Precedence(t *testing.T) {
	ctx := context.Background()

	t.Run("off_when_no_config", func(t *testing.T) {
		reg := newRegistry(t)
		res, active, err := projection.ActiveNamingPolicy(ctx, reg, projAgent, projID(), config.RuntimeNamingConfig{})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if active {
			t.Errorf("config-free resolution reported active=%v, want off; res=%+v", active, res)
		}
	})

	t.Run("yaml_default_when_no_section", func(t *testing.T) {
		reg := newRegistry(t)
		yaml := config.RuntimeNamingConfig{Auto: true, AfterTurns: 2, MaxTitleLen: 120}
		res, active, err := projection.ActiveNamingPolicy(ctx, reg, projAgent, projID(), yaml)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !active {
			t.Fatal("yaml auto=true should resolve active")
		}
		if res.Policy.AfterTurns != 2 || res.Policy.MaxTitleLen != 120 {
			t.Errorf("yaml policy = %+v, want after=2 maxlen=120", res.Policy)
		}
	})

	t.Run("agentcfg_wins_over_yaml", func(t *testing.T) {
		reg := newRegistry(t)
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Naming: &agentcfg.NamingSection{Auto: true, AfterTurns: 5, Model: "agent-model"},
		}); err != nil {
			t.Fatalf("SetRevision: %v", err)
		}
		yaml := config.RuntimeNamingConfig{Auto: true, AfterTurns: 2}
		res, active, err := projection.ActiveNamingPolicy(ctx, reg, projAgent, projID(), yaml)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !active {
			t.Fatal("agentcfg section auto=true should be active")
		}
		if res.Policy.AfterTurns != 5 || res.Model != "agent-model" {
			t.Errorf("agentcfg policy = %+v model=%q, want after=5 model=agent-model", res.Policy, res.Model)
		}
		// Defaults applied: MaxTitleLen unset in the section resolves to 80.
		if res.Policy.MaxTitleLen != 80 {
			t.Errorf("MaxTitleLen = %d, want defaulted 80", res.Policy.MaxTitleLen)
		}
	})

	t.Run("agentcfg_bare_auto_false_overrides_yaml_on", func(t *testing.T) {
		reg := newRegistry(t)
		// THE M1 footgun regression: a BARE `{auto: false}` section — no other
		// field set — is an explicit per-agent opt-out. Normalization preserves
		// it (section presence is the signal), so it WINS over a yaml default
		// that is on. Before the fix the section was dropped as "inert" and the
		// agent silently kept auto-naming.
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Naming: &agentcfg.NamingSection{Auto: false},
		}); err != nil {
			t.Fatalf("SetRevision: %v", err)
		}
		yaml := config.RuntimeNamingConfig{Auto: true, AfterTurns: 1}
		_, active, err := projection.ActiveNamingPolicy(ctx, reg, projAgent, projID(), yaml)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if active {
			t.Error("a bare agentcfg auto:false section must override yaml auto=true → off")
		}
	})
}

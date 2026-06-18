package config_test

// Phase 86 (D-229) — the distributed.bus_poll_interval validation: the
// durable bus driver's optional poll-cadence knob. Zero uses the
// driver default; a negative value is a misconfiguration and is
// rejected loudly.

import (
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

func TestValidateDistributed_BusPollInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string // "" = valid
	}{
		{"unset is valid (driver default)", func(c *config.Config) {
			c.Distributed.BusPollInterval = 0
		}, ""},
		{"positive is valid", func(c *config.Config) {
			c.Distributed.BusPollInterval = 2 * time.Second
		}, ""},
		{"durable driver is accepted", func(c *config.Config) {
			c.Distributed.BusDriver = "durable"
		}, ""},
		{"negative is rejected", func(c *config.Config) {
			c.Distributed.BusPollInterval = -time.Second
		}, "distributed.bus_poll_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadValid(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}

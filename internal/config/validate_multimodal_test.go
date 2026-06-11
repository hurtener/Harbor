package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/planner"
)

// TestValidateMultimodal_Disposition exercises the Phase 84b (D-189)
// `multimodal.disposition` validator: key grammar (`*` / `type/*` /
// `type/subtype`), value grammar (`ref` / `inline` /
// `provider_native` / `tool:<name>`), and the empty-block default.
func TestValidateMultimodal_Disposition(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		block   map[string]string
		wantErr string // "" = valid
	}{
		{"empty block valid", nil, ""},
		{"exact mime + ref", map[string]string{"application/pdf": "ref"}, ""},
		{"family wildcard + inline", map[string]string{"image/*": "inline"}, ""},
		{"star default + provider_native", map[string]string{"*": "provider_native"}, ""},
		{"tool value", map[string]string{"text/csv": "tool:csv.summarise"}, ""},
		{"bad key no slash", map[string]string{"pdf": "ref"}, "multimodal.disposition"},
		{"bad key empty subtype", map[string]string{"application/": "ref"}, "multimodal.disposition"},
		{"bad key empty type", map[string]string{"/pdf": "ref"}, "multimodal.disposition"},
		{"bad key double slash", map[string]string{"a/b/c": "ref"}, "multimodal.disposition"},
		{"bad value", map[string]string{"application/pdf": "native"}, "multimodal.disposition"},
		{"bad value bare tool prefix", map[string]string{"application/pdf": "tool:"}, "multimodal.disposition"},
		{"bad value uppercase", map[string]string{"application/pdf": "REF"}, "multimodal.disposition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoadValid(t)
			cfg.Multimodal = config.MultimodalConfig{Disposition: tc.block}
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

// TestValidateMultimodal_GrammarLockstepWithPlanner pins the
// config-side disposition-value grammar (a local mirror — the D-193
// import direction forbids config → planner) against the
// planner-side ParseDisposition: every value the validator accepts
// must parse, and every value it rejects must not.
func TestValidateMultimodal_GrammarLockstepWithPlanner(t *testing.T) {
	t.Parallel()
	values := []string{
		"ref", "inline", "provider_native", "tool:pdf.extract", "tool:x",
		"", "REF", "tool:", "Inline", "provider-native", "ref ",
	}
	for _, v := range values {
		cfg := mustLoadValid(t)
		cfg.Multimodal = config.MultimodalConfig{Disposition: map[string]string{"text/csv": v}}
		cfgErr := cfg.Validate()
		_, parseErr := planner.ParseDisposition(v)
		if (cfgErr == nil) != (parseErr == nil) {
			t.Fatalf("grammar drift on %q: config validate err=%v, planner parse err=%v", v, cfgErr, parseErr)
		}
	}
}

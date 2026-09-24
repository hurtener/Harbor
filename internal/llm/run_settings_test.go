package llm_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestRunSettings_AdmissionDoesNotInventAnOutputCeiling(t *testing.T) {
	t.Parallel()
	ptrString := func(v string) *string { return &v }
	ptrInt := func(v int) *int { return &v }
	for _, tc := range []struct {
		name      string
		settings  *llm.RunSettings
		route     *llm.ProviderRoute
		wantField string
	}{
		{name: "nil"},
		{name: "empty", settings: &llm.RunSettings{}},
		{name: "large output", settings: &llm.RunSettings{MaxTokens: ptrInt(128000)}},
		{name: "larger output", settings: &llm.RunSettings{MaxTokens: ptrInt(1000000)}},
		{name: "maximum model name", settings: &llm.RunSettings{Model: ptrString(strings.Repeat("m", 512))}},
		{name: "routed output override", settings: &llm.RunSettings{MaxTokens: ptrInt(128000), ReasoningEffort: ptrString("high")}, route: &llm.ProviderRoute{}},
		{name: "ambiguous model", settings: &llm.RunSettings{Model: ptrString("model")}, route: &llm.ProviderRoute{}, wantField: "llm_settings.model"},
		{name: "empty model", settings: &llm.RunSettings{Model: ptrString("")}, wantField: "llm_settings.model"},
		{name: "padded model", settings: &llm.RunSettings{Model: ptrString(" model ")}, wantField: "llm_settings.model"},
		{name: "oversized name", settings: &llm.RunSettings{Model: ptrString(strings.Repeat("m", 513))}, wantField: "llm_settings.model"},
		{name: "unknown reasoning", settings: &llm.RunSettings{ReasoningEffort: ptrString("unsupported")}, wantField: "llm_settings.reasoning_effort"},
		{name: "zero output", settings: &llm.RunSettings{MaxTokens: ptrInt(0)}, wantField: "llm_settings.max_tokens"},
		{name: "negative output", settings: &llm.RunSettings{MaxTokens: ptrInt(-1)}, wantField: "llm_settings.max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := llm.ValidateRunSettings(tc.settings, tc.route)
			if tc.wantField == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("error=%v, want field %s", err, tc.wantField)
			}
		})
	}
	// This syntax gate does not authorize capacity. Governed model admission
	// separately checks the selected model's real input/output allowance.
	for _, effort := range []string{"", "off", "low", "medium", "high"} {
		if err := llm.ValidateRunSettings(&llm.RunSettings{ReasoningEffort: &effort}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunSettings_CloneDetachesEveryOverride(t *testing.T) {
	t.Parallel()
	if llm.CloneRunSettings(nil) != nil {
		t.Fatal("nil settings became explicit settings")
	}
	empty := &llm.RunSettings{}
	if cloned := llm.CloneRunSettings(empty); cloned == empty || !reflect.DeepEqual(cloned, empty) {
		t.Fatal("empty settings changed shape or retained caller ownership")
	}
	model, effort, tokens := "model", "high", 128000
	source := &llm.RunSettings{Model: &model, ReasoningEffort: &effort, MaxTokens: &tokens}
	clone := llm.CloneRunSettings(source)
	if !reflect.DeepEqual(source, clone) || source.Model == clone.Model || source.ReasoningEffort == clone.ReasoningEffort || source.MaxTokens == clone.MaxTokens {
		t.Fatal("clone lost settings or retained caller-owned pointers")
	}
	model, effort, tokens = "other", "off", 1
	if *clone.Model != "model" || *clone.ReasoningEffort != "high" || *clone.MaxTokens != 128000 {
		t.Fatal("caller mutation changed admitted settings")
	}
	*clone.Model, *clone.ReasoningEffort, *clone.MaxTokens = "clone", "low", 2
	if model != "other" || effort != "off" || tokens != 1 {
		t.Fatal("clone mutation changed caller settings")
	}
}

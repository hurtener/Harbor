package llm

import (
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
)

// TestSnapshotFromConfig_GoldenFullProjection pins the full config →
// snapshot projection, including the D-155 fields (CustomProviders /
// NetworkDefaults / Corrections) and the 110c cross-fix fields
// (per-profile CostOverrides + Corrections that the absorbed
// cmd/devstack copyModelProfiles helpers silently dropped).
func TestSnapshotFromConfig_GoldenFullProjection(t *testing.T) {
	t.Parallel()
	enabled := false
	maxTok := 4096
	cfg := config.LLMConfig{
		Driver:               "bifrost",
		Provider:             "openrouter",
		Model:                "anthropic/claude-sonnet-4",
		APIKey:               "env.FAKE_TEST_KEY", // documented dummy fixture value, not a secret
		BaseURL:              "https://example.invalid/v1",
		Timeout:              42 * time.Second,
		ContextWindowReserve: 0.07,
		ModelProfiles: map[string]config.LLMModelProfileConfig{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
				JSONSchemaMode:      "native",
				DefaultMaxTokens:    &maxTok,
				ReasoningEffort:     "medium",
				MaxRetries:          2,
				CostOverrides: &config.LLMCostOverridesConfig{
					InputPer1M:     3.0,
					OutputPer1M:    15.0,
					ReasoningPer1M: 15.0,
					Currency:       "USD",
				},
				Corrections: &config.LLMCorrectionsProfileConfig{
					MessageOrdering:        "system_first_strict",
					SchemaMode:             "openai_strict",
					ReasoningEffortRouting: "thinking_model",
					ResponseFormatShape:    "anthropic",
					UsageBackfillEnabled:   true,
				},
			},
		},
		Corrections: config.LLMCorrectionsConfig{Enabled: &enabled},
		CustomProviders: []config.LLMCustomProviderConfig{
			{
				Name:                 "nim",
				BaseURL:              "https://nim.example.invalid/v1",
				APIKeyEnvVar:         "NIM_KEY",
				Models:               []string{"meta/llama-3.3-70b"},
				BaseProviderType:     "openai",
				Timeout:              30 * time.Second,
				MaxRetries:           3,
				RetryBackoffInitial:  100 * time.Millisecond,
				RetryBackoffMax:      5 * time.Second,
				Concurrency:          8,
				BufferSize:           64,
				RequestPathOverrides: map[string]string{"chat": "/chat/completions"},
			},
		},
		NetworkDefaults: config.LLMNetworkDefaults{
			Timeout:             20 * time.Second,
			MaxRetries:          1,
			RetryBackoffInitial: 50 * time.Millisecond,
			RetryBackoffMax:     2 * time.Second,
			Concurrency:         4,
			BufferSize:          32,
		},
	}
	art := config.ArtifactsConfig{HeavyOutputThresholdBytes: 64 * 1024}

	snap := SnapshotFromConfig(cfg, art)

	if snap.Driver != "bifrost" || snap.Provider != "openrouter" ||
		snap.Model != "anthropic/claude-sonnet-4" || snap.APIKey != "env.FAKE_TEST_KEY" ||
		snap.BaseURL != "https://example.invalid/v1" || snap.Timeout != 42*time.Second {
		t.Errorf("scalar knobs not carried verbatim: %+v", snap)
	}
	if snap.ContextWindowReserve != 0.07 {
		t.Errorf("ContextWindowReserve = %g, want 0.07", snap.ContextWindowReserve)
	}
	if snap.HeavyOutputThreshold != 64*1024 {
		t.Errorf("HeavyOutputThreshold = %d, want %d", snap.HeavyOutputThreshold, 64*1024)
	}
	if !snap.DisableCorrections {
		t.Error("DisableCorrections = false, want true (operator set corrections.enabled: false)")
	}
	if snap.DisableDowngrade || snap.DisableRetry || snap.DisableGovernance {
		t.Error("Disable{Downgrade,Retry,Governance} must stay zero (no config knob exists)")
	}

	prof, ok := snap.ModelProfiles["anthropic/claude-sonnet-4"]
	if !ok {
		t.Fatal("model profile not projected")
	}
	if prof.ContextWindowTokens != 200000 || prof.TokenEstimator != "chars_div_4" ||
		prof.JSONSchemaMode != "native" || prof.ReasoningEffort != ReasoningEffort("medium") ||
		prof.MaxRetries != 2 {
		t.Errorf("profile scalars not carried: %+v", prof)
	}
	if prof.DefaultMaxTokens == nil || *prof.DefaultMaxTokens != 4096 {
		t.Errorf("DefaultMaxTokens = %v, want ptr to 4096", prof.DefaultMaxTokens)
	}
	// 110c cross-fix: CostOverrides + Corrections must survive the
	// projection (they were silently dropped pre-110c).
	if prof.CostOverrides == nil {
		t.Fatal("CostOverrides dropped — the pre-110c silent-field-drop bug regressed")
	}
	if prof.CostOverrides.InputPer1M != 3.0 || prof.CostOverrides.OutputPer1M != 15.0 ||
		prof.CostOverrides.ReasoningPer1M != 15.0 || prof.CostOverrides.Currency != "USD" {
		t.Errorf("CostOverrides = %+v, want 3/15/15 USD", prof.CostOverrides)
	}
	wantCorr := CorrectionsProfile{
		MessageOrdering:        OrderingSystemFirstStrict,
		SchemaMode:             SchemaOpenAIStrict,
		ReasoningEffortRouting: ReasoningRouteThinking,
		ResponseFormatShape:    ResponseFormatAnthropic,
		UsageBackfillEnabled:   true,
	}
	if prof.Corrections != wantCorr {
		t.Errorf("Corrections = %+v, want %+v — the pre-110c silent-field-drop bug regressed", prof.Corrections, wantCorr)
	}

	if len(snap.CustomProviders) != 1 {
		t.Fatalf("CustomProviders len = %d, want 1 (D-155 regression)", len(snap.CustomProviders))
	}
	cp := snap.CustomProviders[0]
	if cp.Name != "nim" || cp.BaseURL != "https://nim.example.invalid/v1" ||
		cp.APIKeyEnvVar != "NIM_KEY" || cp.BaseProviderType != "openai" ||
		cp.Timeout != 30*time.Second || cp.MaxRetries != 3 ||
		cp.RetryBackoffInitial != 100*time.Millisecond || cp.RetryBackoffMax != 5*time.Second ||
		cp.Concurrency != 8 || cp.BufferSize != 64 {
		t.Errorf("custom provider not carried verbatim: %+v", cp)
	}
	if len(cp.Models) != 1 || cp.Models[0] != "meta/llama-3.3-70b" {
		t.Errorf("Models = %v", cp.Models)
	}
	if cp.RequestPathOverrides["chat"] != "/chat/completions" {
		t.Errorf("RequestPathOverrides = %v", cp.RequestPathOverrides)
	}
	wantNet := NetworkDefaults{
		Timeout:             20 * time.Second,
		MaxRetries:          1,
		RetryBackoffInitial: 50 * time.Millisecond,
		RetryBackoffMax:     2 * time.Second,
		Concurrency:         4,
		BufferSize:          32,
	}
	if snap.NetworkDefaults != wantNet {
		t.Errorf("NetworkDefaults = %+v, want %+v (D-155 regression)", snap.NetworkDefaults, wantNet)
	}
}

// TestSnapshotFromConfig_CorrectionsEnabledDefaults pins the tri-state
// `corrections.enabled` resolution: nil → enabled (DisableCorrections
// false), explicit true → enabled, explicit false → disabled.
func TestSnapshotFromConfig_CorrectionsEnabledDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		enabled *bool
		want    bool // DisableCorrections
	}{
		{"nil means enabled", nil, false},
		{"explicit true means enabled", boolPtrForTest(true), false},
		{"explicit false means disabled", boolPtrForTest(false), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := SnapshotFromConfig(config.LLMConfig{
				Corrections: config.LLMCorrectionsConfig{Enabled: tc.enabled},
			}, config.ArtifactsConfig{})
			if snap.DisableCorrections != tc.want {
				t.Errorf("DisableCorrections = %v, want %v", snap.DisableCorrections, tc.want)
			}
		})
	}
}

// TestSnapshotFromConfig_DeepCopyIsolation asserts the snapshot is a
// deep copy — mutating the snapshot's maps / slices / pointers never
// reaches back into the caller's config (and vice versa). The `harbor
// dev` mock override mutates the returned snapshot in place; this pins
// that the override stays local.
func TestSnapshotFromConfig_DeepCopyIsolation(t *testing.T) {
	t.Parallel()
	maxTok := 100
	cfg := config.LLMConfig{
		ModelProfiles: map[string]config.LLMModelProfileConfig{
			"m": {ContextWindowTokens: 1000, DefaultMaxTokens: &maxTok},
		},
		CustomProviders: []config.LLMCustomProviderConfig{
			{Name: "p", Models: []string{"a"}, RequestPathOverrides: map[string]string{"k": "v"}},
			{Name: "q"}, // no overrides: the nil-for-empty map branch
		},
	}
	snap := SnapshotFromConfig(cfg, config.ArtifactsConfig{})
	if snap.CustomProviders[1].RequestPathOverrides != nil {
		t.Error("empty RequestPathOverrides must project to nil")
	}

	prof := snap.ModelProfiles["m"]
	*prof.DefaultMaxTokens = 999
	if maxTok != 100 {
		t.Error("mutating the snapshot's DefaultMaxTokens reached the caller's config")
	}
	snap.CustomProviders[0].Models[0] = "mutated"
	if cfg.CustomProviders[0].Models[0] != "a" {
		t.Error("mutating the snapshot's Models slice reached the caller's config")
	}
	snap.CustomProviders[0].RequestPathOverrides["k"] = "mutated"
	if cfg.CustomProviders[0].RequestPathOverrides["k"] != "v" {
		t.Error("mutating the snapshot's RequestPathOverrides map reached the caller's config")
	}
}

// TestSnapshotFromConfig_EmptyCollectionsStayNil pins the nil-for-empty
// convention the absorbed helpers carried: empty config maps / slices
// project to nil, not empty allocations.
func TestSnapshotFromConfig_EmptyCollectionsStayNil(t *testing.T) {
	t.Parallel()
	snap := SnapshotFromConfig(config.LLMConfig{}, config.ArtifactsConfig{})
	if snap.ModelProfiles != nil {
		t.Errorf("ModelProfiles = %v, want nil", snap.ModelProfiles)
	}
	if snap.CustomProviders != nil {
		t.Errorf("CustomProviders = %v, want nil", snap.CustomProviders)
	}
}

// TestSnapshotFromConfig_FieldParity_LLMConfig is the reflection gate
// (Phase 110c acceptance): every field of `config.LLMConfig` is either
// projected by SnapshotFromConfig or named in the exclusion map with a
// reason. A new config field without a projection (or an explicit
// exclusion) fails this test — the mechanical closure of the D-155 /
// B3 silent-field-drop class.
func TestSnapshotFromConfig_FieldParity_LLMConfig(t *testing.T) {
	t.Parallel()
	projected := map[string]bool{
		"Driver":               true,
		"Provider":             true,
		"Model":                true,
		"APIKey":               true,
		"BaseURL":              true,
		"Timeout":              true,
		"ContextWindowReserve": true,
		"ModelProfiles":        true,
		"Corrections":          true, // → DisableCorrections (inverse bool)
		"CustomProviders":      true,
		"NetworkDefaults":      true,
		"CredentialSource":     true, // → the brokered-XOR-local primary selector
		"InferenceBroker":      true, // → the named broker the primary key is pulled from
	}
	// InferenceBrokers is boundary-wired, not a snapshot field: the boot
	// builds the InferenceKeySource over the shared LiveKey and connects it,
	// rather than handing the broker LIST to the LLM client snapshot.
	excluded := map[string]string{
		"InferenceBrokers": "boundary-wired: the boot builds the broker-pull InferenceKeySource; the LLM client reads the seeded LiveKey, not the broker list",
		"ExternalGrant":    "assembly-wired: the runtime constructs the verifier, credential resolver, reservation manager, and receipt outbox around the LLM client; it is not part of the provider snapshot",
	}
	assertFieldParity(t, reflect.TypeOf(config.LLMConfig{}), projected, excluded)
}

// TestSnapshotFromConfig_FieldParity_SubStructs extends the gate to the
// projected sub-structs, where the pre-110c drop actually lived
// (LLMModelProfileConfig.CostOverrides / .Corrections).
func TestSnapshotFromConfig_FieldParity_SubStructs(t *testing.T) {
	t.Parallel()
	t.Run("LLMModelProfileConfig", func(t *testing.T) {
		t.Parallel()
		assertFieldParity(t, reflect.TypeOf(config.LLMModelProfileConfig{}), map[string]bool{
			"ContextWindowTokens": true,
			"TokenEstimator":      true,
			"JSONSchemaMode":      true,
			"DefaultMaxTokens":    true,
			"ReasoningEffort":     true,
			"CostOverrides":       true,
			"Corrections":         true,
			"MaxRetries":          true,
		}, map[string]string{})
	})
	t.Run("LLMCustomProviderConfig", func(t *testing.T) {
		t.Parallel()
		assertFieldParity(t, reflect.TypeOf(config.LLMCustomProviderConfig{}), map[string]bool{
			"Name":                 true,
			"BaseURL":              true,
			"APIKeyEnvVar":         true,
			"Models":               true,
			"BaseProviderType":     true,
			"Timeout":              true,
			"MaxRetries":           true,
			"RetryBackoffInitial":  true,
			"RetryBackoffMax":      true,
			"Concurrency":          true,
			"BufferSize":           true,
			"RequestPathOverrides": true,
		}, map[string]string{})
	})
	t.Run("LLMNetworkDefaults", func(t *testing.T) {
		t.Parallel()
		assertFieldParity(t, reflect.TypeOf(config.LLMNetworkDefaults{}), map[string]bool{
			"Timeout":             true,
			"MaxRetries":          true,
			"RetryBackoffInitial": true,
			"RetryBackoffMax":     true,
			"Concurrency":         true,
			"BufferSize":          true,
		}, map[string]string{})
	})
	t.Run("LLMCorrectionsProfileConfig", func(t *testing.T) {
		t.Parallel()
		assertFieldParity(t, reflect.TypeOf(config.LLMCorrectionsProfileConfig{}), map[string]bool{
			"MessageOrdering":        true,
			"SchemaMode":             true,
			"ReasoningEffortRouting": true,
			"ResponseFormatShape":    true,
			"UsageBackfillEnabled":   true,
		}, map[string]string{})
	})
	t.Run("LLMCostOverridesConfig", func(t *testing.T) {
		t.Parallel()
		assertFieldParity(t, reflect.TypeOf(config.LLMCostOverridesConfig{}), map[string]bool{
			"InputPer1M":     true,
			"OutputPer1M":    true,
			"ReasoningPer1M": true,
			"Currency":       true,
		}, map[string]string{})
	})
}

// assertFieldParity walks every exported field of typ and fails unless
// it is named in exactly one of projected / excluded. Shared by the
// per-struct parity gates above.
func assertFieldParity(t *testing.T, typ reflect.Type, projected map[string]bool, excluded map[string]string) {
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
			t.Errorf("%s.%s listed as both projected and excluded — pick one", typ.Name(), f.Name)
		case !isProjected && !isExcluded:
			t.Errorf("%s.%s is neither projected by the FromConfig projection nor excluded with a reason — the D-155/B3 silent-field-drop class; map it or exclude it explicitly", typ.Name(), f.Name)
		}
	}
	for name := range projected {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("projected field %s.%s no longer exists on the config struct — update the parity sets", typ.Name(), name)
		}
	}
	for name := range excluded {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("excluded field %s.%s no longer exists on the config struct — update the parity sets", typ.Name(), name)
		}
	}
}

func boolPtrForTest(b bool) *bool { return &b }

// from_config.go — the exported config→snapshot projection (Phase 110c,
// D-196).
//
// Before 110c the `config.LLMConfig` → `llm.ConfigSnapshot` projection
// lived as unexported `package main` helpers in `cmd/harbor/cmd_dev.go`
// (`copyModelProfiles` / `copyCustomProviders` / `copyNetworkDefaults` /
// `disableCorrectionsFromConfig`), hand-duplicated in
// `harbortest/devstack`. That mechanism shipped the D-155 silent-field-
// drop bug (CustomProviders / NetworkDefaults / Corrections dropped from
// the snapshot) and a second live drop found by the same parity gate
// this file ships (per-model `cost_overrides:` + `corrections:` dropped
// by both copies of copyModelProfiles). The projection now lives next to
// the snapshot type it fills, so a new snapshot field and its projection
// land in the same package — and the field-parity test in
// from_config_test.go fails the build when they don't.
//
// Import direction (settled by the Wave B re-homing program, D-193):
// the subsystem imports `internal/config` ADDITIVELY. `internal/config`
// stays a leaf with no subsystem imports; `SnapshotFromConfig` is one
// optional helper on the side, never a required path — `Open(ctx,
// snapshot, deps)` is unchanged and snapshot-first construction remains
// the headless golden path.

package llm

import "github.com/hurtener/Harbor/internal/config"

// SnapshotFromConfig projects the operator-facing `config.LLMConfig`
// block (plus the one artifacts-config field the LLM edge consumes)
// onto the llm package's decoupled ConfigSnapshot. Every config-sourced
// snapshot field is populated here — cmd/harbor, harbortest/devstack,
// and headless embedders all call this ONE projection (closing the
// D-155 / audit-B3 silent-field-drop class).
//
// `art` supplies `HeavyOutputThresholdBytes` → `HeavyOutputThreshold`
// (the snapshot mirrors it so the llm package does not re-import the
// artifact-config struct; see the ConfigSnapshot godoc).
//
// The returned snapshot is a deep copy: mutating it (e.g. the
// `HARBOR_DEV_ALLOW_MOCK=1` driver override in `harbor dev`) never
// reaches back into the caller's *config.Config.
//
// Fields NOT populated here, with reasons:
//   - `DisableDowngrade` / `DisableRetry` / `DisableGovernance` — no
//     operator-facing config knob exists for them (test-only opt-outs);
//     the zero value (enabled) is the production default.
//   - `ModelProfile.OutputMode` — llm-internal normalisation target;
//     `applyDefaults` resolves it from `JSONSchemaMode` + the
//     registered per-provider resolver at Open time.
func SnapshotFromConfig(cfg config.LLMConfig, art config.ArtifactsConfig) ConfigSnapshot {
	return ConfigSnapshot{
		Driver:               cfg.Driver,
		Provider:             cfg.Provider,
		Model:                cfg.Model,
		APIKey:               cfg.APIKey,
		BaseURL:              cfg.BaseURL,
		Timeout:              cfg.Timeout,
		ContextWindowReserve: cfg.ContextWindowReserve,
		HeavyOutputThreshold: art.HeavyOutputThresholdBytes,
		ModelProfiles:        modelProfilesFromConfig(cfg.ModelProfiles),
		// Phase 83l / D-155 — these three were the original production
		// field-drop bug: an operator-declared custom provider was
		// silently ignored at boot ("declared custom: (none)").
		CustomProviders:    customProvidersFromConfig(cfg.CustomProviders),
		NetworkDefaults:    networkDefaultsFromConfig(cfg.NetworkDefaults),
		DisableCorrections: disableCorrectionsFromConfig(cfg.Corrections),
	}
}

// modelProfilesFromConfig converts the config-package map shape into
// the llm-package ModelProfile map. Each profile field is copied by
// value — both packages own their own struct types so a copy keeps the
// seam decoupled.
//
// Phase 110c §17.6 cross-fix: the absorbed cmd/devstack
// `copyModelProfiles` helpers silently dropped `CostOverrides` and
// `Corrections` — an operator's per-model `cost_overrides:` /
// `corrections:` yaml validated cleanly and then did nothing (the
// corrections layer read a zero-valued CorrectionsProfile; the cost
// accumulator never saw the override table). The field-parity test
// that ships with this projection surfaced the drop; both fields are
// now mapped.
func modelProfilesFromConfig(in map[string]config.LLMModelProfileConfig) map[string]ModelProfile {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ModelProfile, len(in))
	for name, p := range in {
		mp := ModelProfile{
			ContextWindowTokens: p.ContextWindowTokens,
			TokenEstimator:      p.TokenEstimator,
			JSONSchemaMode:      p.JSONSchemaMode,
			ReasoningEffort:     ReasoningEffort(p.ReasoningEffort),
			MaxRetries:          p.MaxRetries,
		}
		if p.DefaultMaxTokens != nil {
			v := *p.DefaultMaxTokens
			mp.DefaultMaxTokens = &v
		}
		if p.CostOverrides != nil {
			mp.CostOverrides = &CostTable{
				InputPer1M:     p.CostOverrides.InputPer1M,
				OutputPer1M:    p.CostOverrides.OutputPer1M,
				ReasoningPer1M: p.CostOverrides.ReasoningPer1M,
				Currency:       p.CostOverrides.Currency,
			}
		}
		if p.Corrections != nil {
			// String-coded enums: yaml values are validated against the
			// same literals the llm constants carry
			// (`internal/config/validate.go::validateCorrectionsProfile`),
			// so a direct cast is the whole translation.
			mp.Corrections = CorrectionsProfile{
				MessageOrdering:        MessageOrderingPolicy(p.Corrections.MessageOrdering),
				SchemaMode:             SchemaSanitizationMode(p.Corrections.SchemaMode),
				ReasoningEffortRouting: ReasoningRouting(p.Corrections.ReasoningEffortRouting),
				ResponseFormatShape:    ResponseFormatProfile(p.Corrections.ResponseFormatShape),
				UsageBackfillEnabled:   p.Corrections.UsageBackfillEnabled,
			}
		}
		out[name] = mp
	}
	return out
}

// customProvidersFromConfig projects `config.LLMCustomProviderConfig`
// entries onto the `llm.CustomProviderSpec` shape the bifrost driver
// reads (Phase 33a; D-155 fix lineage).
func customProvidersFromConfig(in []config.LLMCustomProviderConfig) []CustomProviderSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]CustomProviderSpec, 0, len(in))
	for _, p := range in {
		out = append(out, CustomProviderSpec{
			Name:                 p.Name,
			BaseURL:              p.BaseURL,
			APIKeyEnvVar:         p.APIKeyEnvVar,
			Models:               append([]string(nil), p.Models...),
			BaseProviderType:     p.BaseProviderType,
			Timeout:              p.Timeout,
			MaxRetries:           p.MaxRetries,
			RetryBackoffInitial:  p.RetryBackoffInitial,
			RetryBackoffMax:      p.RetryBackoffMax,
			Concurrency:          p.Concurrency,
			BufferSize:           p.BufferSize,
			RequestPathOverrides: cloneStringValueMap(p.RequestPathOverrides),
		})
	}
	return out
}

// networkDefaultsFromConfig projects the operator's `network_defaults`
// block onto the snapshot (Phase 33a; D-155 fix lineage).
func networkDefaultsFromConfig(in config.LLMNetworkDefaults) NetworkDefaults {
	return NetworkDefaults{
		Timeout:             in.Timeout,
		MaxRetries:          in.MaxRetries,
		RetryBackoffInitial: in.RetryBackoffInitial,
		RetryBackoffMax:     in.RetryBackoffMax,
		Concurrency:         in.Concurrency,
		BufferSize:          in.BufferSize,
	}
}

// disableCorrectionsFromConfig resolves the operator-facing
// `llm.corrections.enabled` field onto the snapshot's
// `DisableCorrections` bool (the inverse — a nil pointer means
// "enabled by default").
func disableCorrectionsFromConfig(cfg config.LLMCorrectionsConfig) bool {
	if cfg.Enabled == nil {
		return false
	}
	return !*cfg.Enabled
}

// cloneStringValueMap returns a defensive copy of m so the snapshot
// cannot be mutated through the caller's retained config maps.
func cloneStringValueMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

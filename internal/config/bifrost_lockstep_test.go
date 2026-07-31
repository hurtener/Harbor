package config

import (
	"sort"
	"strings"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"
)

// TestNativeBifrostProviders_LockstepWithSDK is the guard that makes
// `nativeBifrostProviders`' "mirror" godoc a checked property instead of
// a claim.
//
// The mirror gates boot: `validateLLMCustomProviders` REJECTS an
// `llm.provider` outside it, and prints the mirror as the authoritative
// set of native providers. So a stale mirror does not fail safe in the
// way that phrase usually implies — it fails CLOSED on a provider the
// SDK supports, with an error message asserting something false about
// the SDK. That shipped: a bump from bifrost v1.5.8 to v1.7.4 added
// seven native providers (`bedrock_mantle`, `deepseek`, `opencode-go`,
// `opencode-zen`, `runware`, `sarvam`, `wafer`) and the mirror did not
// move, so `llm.provider: deepseek` was refused at boot as "not a
// native bifrost provider".
//
// The import of `bfschemas` is deliberately TEST-ONLY. Deriving the map
// in production would remove this drift structurally, but at the cost of
// tripling the config package's dependency closure (93 → ~310 packages,
// including a JIT-assembly JSON codec) for a package every binary and
// embedder loads to parse `harbor.yaml`. The test binary links the SDK
// for free, so the guard is paid for where it is cheap.
//
// Equality is asserted in BOTH directions on purpose. An upstream ADD
// that the mirror misses is the drift that shipped; an upstream REMOVE
// that the mirror keeps is the inverse, and would leave Harbor accepting
// a provider the driver can no longer resolve — a boot that passes
// validation and then fails deeper, which is strictly worse than a
// refusal at the config edge.
func TestNativeBifrostProviders_LockstepWithSDK(t *testing.T) {
	upstream := make(map[string]struct{}, len(bfschemas.StandardProviders))
	for _, p := range bfschemas.StandardProviders {
		name := string(p)
		if name == "" {
			t.Fatal("bfschemas.StandardProviders contains an empty provider name")
		}
		if _, dup := upstream[name]; dup {
			t.Fatalf("bfschemas.StandardProviders lists %q twice", name)
		}
		upstream[name] = struct{}{}
	}
	if len(upstream) == 0 {
		t.Fatal("bfschemas.StandardProviders is empty — the SDK surface moved and this guard is no longer reading the provider list")
	}

	var missing []string // upstream has it, the mirror does not
	for name := range upstream {
		if _, ok := nativeBifrostProviders[name]; !ok {
			missing = append(missing, name)
		}
	}
	var extra []string // the mirror has it, upstream does not
	for name := range nativeBifrostProviders {
		if _, ok := upstream[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("nativeBifrostProviders is MISSING %d provider(s) the bifrost SDK ships natively: %s\n"+
			"An operator setting `llm.provider` to any of these is refused at boot with a message asserting it is not bifrost-native, which is false. "+
			"Add them to nativeBifrostProviders in internal/config/validate.go.",
			len(missing), strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("nativeBifrostProviders lists %d provider(s) the bifrost SDK no longer ships: %s\n"+
			"Config would accept these and the driver would then refuse to resolve them, moving the failure past validation. "+
			"Remove them from nativeBifrostProviders in internal/config/validate.go.",
			len(extra), strings.Join(extra, ", "))
	}
}

// TestValidateLLMProvider_AcceptsEveryNativeSDKProvider drives the guard
// through the actual boot-gating call path rather than comparing two
// maps, so a future refactor that keeps the mirror correct but stops
// CONSULTING it still fails here.
//
// THE PRECONDITION IS LOAD-BEARING AND WAS NEARLY MISSED. The native
// cross-check sits AFTER an early return that fires when no custom
// providers are declared:
//
//	if driver == "mock" || len(c.LLM.CustomProviders) == 0 { return }
//
// So a first cut of this test that set only `llm.provider` passed for
// every name — including names deliberately deleted from the mirror —
// because validation returned before reaching the arm under test. The
// unrelated custom-provider entry below exists solely to get past that
// early return, and removing it makes this test inert rather than red.
//
// It also states the true blast radius of a stale mirror, which is
// narrower than "every deployment": `llm.provider: deepseek` on a config
// with NO `llm.custom_providers` was always accepted here and resolved
// natively by the driver. The false refusal reached only operators who
// declared at least one custom provider alongside it.
func TestValidateLLMProvider_AcceptsEveryNativeSDKProvider(t *testing.T) {
	for _, p := range bfschemas.StandardProviders {
		name := string(p)
		t.Run(name, func(t *testing.T) {
			c := &Config{}
			c.LLM.Provider = name
			c.LLM.CustomProviders = []LLMCustomProviderConfig{sentinelCustomProvider()}
			if _, err := c.validateLLMCustomProviders("bifrost"); err != nil {
				t.Fatalf("llm.provider = %q (a native bifrost provider) was refused at config validation: %v", name, err)
			}
		})
	}
}

// sentinelCustomProvider is a fully-populated custom-provider entry whose
// name cannot collide with any native provider. Tests use it to get past
// `validateLLMCustomProviders`' empty-list early return so the native
// cross-check downstream of it actually executes.
//
// The env-var field carries a variable NAME, never a secret (§7 rule 2).
func sentinelCustomProvider() LLMCustomProviderConfig {
	return LLMCustomProviderConfig{
		Name:         "harbor-test-sentinel-gateway",
		BaseURL:      "https://example.invalid/v1",
		APIKeyEnvVar: "HARBOR_TEST_UNSET_PROVIDER_KEY",
		Models:       []string{"a-model"},
	}
}

// TestValidateLLMProvider_RefusesCustomProviderShadowingNativeName pins
// the OTHER edge the mirror gates, which widened with it: a custom
// provider may not take a native provider's name.
//
// This is not incidental to the mirror fix — it is a behaviour change
// the fix carries. Before it, `deepseek` was not in the mirror, so an
// operator could declare a custom provider under that name and the
// driver would silently prefer the custom entry over the SDK's native
// one (`customByName` is consulted before `isKnownProvider`). Now the
// collision is refused at the config edge with a message naming it,
// which is the loud form of the same outcome (CLAUDE.md §5).
func TestValidateLLMProvider_RefusesCustomProviderShadowingNativeName(t *testing.T) {
	for _, name := range []string{"deepseek", "openai"} {
		t.Run(name, func(t *testing.T) {
			c := &Config{}
			c.LLM.Provider = name
			c.LLM.CustomProviders = []LLMCustomProviderConfig{{
				Name: name,
				// Every field the validator checks BEFORE the collision
				// arm is populated, so the test reaches the arm it is
				// about rather than tripping an earlier one. The env-var
				// name is a variable NAME, never a secret (§7 rule 2).
				BaseURL:      "https://example.invalid/v1",
				APIKeyEnvVar: "HARBOR_TEST_UNSET_PROVIDER_KEY",
				Models:       []string{"a-model"},
			}}
			_, err := c.validateLLMCustomProviders("bifrost")
			if err == nil {
				t.Fatalf("a custom provider named %q (a native bifrost provider) was accepted — the shadowing collision is unguarded", name)
			}
			if !strings.Contains(err.Error(), "collides with a native bifrost provider") {
				t.Fatalf("expected the native-collision error for %q, got: %v", name, err)
			}
		})
	}
}

package agentcfg

import "testing"

func TestPromptLayersGuidance_CanonicalCompatibility(t *testing.T) {
	const legacyJSON = `{"prompt_layers":{"base":"base","user":"user"}}`
	const legacyHash = "6e35d39deec4372425ce944a06a8b755e42f31c53a6a8eb3e699dba32fea602a"
	base, user := "base", "user"
	empty, blank := "", " \t\n"
	for _, guidance := range []*string{nil, &empty, &blank} {
		payload := ConfigPayload{PromptLayers: &PromptLayers{Base: &base, User: &user, Guidance: guidance}}
		normalized := NormalizePayload(payload)
		if got := normalized.GuidancePrompt(); got != nil {
			t.Fatalf("normalized guidance = %q, want inherited", *got)
		}
		canonical, err := canonicalJSON(normalized)
		if err != nil || string(canonical) != legacyJSON {
			t.Fatalf("canonical payload = %s, err=%v", canonical, err)
		}
		hash, err := ContentHash(payload)
		if err != nil || hash != legacyHash {
			t.Fatalf("content hash = %q, err=%v", hash, err)
		}
	}
	if got := NormalizePayload(ConfigPayload{PromptLayers: &PromptLayers{Guidance: &blank}}); got.PromptLayers != nil {
		t.Fatal("a blank-only prompt section must normalize away")
	}
	guidance := "  <operator>follow this</operator>\n"
	withGuidance := ConfigPayload{PromptLayers: &PromptLayers{Base: &base, User: &user, Guidance: &guidance}}
	if got := NormalizePayload(withGuidance).GuidancePrompt(); got == nil || *got != guidance {
		t.Fatalf("nonblank guidance was changed: %v", got)
	}
	hash, err := ContentHash(withGuidance)
	if err != nil || hash == legacyHash {
		t.Fatalf("override must affect the content hash: hash=%q, err=%v", hash, err)
	}
	withoutGuidance := ConfigPayload{PromptLayers: &PromptLayers{Base: &base, User: &user, Guidance: &blank}}
	diff := DiffPromptLayers(withoutGuidance, withGuidance)
	if !diff.Changed() || !diff.GuidanceChanged || diff.GuidanceFrom != "" || diff.GuidanceTo != guidance || diff.BaseChanged || diff.UserChanged {
		t.Fatalf("unexpected guidance-only delta: %+v", diff)
	}
}

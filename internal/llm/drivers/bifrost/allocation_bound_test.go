package bifrost

import (
	"errors"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestAllocationBound_PhysicalTimesLogicalAndClosedFamilies(t *testing.T) {
	req := llm.CompleteRequest{Model: "fixture", MaxTokens: new(100)}
	for _, provider := range []bfschemas.ModelProvider{bfschemas.OpenAI, bfschemas.Anthropic, "custom-fixture", bfschemas.Gemini, bfschemas.Bedrock, bfschemas.OpenRouter} {
		for _, retries := range []int{-1, 0, 1, 198, 199, 998, int(^uint(0) >> 1)} {
			cfg := &bfschemas.ProviderConfig{NetworkConfig: bfschemas.NetworkConfig{MaxRetries: retries}}
			if provider == "custom-fixture" {
				cfg.CustomProviderConfig = &bfschemas.CustomProviderConfig{BaseProviderType: bfschemas.OpenAI}
			}
			d := &Driver{provider: provider, account: &Account{provider: provider, primaryConfig: cfg}}
			n, err := d.ProviderAttemptBound(t.Context(), req)
			admitted := (provider == bfschemas.OpenAI || provider == bfschemas.Anthropic || provider == "custom-fixture") && retries >= 0 && retries <= 198
			if !admitted {
				if !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
					t.Fatalf("%s/%d accepted: %d %v", provider, retries, n, err)
				}
				continue
			}
			want := (retries + 2) * fasthttp.DefaultMaxIdemponentCallAttempts
			if err != nil || n != want {
				t.Fatalf("%s/%d: got %d %v want %d", provider, retries, n, err, want)
			}
		}
	}
	for _, d := range []*Driver{{}, {provider: bfschemas.OpenAI, account: &Account{provider: bfschemas.OpenAI}}, {provider: bfschemas.OpenAI, account: &Account{provider: bfschemas.OpenAI, primaryConfig: &bfschemas.ProviderConfig{CustomProviderConfig: &bfschemas.CustomProviderConfig{BaseProviderType: bfschemas.Gemini}}}}} {
		if _, err := d.ProviderAttemptBound(t.Context(), req); !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
			t.Fatal("unproved factory", err)
		}
	}
}

func TestAllocationBound_SelectedRouteAndRequestShape(t *testing.T) {
	d := &Driver{provider: bfschemas.OpenAI, account: &Account{provider: bfschemas.OpenAI, primaryConfig: &bfschemas.ProviderConfig{}}}
	req := llm.CompleteRequest{Model: "fixture", MaxTokens: new(100)}
	routed := llm.WithTrustedProviderRoute(t.Context(), llm.TrustedProviderRouteContext{})
	if _, err := d.ProviderAttemptBound(routed, req); !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
		t.Fatal("missing selection", err)
	}
	for _, provider := range curatedRouteProviders {
		ctx := llm.WithSelectedProviderRoute(routed, llm.SelectedProviderRoute{Provider: string(provider), Model: req.Model})
		n, err := d.ProviderAttemptBound(ctx, req)
		if provider == bfschemas.OpenAI || provider == bfschemas.Anthropic {
			if err != nil || n != 2*fasthttp.DefaultMaxIdemponentCallAttempts {
				t.Fatalf("route %s: %d %v", provider, n, err)
			}
		} else if !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
			t.Fatalf("unproved route %s accepted: %d %v", provider, n, err)
		}
	}
	for _, selection := range []llm.SelectedProviderRoute{{Provider: "openai", Model: "different"}, {Provider: "openai", Model: req.Model, Endpoint: &llm.ProviderEndpointBinding{Kind: llm.ProviderEndpointOpenAICompatible, Value: "http://127.0.0.1:1", Digest: "invalid"}}} {
		if _, err := d.ProviderAttemptBound(llm.WithSelectedProviderRoute(routed, selection), req); !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
			t.Fatal("unbound route", err)
		}
	}
	endpoint, digest, err := llm.NormalizeProviderEndpoint("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := llm.WithSelectedProviderRoute(routed, llm.SelectedProviderRoute{Provider: "openai", Model: req.Model, Endpoint: &llm.ProviderEndpointBinding{Kind: llm.ProviderEndpointOpenAICompatible, Value: endpoint, Digest: digest}})
	if n, err := d.ProviderAttemptBound(ctx, req); err != nil || n != 10 {
		t.Fatalf("compatible route: %d %v", n, err)
	}
	for _, part := range []llm.ContentPart{{Type: llm.PartImage, Image: &llm.ImagePart{}}, {Type: llm.PartAudio, Audio: &llm.AudioPart{}}, {Type: llm.PartFile, File: &llm.FilePart{}}, {Type: llm.PartText, File: &llm.FilePart{}}} {
		req.Messages = []llm.ChatMessage{{Content: llm.Content{Parts: []llm.ContentPart{part}}}}
		if _, err := d.ProviderAttemptBound(t.Context(), req); !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
			t.Fatal("auxiliary work accepted", err)
		}
	}
	req.Messages = nil
	for _, extra := range []map[string]any{{"fallbacks": []string{"other/model"}}, {"max_tokens": 999999}, {"raw_request_body": "opaque"}} {
		req.Extra = extra
		if _, err := d.ProviderAttemptBound(t.Context(), req); !errors.Is(err, llm.ErrAllocationBoundUnavailable) {
			t.Fatal("opaque request accepted", err)
		}
	}
	req.Extra = nil
	req.Messages = []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("ordinary text")}}}
	for _, provider := range []bfschemas.ModelProvider{bfschemas.OpenAI, bfschemas.Anthropic} {
		translated, err := translateRequest(provider, req)
		if err != nil || len(translated.Fallbacks) != 0 || len(translated.RawRequestBody) != 0 || translated.Params == nil || translated.Params.MaxCompletionTokens == nil || *translated.Params.MaxCompletionTokens != 100 || len(translated.Params.ExtraParams) != 0 {
			t.Fatalf("unproved request translation for %s: %+v %v", provider, translated, err)
		}
	}
}

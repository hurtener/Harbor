package bifrost

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
)

// This crosses the planner materializer, mandatory LLM safety pass, and the
// Bifrost provider request. A small PNG would miss the heavy-output threshold.
func TestUploadedPNGProviderRequestAboveHeavyThreshold(t *testing.T) {
	const threshold = 256 << 10
	imageBytes := largeValidPNG(t)
	if len(imageBytes) <= threshold || len(imageBytes) > 3<<20 {
		t.Fatalf("fixture size %d must cross threshold within direct-input bound", len(imageBytes))
	}
	for _, tc := range []struct {
		name       string
		imageInput *bool
		wantImage  bool
	}{
		{"vision", boolPtr(true), true},
		{"nonvision", boolPtr(false), false},
		{"unknown", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubClient()
			driverName := "image-request-test-" + tc.name
			llm.Register(driverName, func(_ llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
				return newDriverWithClientAndStore(stub, bfschemas.OpenAI, deps.Bus, deps.Artifacts), nil
			})
			deps, cleanup := makeCustomProviderTestDeps(t)
			defer cleanup()
			cfg := llm.ConfigSnapshot{
				Driver: driverName, Model: "image-model",
				HeavyOutputThreshold: threshold, ContextWindowReserve: .05,
				DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true,
				ModelProfiles: map[string]llm.ModelProfile{"image-model": {ContextWindowTokens: 100000, ImageInput: tc.imageInput}},
			}
			client, err := llm.Open(t.Context(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close(context.Background()) }()
			ctx, err := identity.With(t.Context(), identity.Identity{TenantID: "tenant", UserID: "user", SessionID: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			content := planner.MaterializeInputContent("What is shown?", []planner.InputArtifactView{{
				ID: "uploaded-png", MIME: "image/png", Filename: "photo.png", SizeBytes: int64(len(imageBytes)), Bytes: imageBytes,
			}}, nil)
			if _, err := client.Complete(ctx, llm.CompleteRequest{Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: content}}}); err != nil {
				t.Fatal(err)
			}
			request := stub.lastRequest()
			if request == nil || len(request.Input) != 1 || request.Input[0].Content == nil {
				t.Fatal("Bifrost did not receive the provider request")
			}
			blocks := request.Input[0].Content.ContentBlocks
			if tc.wantImage {
				want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
				if len(blocks) != 2 || blocks[1].Type != bfschemas.ChatContentBlockTypeImage ||
					blocks[1].ImageURLStruct == nil || blocks[1].ImageURLStruct.URL != want {
					t.Fatal("provider request did not carry the uploaded PNG image block")
				}
			} else {
				for _, block := range blocks {
					if block.Type == bfschemas.ChatContentBlockTypeImage || block.Type == bfschemas.ChatContentBlockTypeFile {
						t.Fatal("nonvision provider received image or file bytes")
					}
				}
				if len(blocks) != 2 || blocks[1].Text == nil ||
					!strings.Contains(*blocks[1].Text, "does not support image input") ||
					!strings.Contains(*blocks[1].Text, "Do not claim to have inspected") {
					t.Fatal("nonvision request did not include clear withheld-image note")
				}
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func largeValidPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	var state uint32 = 1
	for y := range 512 {
		for x := range 512 {
			state = state*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: byte(state >> 24), G: byte(state >> 16), B: byte(state >> 8), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

package llm_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

// TestSafety_ProviderFileIDOnlyPart_IsLegalOverThreshold — the edge
// guard exemption for the provider-native path (RFC §6.5 / §6.10):
// a part whose only content is an opaque `ProviderFileID` carries no
// inline bytes regardless of the referenced file's size, so the
// D-026 context-leak guard MUST NOT fire on it and the safety pass
// lets the request through to the driver.
func TestSafety_ProviderFileIDOnlyPart_IsLegalOverThreshold(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartText, Text: "describe the attached file"},
				// file_id-only parts across all three modal shapes —
				// no URL, no DataURL, no Artifact, no inline bytes.
				{Type: llm.PartImage, Image: &llm.ImagePart{
					ProviderFileID: "file-img-1", MIME: "image/png", ProviderNative: true,
				}},
				{Type: llm.PartAudio, Audio: &llm.AudioPart{
					ProviderFileID: "file-aud-1", MIME: "audio/wav", ProviderNative: true,
				}},
				{Type: llm.PartFile, File: &llm.FilePart{
					ProviderFileID: "file-doc-1", MIME: "application/pdf",
					DocumentType: "pdf", ProviderNative: true,
				}},
			}},
		}},
	}
	if _, err := client.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v — a file_id-only part must be legal over-threshold", err)
	}
}

// TestSafety_ProviderFileIDPart_SmuggledBytesStillHandled — a part
// that carries a ProviderFileID AND an over-threshold inline DataURL
// does not slip raw bytes past the safety net: the auto-materialize
// step rewrites the oversize DataURL to an Artifact reference exactly
// as for any other part (the exemption is precise — it covers the
// absence of bytes, never their presence), and a raw heavy TEXT part
// in the same message still trips ErrContextLeak.
func TestSafety_ProviderFileIDPart_SmuggledBytesStillHandled(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())

	// Oversize DataURL alongside a ProviderFileID: materialized, not
	// leaked — the call succeeds.
	big := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("Z", 40*1024)))
	okReq := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartImage, Image: &llm.ImagePart{
					ProviderFileID: "file-img-2",
					DataURL:        "data:image/png;base64," + big,
					MIME:           "image/png",
				}},
			}},
		}},
	}
	if _, err := client.Complete(ctx, okReq); err != nil {
		t.Fatalf("Complete: %v — oversize DataURL must be auto-materialized, not fatal", err)
	}

	// Raw heavy text in a sibling part still trips the guard — the
	// exemption never blankets the whole message.
	leakReq := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartImage, Image: &llm.ImagePart{
					ProviderFileID: "file-img-3", MIME: "image/png",
				}},
				{Type: llm.PartText, Text: strings.Repeat("Y", 40*1024)},
			}},
		}},
	}
	if _, err := client.Complete(ctx, leakReq); !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("err = %v, want ErrContextLeak for the sibling raw heavy text part", err)
	}
}

package llm_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// makeImageDataURL builds a data: URL whose decoded payload is `size`
// bytes of a deterministic pattern.
func makeImageDataURL(size int) string {
	raw := []byte(strings.Repeat("Z", size))
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestMaterialize_OversizeDataURLBecomesArtifact(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeImageMaterialized)

	ctx := withIdentity(t, context.Background())
	const size = 40 * 1024 // > 32 KiB threshold
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type: llm.PartImage,
				Image: &llm.ImagePart{
					DataURL: makeImageDataURL(size),
					MIME:    "image/png",
				},
			}}},
		}},
	}
	_, err = client.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(llm.ImageMaterializedPayload)
		if !ok {
			t.Fatalf("payload type=%T, want ImageMaterializedPayload", ev.Payload)
		}
		if p.SizeBytes != int64(size) {
			t.Errorf("payload.SizeBytes=%d, want %d", p.SizeBytes, size)
		}
		if p.MIME != "image/png" {
			t.Errorf("payload.MIME=%q, want image/png", p.MIME)
		}
		if p.ArtifactRef == "" {
			t.Errorf("payload.ArtifactRef is empty")
		}
		// Identity made it through the materialize path.
		if p.Identity.TenantID != "T" || p.Identity.UserID != "U" || p.Identity.SessionID != "S" {
			t.Errorf("payload.Identity=%+v, want T/U/S", p.Identity)
		}
		// Verify the artifact landed in the store under the calling
		// identity's scope (multi-isolation gate).
		scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
		got, ok, err := deps.Artifacts.Get(ctx, scope, p.ArtifactRef)
		if err != nil {
			t.Fatalf("artifacts.Get: %v", err)
		}
		if !ok {
			t.Fatalf("artifacts.Get(%q): not found", p.ArtifactRef)
		}
		if len(got) != size {
			t.Errorf("artifact bytes len=%d, want %d", len(got), size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe llm.image.materialized within 2s")
	}
}

func TestMaterialize_BelowThreshold_NoOp(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeImageMaterialized)

	ctx := withIdentity(t, context.Background())
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type: llm.PartImage,
				Image: &llm.ImagePart{
					DataURL: makeImageDataURL(1024), // 1 KiB; well below threshold
					MIME:    "image/png",
				},
			}}},
		}},
	}
	if _, err := client.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected materialize event: %v", ev.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMaterialize_ExistingArtifactNoOp(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeImageMaterialized)

	ctx := withIdentity(t, context.Background())
	stub := &llm.ArtifactStub{Ref: "ref-existing", MIME: "image/png", SizeBytes: 1234}
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type:  llm.PartImage,
				Image: &llm.ImagePart{Artifact: stub, MIME: "image/png"},
			}}},
		}},
	}
	if _, err := client.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected materialize event for existing artifact: %v", ev.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestArtifactStub_RoundTrip(t *testing.T) {
	stub := llm.ArtifactStub{
		Ref:       "ref-abc",
		MIME:      "image/png",
		SizeBytes: 4096,
		Hash:      "sha256:deadbeef",
		Summary:   "screenshot",
		Fetch:     &llm.StubFetch{Tool: "artifact.fetch", ID: "ref-abc"},
	}
	b, err := stub.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	s := string(b)
	// All required fields present + canonical names.
	for _, want := range []string{
		`"artifact_ref":"ref-abc"`,
		`"mime":"image/png"`,
		`"size_bytes":4096`,
		`"hash":"sha256:deadbeef"`,
		`"summary":"screenshot"`,
		`"fetch":{"tool":"artifact.fetch","id":"ref-abc"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ArtifactStub JSON %q missing %q", s, want)
		}
	}
}

func TestMaterialize_MissingIdentityRejected(t *testing.T) {
	// The materialize step runs INSIDE the safety pass; the safety
	// pass requires identity. This test asserts the boundary check
	// fires before materialize even sees the request.
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type: llm.PartImage,
				Image: &llm.ImagePart{
					DataURL: makeImageDataURL(40 * 1024),
					MIME:    "image/png",
				},
			}}},
		}},
	}
	// No identity in ctx → ErrIdentityMissing.
	if _, err := client.Complete(context.Background(), req); err == nil {
		t.Fatal("Complete accepted request without identity")
	}

	// Confirm that with identity in ctx the same request succeeds
	// (sanity check that materialize works under the same shape).
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"})
	if _, err := client.Complete(ctx, req); err != nil {
		t.Fatalf("Complete with identity: %v", err)
	}
}

// Fetch-hint ↔ builtin lockstep (Phase 84b PR — §17.6 fix).
//
// The pre-84b auto-materialize pass stamped `Fetch.Tool:
// "artifact.fetch"` (dot) on every over-threshold attachment's stub —
// a tool that exists NOWHERE in the catalog (the registered Phase
// 107c meta-tool is `artifact_fetch`). The LLM was instructed to call
// a nonexistent tool and could never recover the bytes (the
// Playground "agent doesn't know what to do with my image" report).
// This test pins the lockstep mechanically: the stub the safety pass
// hands the driver MUST name a tool the builtin set actually
// registers.
package llm_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

// fetchHintCaptureDriver records the post-safety-pass request the
// driver receives, so the test reads the materialized stub.
type fetchHintCaptureDriver struct {
	mu  sync.Mutex
	req llm.CompleteRequest
}

func (d *fetchHintCaptureDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.mu.Lock()
	d.req = req
	d.mu.Unlock()
	return llm.CompleteResponse{Content: "ok"}, nil
}

func (d *fetchHintCaptureDriver) Close(_ context.Context) error { return nil }

// fetchHintDriver / fetchHintRegisterOnce — the driver registry is
// process-wide write-once (PR #16 lesson: a per-test Register panics
// under `go test -count=N`), so the capture driver registers exactly
// once and the test swaps the capture target through the shared
// pointer.
var (
	fetchHintDriver       = &fetchHintCaptureDriver{}
	fetchHintRegisterOnce sync.Once
)

func TestMaterialize_FetchHint_NamesRegisteredBuiltin(t *testing.T) {
	fetchHintRegisterOnce.Do(func() {
		llm.Register("fetchhint-capture", func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) {
			return fetchHintDriver, nil
		})
	})

	deps, cleanup := makeDeps(t)
	defer cleanup()
	snap := makeSnapshot("m", 1_000_000)
	snap.Driver = "fetchhint-capture"
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background())
	const size = 40 * 1024 // > the 32 KiB threshold → auto-materialize fires
	if _, err := client.Complete(ctx, llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{{
				Type:  llm.PartImage,
				Image: &llm.ImagePart{DataURL: makeImageDataURL(size), MIME: "image/png"},
			}}},
		}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	fetchHintDriver.mu.Lock()
	got := fetchHintDriver.req
	fetchHintDriver.mu.Unlock()
	var stubTool string
	for _, m := range got.Messages {
		for _, p := range m.Content.Parts {
			if p.Type == llm.PartImage && p.Image != nil && p.Image.Artifact != nil && p.Image.Artifact.Fetch != nil {
				stubTool = p.Image.Artifact.Fetch.Tool
			}
		}
	}
	if stubTool == "" {
		t.Fatal("the safety pass did not materialize the oversize image to a stub with a fetch hint")
	}

	// The lockstep: the hinted name resolves in a catalog carrying the
	// REAL builtin registrations.
	cat := tools.NewCatalog()
	if err := builtin.RegisterWith(builtin.RegistryContext{Catalog: cat}, []string{"artifact_fetch"}); err != nil {
		t.Fatalf("builtin.RegisterWith: %v", err)
	}
	if _, found := cat.Resolve(stubTool); !found {
		t.Fatalf("materialized stub hints Fetch.Tool=%q, which the builtin set does not register (want artifact_fetch)", stubTool)
	}
}

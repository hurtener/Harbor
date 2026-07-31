// Provider-native multimodal upload-pass tests (Phase 84c — D-190).
//
// Covers: per-modality upload + part rewrite, the identity-scoped
// file_id cache (re-attach reuse, cross-session isolation, TTL expiry
// with remote delete, LRU eviction, Close-time sweep), the loud
// degradation for providers without upload support, fail-loud paths
// (missing store / missing artifact / upload failure), the headless
// hand-built-request guarantee, streaming-with-multimodal, the
// caller-request immutability contract, and the D-025 concurrent-
// reuse contract for the cache (N≥100 under -race).
package bifrost

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	providerutils "github.com/maximhq/bifrost/core/providers/utils"
	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
)

// openArtifactStore builds the production inmem artifact store (the
// driver blank import rides conformance_test.go in this package).
func openArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// putTestArtifact stores bytes under the given session's scope and
// returns the stub the materializer would emit for it.
func putTestArtifact(t *testing.T, store artifacts.ArtifactStore, sess, mime string, payload []byte) *llm.ArtifactStub {
	t.Helper()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: sess}
	ref, err := store.PutBytes(context.Background(), scope, payload, artifacts.PutOpts{MimeType: mime, Namespace: "test"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return &llm.ArtifactStub{Ref: ref.ID, MIME: mime, SizeBytes: int64(len(payload))}
}

// providerNativeImageReq builds a hand-built CompleteRequest carrying
// one provider_native image part backed by the given stub.
func providerNativeImageReq(stub *llm.ArtifactStub) llm.CompleteRequest {
	return llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartText, Text: "what is in this image?"},
				{Type: llm.PartImage, Image: &llm.ImagePart{
					Artifact:       stub,
					MIME:           stub.MIME,
					ProviderNative: true,
				}},
			}},
		}},
	}
}

// findFileBlocks returns every file-typed content block in the
// recorded bifrost request.
func findFileBlocks(req *bfschemas.BifrostChatRequest) []*bfschemas.ChatInputFile {
	var out []*bfschemas.ChatInputFile
	if req == nil {
		return nil
	}
	for _, m := range req.Input {
		if m.Content == nil || m.Content.ContentBlocks == nil {
			continue
		}
		for _, b := range m.Content.ContentBlocks {
			if b.Type == bfschemas.ChatContentBlockTypeFile && b.File != nil {
				out = append(out, b.File)
			}
		}
	}
	return out
}

// TestProviderNative_ImageUpload_RewritesAndEmits — the priority-1
// modality: an over-threshold image artifact flagged provider_native
// uploads via FileUploadRequest (purpose=vision), the chat request
// carries the file_id reference, and `llm.provider_file.uploaded` is
// published with the full payload.
func TestProviderNative_ImageUpload_RewritesAndEmits(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeProviderFileUploaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	store := openArtifactStore(t)
	payload := []byte(strings.Repeat("png-bytes", 64))
	stub := putTestArtifact(t, store, "s-img", "image/png", payload)

	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, bus, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-img")
	if _, err := drv.Complete(ctx, providerNativeImageReq(stub)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	uploads := stubC.recordedUploads()
	if len(uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(uploads))
	}
	up := uploads[0]
	if string(up.Purpose) != string(bfschemas.FilePurposeVision) {
		t.Errorf("upload purpose = %q, want vision for image/*", up.Purpose)
	}
	if len(up.File) != len(payload) {
		t.Errorf("upload bytes = %d, want %d", len(up.File), len(payload))
	}
	if up.ContentType == nil || *up.ContentType != "image/png" {
		t.Errorf("upload content type = %v, want image/png", up.ContentType)
	}

	blocks := findFileBlocks(stubC.lastRequest())
	if len(blocks) != 1 || blocks[0].FileID == nil || *blocks[0].FileID == "" {
		t.Fatalf("chat request file blocks = %+v, want one with a file_id", blocks)
	}
	if blocks[0].FileType == nil || *blocks[0].FileType != "image/png" {
		t.Errorf("file block FileType = %v, want image/png", blocks[0].FileType)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(llm.ProviderFileUploadedPayload)
		if !ok {
			t.Fatalf("payload type = %T", ev.Payload)
		}
		if p.Modality != "image" || p.ArtifactRef != stub.Ref || p.FileID != *blocks[0].FileID {
			t.Errorf("payload = %+v, want image modality + ref %q + file id %q", p, stub.Ref, *blocks[0].FileID)
		}
		if p.Provider != string(bfschemas.OpenAI) || p.Identity.SessionID != "s-img" {
			t.Errorf("payload provider/identity = %q / %+v", p.Provider, p.Identity)
		}
		if p.SizeBytes != int64(len(payload)) {
			t.Errorf("payload SizeBytes = %d, want %d", p.SizeBytes, len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe llm.provider_file.uploaded within 2s")
	}
}

// TestProviderNative_Modalities — audio, video and pdf round through
// the same upload pass with the right purpose, modality vocabulary,
// and part shapes (audio via AudioPart; video + pdf via FilePart).
func TestProviderNative_Modalities(t *testing.T) {
	cases := []struct {
		name         string
		mime         string
		part         func(stub *llm.ArtifactStub) llm.ContentPart
		wantModality string
	}{
		{
			name: "audio",
			mime: "audio/wav",
			part: func(stub *llm.ArtifactStub) llm.ContentPart {
				return llm.ContentPart{Type: llm.PartAudio, Audio: &llm.AudioPart{
					Artifact: stub, MIME: stub.MIME, ProviderNative: true,
				}}
			},
			wantModality: "audio",
		},
		{
			name: "video",
			mime: "video/mp4",
			part: func(stub *llm.ArtifactStub) llm.ContentPart {
				return llm.ContentPart{Type: llm.PartFile, File: &llm.FilePart{
					Artifact: stub, MIME: stub.MIME, ProviderNative: true,
				}}
			},
			wantModality: "video",
		},
		{
			name: "pdf",
			mime: "application/pdf",
			part: func(stub *llm.ArtifactStub) llm.ContentPart {
				return llm.ContentPart{Type: llm.PartFile, File: &llm.FilePart{
					Artifact: stub, MIME: stub.MIME, Filename: "doc.pdf",
					ProviderNative: true, DocumentType: "pdf",
				}}
			},
			wantModality: "pdf",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus, busCleanup := openBus(t)
			defer busCleanup()
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Admin: true,
				Types: []events.EventType{llm.EventTypeProviderFileUploaded},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			store := openArtifactStore(t)
			sess := "s-" + tc.name
			stub := putTestArtifact(t, store, sess, tc.mime, []byte("payload-"+tc.name))
			stubC := newStubClient()
			drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, bus, store)
			defer func() { _ = drv.Close(context.Background()) }()

			ctx := withIdentity(t, context.Background(), sess)
			req := llm.CompleteRequest{
				Model: "m",
				Messages: []llm.ChatMessage{{
					Role:    llm.RoleUser,
					Content: llm.Content{Parts: []llm.ContentPart{tc.part(stub)}},
				}},
			}
			if _, err := drv.Complete(ctx, req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			uploads := stubC.recordedUploads()
			if len(uploads) != 1 {
				t.Fatalf("uploads = %d, want 1", len(uploads))
			}
			if string(uploads[0].Purpose) != string(bfschemas.FilePurposeUserData) {
				t.Errorf("purpose = %q, want user_data for %s", uploads[0].Purpose, tc.mime)
			}
			blocks := findFileBlocks(stubC.lastRequest())
			if len(blocks) != 1 || blocks[0].FileID == nil {
				t.Fatalf("file blocks = %+v, want one with file_id", blocks)
			}
			select {
			case ev := <-sub.Events():
				p := ev.Payload.(llm.ProviderFileUploadedPayload)
				if p.Modality != tc.wantModality {
					t.Errorf("modality = %q, want %q", p.Modality, tc.wantModality)
				}
			case <-time.After(2 * time.Second):
				t.Error("no upload event within 2s")
			}
		})
	}
}

// TestProviderNative_CacheAvoidsReupload — re-attaching the same
// artifact in the same identity scope reuses the cached file_id: one
// upload, one event, two chat calls both carrying the same file_id.
func TestProviderNative_CacheAvoidsReupload(t *testing.T) {
	store := openArtifactStore(t)
	stub := putTestArtifact(t, store, "s-cache", "image/png", []byte(strings.Repeat("img", 100)))
	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-cache")
	for i := range 2 {
		if _, err := drv.Complete(ctx, providerNativeImageReq(stub)); err != nil {
			t.Fatalf("Complete[%d]: %v", i, err)
		}
	}
	if got := stubC.uploadCalls.Load(); got != 1 {
		t.Errorf("upload calls = %d, want 1 (cache must absorb the re-attach)", got)
	}
	blocks := findFileBlocks(stubC.lastRequest())
	if len(blocks) != 1 || blocks[0].FileID == nil || *blocks[0].FileID == "" {
		t.Fatalf("second call file blocks = %+v, want the cached file_id", blocks)
	}
}

// TestProviderNative_CacheIsolation_AcrossSessions — the same content
// attached from two different sessions never shares a file_id: the
// cache key carries the identity triple (AGENTS.md §6).
func TestProviderNative_CacheIsolation_AcrossSessions(t *testing.T) {
	store := openArtifactStore(t)
	payload := []byte(strings.Repeat("shared-bytes", 32))
	stubA := putTestArtifact(t, store, "s-A", "image/png", payload)
	stubB := putTestArtifact(t, store, "s-B", "image/png", payload)

	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctxA := withIdentity(t, context.Background(), "s-A")
	if _, err := drv.Complete(ctxA, providerNativeImageReq(stubA)); err != nil {
		t.Fatalf("Complete A: %v", err)
	}
	fileA := findFileBlocks(stubC.lastRequest())[0].FileID

	ctxB := withIdentity(t, context.Background(), "s-B")
	if _, err := drv.Complete(ctxB, providerNativeImageReq(stubB)); err != nil {
		t.Fatalf("Complete B: %v", err)
	}
	fileB := findFileBlocks(stubC.lastRequest())[0].FileID

	if got := stubC.uploadCalls.Load(); got != 2 {
		t.Errorf("upload calls = %d, want 2 (no cross-session cache reuse)", got)
	}
	if fileA == nil || fileB == nil || *fileA == *fileB {
		t.Errorf("file ids A=%v B=%v must differ across sessions", fileA, fileB)
	}
}

// TestProviderNative_UnsupportedProvider_DegradesLoudly — a provider
// whose file surface returns bifrost's `unsupported_operation` keeps
// the canonical ArtifactStub rendering (the universal degradation,
// RFC §6.5): the call succeeds, the chat request carries the stub
// JSON text block, no file block, no upload event.
//
// THE FIXTURE IS BUILT BY THE UPSTREAM CONSTRUCTOR, NOT BY HAND
// (CLAUDE.md §17.8). It previously hand-assembled a BifrostError with
// the literal string "unsupported_operation" in Code — a fixture
// encoding this implementer's reading of bifrost's contract, which is
// self-consistent with the matcher by construction and would stay green
// against a matcher wired to the wrong field. It now calls
// `providerUtils.NewUnsupportedOperationError(FileUploadRequest,
// Cohere)` — byte-for-byte the call Cohere's own FileUpload makes
// (providers/cohere/cohere.go), for the same provider this test already
// drives. If upstream renames the code, moves it off `Error.Code`, or
// changes the field's type, this test goes red at the shape rather than
// at a string this repository chose.
func TestProviderNative_UnsupportedProvider_DegradesLoudly(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeProviderFileUploaded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	store := openArtifactStore(t)
	stub := putTestArtifact(t, store, "s-unsup", "image/png", []byte("img"))
	stubC := newStubClient()
	// The error the real Cohere provider returns from FileUpload, built by
	// the upstream constructor rather than transcribed here.
	unsupported := providerutils.NewUnsupportedOperationError(
		bfschemas.FileUploadRequest, bfschemas.Cohere)
	// Guard the fixture itself: if a future bifrost drops the code (or moves
	// it), the constructor still returns a *BifrostError and this test would
	// silently start covering the generic upload-failure path instead of the
	// degradation path it is named for.
	if unsupported == nil || unsupported.Error == nil || unsupported.Error.Code == nil {
		t.Fatalf("bifrost's NewUnsupportedOperationError no longer carries an Error.Code (%+v) — the degradation matcher keys on it, so this fixture can no longer exercise the path it names", unsupported)
	}
	stubC.uploadHandler = func(_ *bfschemas.BifrostFileUploadRequest) (*bfschemas.BifrostFileUploadResponse, *bfschemas.BifrostError) {
		return nil, unsupported
	}
	drv := newDriverWithClientAndStore(stubC, bfschemas.Cohere, bus, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-unsup")
	if _, err := drv.Complete(ctx, providerNativeImageReq(stub)); err != nil {
		t.Fatalf("Complete must succeed via the stub degradation, got: %v", err)
	}
	if blocks := findFileBlocks(stubC.lastRequest()); len(blocks) != 0 {
		t.Errorf("file blocks = %+v, want none (degraded to ArtifactStub)", blocks)
	}
	// The stub text block carries the canonical artifact_ref JSON.
	var sawStub bool
	for _, m := range stubC.lastRequest().Input {
		if m.Content == nil || m.Content.ContentBlocks == nil {
			continue
		}
		for _, b := range m.Content.ContentBlocks {
			if b.Type == bfschemas.ChatContentBlockTypeText && b.Text != nil && strings.Contains(*b.Text, stub.Ref) {
				sawStub = true
			}
		}
	}
	if !sawStub {
		t.Error("degraded request must carry the ArtifactStub reference text block")
	}
	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected upload event on the degradation path: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestProviderNative_UploadError_FailsLoudly — any non-unsupported
// upload failure fails the Complete (no silent degradation for
// transient/provider errors — AGENTS.md §13).
func TestProviderNative_UploadError_FailsLoudly(t *testing.T) {
	store := openArtifactStore(t)
	stub := putTestArtifact(t, store, "s-fail", "image/png", []byte("img"))
	stubC := newStubClient()
	status := 500
	stubC.uploadHandler = func(_ *bfschemas.BifrostFileUploadRequest) (*bfschemas.BifrostFileUploadResponse, *bfschemas.BifrostError) {
		return nil, &bfschemas.BifrostError{StatusCode: &status, Error: &bfschemas.ErrorField{Message: "boom"}}
	}
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-fail")
	_, err := drv.Complete(ctx, providerNativeImageReq(stub))
	if err == nil || !strings.Contains(err.Error(), "FileUploadRequest") {
		t.Fatalf("err = %v, want a loud FileUploadRequest failure", err)
	}
}

// TestProviderNative_TTLExpiry_DeletesAndReuploads — an expired cache
// entry triggers a best-effort remote FileDeleteRequest and the next
// attach re-uploads. Controllable clock; no sleeps (AGENTS.md §11).
func TestProviderNative_TTLExpiry_DeletesAndReuploads(t *testing.T) {
	store := openArtifactStore(t)
	stub := putTestArtifact(t, store, "s-ttl", "image/png", []byte("img-ttl"))
	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()

	var fakeNow atomic.Int64
	base := time.Now()
	fakeNow.Store(0)
	drv.files.ttl = time.Minute
	drv.files.now = func() time.Time { return base.Add(time.Duration(fakeNow.Load())) }

	ctx := withIdentity(t, context.Background(), "s-ttl")
	if _, err := drv.Complete(ctx, providerNativeImageReq(stub)); err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	firstID := *findFileBlocks(stubC.lastRequest())[0].FileID

	fakeNow.Store(int64(2 * time.Minute)) // past TTL
	if _, err := drv.Complete(ctx, providerNativeImageReq(stub)); err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if got := stubC.uploadCalls.Load(); got != 2 {
		t.Errorf("upload calls = %d, want 2 (expired entry re-uploads)", got)
	}
	deletes := stubC.recordedDeletes()
	if len(deletes) != 1 || deletes[0].FileID != firstID {
		t.Errorf("deletes = %+v, want exactly the expired file %q", deletes, firstID)
	}
}

// TestProviderNative_LRUEviction_DeletesRemote — past-capacity insert
// evicts the least-recently-used entry and deletes its remote file.
func TestProviderNative_LRUEviction_DeletesRemote(t *testing.T) {
	store := openArtifactStore(t)
	stubA := putTestArtifact(t, store, "s-lru", "image/png", []byte("img-A"))
	stubB := putTestArtifact(t, store, "s-lru", "image/png", []byte("img-B"))
	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()
	drv.files.capacity = 1

	ctx := withIdentity(t, context.Background(), "s-lru")
	if _, err := drv.Complete(ctx, providerNativeImageReq(stubA)); err != nil {
		t.Fatalf("Complete A: %v", err)
	}
	firstID := *findFileBlocks(stubC.lastRequest())[0].FileID
	if _, err := drv.Complete(ctx, providerNativeImageReq(stubB)); err != nil {
		t.Fatalf("Complete B: %v", err)
	}
	deletes := stubC.recordedDeletes()
	if len(deletes) != 1 || deletes[0].FileID != firstID {
		t.Errorf("deletes = %+v, want the LRU-evicted file %q", deletes, firstID)
	}
	if got := drv.files.len(); got != 1 {
		t.Errorf("cache len = %d, want 1 (capacity bound)", got)
	}
}

// TestProviderNative_CloseSweepsRemoteFiles — Close drains the cache
// and best-effort deletes every remote file, so a headless consumer
// who never runs a dev loop does not leak provider-side files.
func TestProviderNative_CloseSweepsRemoteFiles(t *testing.T) {
	store := openArtifactStore(t)
	stub := putTestArtifact(t, store, "s-close", "image/png", []byte("img-close"))
	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)

	ctx := withIdentity(t, context.Background(), "s-close")
	if _, err := drv.Complete(ctx, providerNativeImageReq(stub)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	fileID := *findFileBlocks(stubC.lastRequest())[0].FileID
	if err := drv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deletes := stubC.recordedDeletes()
	if len(deletes) != 1 || deletes[0].FileID != fileID {
		t.Errorf("deletes = %+v, want the Close-swept file %q", deletes, fileID)
	}
	if got := drv.files.len(); got != 0 {
		t.Errorf("cache len after Close = %d, want 0", got)
	}
	// Idempotent: second Close performs no second sweep.
	if err := drv.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := len(stubC.recordedDeletes()); got != 1 {
		t.Errorf("deletes after second Close = %d, want 1", got)
	}
}

// TestProviderNative_HeadlessDataURL — the headless reachability
// guarantee: a hand-built CompleteRequest with a sub-threshold inline
// DataURL and the ProviderNative flag uploads the decoded bytes with
// NO planner, run loop, config, artifact store, or Protocol in the
// path.
func TestProviderNative_HeadlessDataURL(t *testing.T) {
	raw := []byte("tiny-inline-image")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	stubC := newStubClient()
	drv := newDriverWithClient(stubC, bfschemas.OpenAI, nil) // no artifact store
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-headless")
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartImage, Image: &llm.ImagePart{
					DataURL: dataURL, MIME: "image/png", ProviderNative: true,
				}},
			}},
		}},
	}
	if _, err := drv.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	uploads := stubC.recordedUploads()
	if len(uploads) != 1 || string(uploads[0].File) != string(raw) {
		t.Fatalf("uploads = %d, want 1 with the decoded inline bytes", len(uploads))
	}
	blocks := findFileBlocks(stubC.lastRequest())
	if len(blocks) != 1 || blocks[0].FileID == nil {
		t.Fatalf("file blocks = %+v, want the uploaded file_id", blocks)
	}
	// Caller-request immutability (D-025 — no bleed through a shared
	// request value): the original part is untouched.
	if req.Messages[0].Content.Parts[0].Image.ProviderFileID != "" {
		t.Error("caller's ImagePart was mutated by the upload pass")
	}
}

// TestProviderNative_MissingArtifactStore_FailsLoudly — an
// artifact-backed provider_native part on a driver without
// Deps.Artifacts is a loud error naming the missing dependency.
func TestProviderNative_MissingArtifactStore_FailsLoudly(t *testing.T) {
	stubC := newStubClient()
	drv := newDriverWithClient(stubC, bfschemas.OpenAI, nil)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-nostore")
	stub := &llm.ArtifactStub{Ref: "art_missing", MIME: "image/png"}
	_, err := drv.Complete(ctx, providerNativeImageReq(stub))
	if err == nil || !strings.Contains(err.Error(), "Deps.Artifacts") {
		t.Fatalf("err = %v, want a loud missing-artifact-store failure", err)
	}
}

// TestProviderNative_MissingArtifact_FailsLoudly — an artifact ref
// that does not resolve in the calling identity's scope fails the
// call (fail-closed isolation: another session's ref is invisible).
func TestProviderNative_MissingArtifact_FailsLoudly(t *testing.T) {
	store := openArtifactStore(t)
	// Stored under session s-OTHER; fetched under s-mine.
	stub := putTestArtifact(t, store, "s-OTHER", "image/png", []byte("img"))
	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-mine")
	_, err := drv.Complete(ctx, providerNativeImageReq(stub))
	if err == nil || !strings.Contains(err.Error(), "not found in scope") {
		t.Fatalf("err = %v, want a loud not-found-in-scope failure", err)
	}
	if got := stubC.uploadCalls.Load(); got != 0 {
		t.Errorf("upload calls = %d, want 0", got)
	}
}

// TestProviderNative_PreSetFileID_SkipsUpload — a part arriving with
// ProviderFileID already set is translated as the file reference with
// no upload round-trip.
func TestProviderNative_PreSetFileID_SkipsUpload(t *testing.T) {
	stubC := newStubClient()
	drv := newDriverWithClient(stubC, bfschemas.OpenAI, nil)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-preset")
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartFile, File: &llm.FilePart{
					ProviderFileID: "file-preset-1", MIME: "application/pdf",
					Filename: "doc.pdf", ProviderNative: true, DocumentType: "pdf",
				}},
			}},
		}},
	}
	if _, err := drv.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := stubC.uploadCalls.Load(); got != 0 {
		t.Errorf("upload calls = %d, want 0 for a pre-set file id", got)
	}
	blocks := findFileBlocks(stubC.lastRequest())
	if len(blocks) != 1 || blocks[0].FileID == nil || *blocks[0].FileID != "file-preset-1" {
		t.Fatalf("file blocks = %+v, want the pre-set file id", blocks)
	}
	if blocks[0].Filename == nil || *blocks[0].Filename != "doc.pdf" {
		t.Errorf("file block filename = %v, want doc.pdf", blocks[0].Filename)
	}
}

// TestProviderNative_StreamingMultimodal — the streaming-with-
// multimodal residual (RFC §11 Q-3): a provider_native multimodal
// request with req.Stream=true uploads first, then streams content
// deltas through the existing streaming path.
func TestProviderNative_StreamingMultimodal(t *testing.T) {
	store := openArtifactStore(t)
	stub := putTestArtifact(t, store, "s-stream", "image/png", []byte("img-stream"))
	stubC := newStubClient()
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-stream")
	req := providerNativeImageReq(stub)
	req.Stream = true
	var deltas []string
	var doneSeen bool
	req.OnContent = func(delta string, done bool) {
		if done {
			doneSeen = true
			return
		}
		deltas = append(deltas, delta)
	}
	resp, err := drv.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := stubC.uploadCalls.Load(); got != 1 {
		t.Errorf("upload calls = %d, want 1 before streaming", got)
	}
	if len(deltas) == 0 || !doneSeen || resp.Content == "" {
		t.Errorf("stream broke: deltas=%d done=%v content=%q", len(deltas), doneSeen, resp.Content)
	}
	if blocks := findFileBlocks(stubC.lastRequest()); len(blocks) != 1 {
		t.Errorf("streamed request file blocks = %+v, want 1", blocks)
	}
}

// TestConcurrent_D025_ProviderFileCache — the D-025 concurrent-reuse
// contract for the file_id cache: N=128 concurrent provider_native
// Completes against ONE shared driver across 8 distinct sessions.
// Asserts no races (-race is the gate), no identity cross-bleed (each
// session's chat requests carry a file_id minted for that session),
// bounded uploads (one per session — the cache absorbs the rest), and
// goroutine-baseline restoration.
func TestConcurrent_D025_ProviderFileCache(t *testing.T) {
	store := openArtifactStore(t)
	const sessions = 8
	const N = 128

	stubs := make([]*llm.ArtifactStub, sessions)
	for s := range sessions {
		stubs[s] = putTestArtifact(t, store, fmt.Sprintf("s-%d", s), "image/png", []byte("shared-image-bytes"))
	}

	stubC := newStubClient()
	var uploadSeq atomic.Int64
	stubC.uploadHandler = func(req *bfschemas.BifrostFileUploadRequest) (*bfschemas.BifrostFileUploadResponse, *bfschemas.BifrostError) {
		id := fmt.Sprintf("file-%d", uploadSeq.Add(1))
		return &bfschemas.BifrostFileUploadResponse{ID: id, Bytes: int64(len(req.File))}, nil
	}
	drv := newDriverWithClientAndStore(stubC, bfschemas.OpenAI, nil, store)

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(N)
	for i := range N {
		s := i % sessions
		go func(i, s int) {
			defer wg.Done()
			ctx := withIdentity(t, context.Background(), fmt.Sprintf("s-%d", s))
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := drv.Complete(ctx, providerNativeImageReq(stubs[s])); err != nil {
				failures.Add(1)
				t.Errorf("Complete[%d]: %v", i, err)
				return
			}
		}(i, s)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent calls failed", failures.Load())
	}
	// One upload per session — the cache absorbed the other N-sessions
	// calls; and never fewer (no cross-session reuse).
	if got := stubC.uploadCalls.Load(); got != sessions {
		t.Errorf("upload calls = %d, want %d (one per session)", got, sessions)
	}
	// Every chat request's file_id maps back to exactly one session
	// (no bleed): the wire saw exactly `sessions` distinct file ids.
	distinctIDs := map[string]bool{}
	for _, req := range stubC.requestsSnapshot() {
		for _, b := range findFileBlocks(req) {
			if b.FileID != nil {
				distinctIDs[*b.FileID] = true
			}
		}
	}
	if len(distinctIDs) != sessions {
		t.Errorf("distinct file ids on the wire = %d, want %d (one per session)", len(distinctIDs), sessions)
	}

	if err := drv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close sweeps all cached files.
	if got := len(stubC.recordedDeletes()); got != sessions {
		t.Errorf("Close-time deletes = %d, want %d", got, sessions)
	}
	// Goroutine baseline restores (bounded real-time check, no
	// fixed sleeps — poll with deadline).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if g := runtime.NumGoroutine(); g > baseline+2 {
		t.Errorf("goroutines = %d, baseline %d — leak suspected", g, baseline)
	}
}

// TestDecodeDataURLPayload_Grammar pins the inline-payload decoder's
// fail-loud branches (non-data URI, missing comma, bad base64) and
// the URL-encoded (non-base64) passthrough.
func TestDecodeDataURLPayload_Grammar(t *testing.T) {
	if _, err := decodeDataURLPayload("https://example.com/x.png"); err == nil {
		t.Error("non-data URI must error")
	}
	if _, err := decodeDataURLPayload("data:image/png;base64"); err == nil {
		t.Error("missing comma must error")
	}
	if _, err := decodeDataURLPayload("data:image/png;base64,!!!not-base64!!!"); err == nil {
		t.Error("malformed base64 must error")
	}
	got, err := decodeDataURLPayload("data:text/plain,hello")
	if err != nil || string(got) != "hello" {
		t.Errorf("plain payload = %q / %v, want hello", got, err)
	}
}

// TestModalityForMIME_Vocabulary pins the event payload's modality
// vocabulary.
func TestModalityForMIME_Vocabulary(t *testing.T) {
	cases := map[string]string{
		"image/png":       "image",
		"audio/wav":       "audio",
		"video/mp4":       "video",
		"application/pdf": "pdf",
		"text/csv":        "file",
		"":                "file",
	}
	for mime, want := range cases {
		if got := modalityForMIME(mime); got != want {
			t.Errorf("modalityForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

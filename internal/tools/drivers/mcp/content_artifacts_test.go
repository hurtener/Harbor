package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactcontent"
)

func contentArtifactScope() artifacts.ArtifactScope {
	return artifacts.ArtifactScope{
		TenantID:  "tenant-content-test",
		UserID:    "user-content-test",
		SessionID: "session-content-test",
		TaskID:    "run-content-test",
	}
}

func newContentArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("inmem ArtifactStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// TestMCPTypedContent_MaterializesOfficialSDKBlocks proves the generic
// artifact-content seam against the pinned MCP SDK's actual content types.
// Binary blocks become individual, typed, identity-scoped artifacts in their
// original order; text and ResourceLink remain in the lowered result.
func TestMCPTypedContent_MaterializesOfficialSDKBlocks(t *testing.T) {
	t.Parallel()
	store := newContentArtifactStore(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("ResourceLink was fetched unexpectedly")
	}))
	defer remote.Close()

	lowered, err := lowerCallToolResult(&mcpsdk.CallToolResult{Content: []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "before"},
		&mcpsdk.ImageContent{
			Data:     []byte{1, 2, 3},
			MIMEType: "image/png",
			Meta:     mcpsdk.Meta{"filename": "../hero.png", "source_uri": "mcp://image"},
		},
		&mcpsdk.AudioContent{Data: []byte{4, 5}, MIMEType: "audio/wav"},
		&mcpsdk.ResourceLink{URI: remote.URL, Name: "remote"},
		&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{
			URI:      "mem://blob",
			MIMEType: "application/octet-stream",
			Blob:     []byte{6, 7, 8, 9},
			Meta:     mcpsdk.Meta{"name": "nested/clip.webm"},
		}},
		&mcpsdk.TextContent{Text: "after"},
	}})
	if err != nil {
		t.Fatalf("lowerCallToolResult: %v", err)
	}
	projected, err := artifactcontent.Materialize(context.Background(), store, contentArtifactScope(), lowered, "mcp")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	value, ok := projected.(MCPToolValue)
	if !ok {
		t.Fatalf("projected type = %T, want MCPToolValue", projected)
	}
	if value.Text != "beforeafter" {
		t.Errorf("text = %q, want beforeafter", value.Text)
	}
	if len(value.Parts) != 4 {
		t.Fatalf("parts = %d, want image/audio/link/embedded", len(value.Parts))
	}
	if value.Parts[0].Image == nil || value.Parts[0].Image.Artifact == nil || value.Parts[0].Image.Data != nil {
		t.Fatalf("image was not replaced by an artifact ref: %+v", value.Parts[0])
	}
	if value.Parts[1].Audio == nil || value.Parts[1].Audio.Artifact == nil || value.Parts[1].Audio.Data != nil {
		t.Fatalf("audio was not replaced by an artifact ref: %+v", value.Parts[1])
	}
	if value.Parts[2].Link == nil || value.Parts[2].Link.URI != remote.URL {
		t.Fatalf("ResourceLink metadata changed: %+v", value.Parts[2])
	}
	if value.Parts[3].Embedded == nil || value.Parts[3].Embedded.Artifact == nil || value.Parts[3].Embedded.Blob != nil {
		t.Fatalf("embedded blob was not replaced by an artifact ref: %+v", value.Parts[3])
	}

	refs, err := store.List(context.Background(), contentArtifactScope().Triple())
	if err != nil {
		t.Fatalf("List same session: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("same-session refs = %d, want 3", len(refs))
	}
	sort.Slice(refs, func(i, j int) bool {
		return sourceContentIndex(refs[i]) < sourceContentIndex(refs[j])
	})
	wantMIME := []string{"image/png", "audio/wav", "application/octet-stream"}
	wantSizes := []int64{3, 2, 4}
	for i, ref := range refs {
		if ref.MimeType != wantMIME[i] || ref.SizeBytes != wantSizes[i] {
			t.Errorf("ref[%d] = mime %q size %d, want %q/%d", i, ref.MimeType, ref.SizeBytes, wantMIME[i], wantSizes[i])
		}
		if ref.SHA256 == "" || ref.Filename == "" || strings.ContainsAny(ref.Filename, `/\\`) {
			t.Errorf("ref[%d] missing safe metadata: %+v", i, ref)
		}
		if ref.Source["source"] != "tool" || ref.Source["producer"] != "mcp" {
			t.Errorf("ref[%d] provenance = %#v, want canonical tool + generic mcp provenance", i, ref.Source)
		}
	}
	if refs[0].Filename != "hero.png" || refs[1].Filename != "content-002.wav" || refs[2].Filename != "clip.webm" {
		t.Errorf("sanitized/fallback filenames = %q/%q/%q, want hero.png/content-002.wav/clip.webm", refs[0].Filename, refs[1].Filename, refs[2].Filename)
	}
	if refs[0].Source["source_uri"] != "mcp://image" {
		t.Errorf("source URI metadata = %#v, want mcp://image", refs[0].Source["source_uri"])
	}
	// Replaying the same lowered result is safe: the content-addressed store
	// deduplicates it, so repeated App/result handling does not spam the
	// session manifest.
	if _, err := artifactcontent.Materialize(context.Background(), store, contentArtifactScope(), lowered, "mcp"); err != nil {
		t.Fatalf("repeat Materialize: %v", err)
	}
	repeated, err := store.List(context.Background(), contentArtifactScope().Triple())
	if err != nil {
		t.Fatalf("List after repeat: %v", err)
	}
	if len(repeated) != 3 {
		t.Fatalf("repeat added manifest refs: got %d, want 3", len(repeated))
	}
	foreignSession := contentArtifactScope()
	foreignSession.SessionID = "another-session"
	foreign, err := store.List(context.Background(), foreignSession)
	if err != nil {
		t.Fatalf("List foreign session: %v", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("foreign session saw %d refs", len(foreign))
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal projected value: %v", err)
	}
	if strings.Contains(string(encoded), `AQID`) || strings.Contains(string(encoded), `BAU`) || strings.Contains(string(encoded), `BgcICQ`) {
		t.Fatalf("projected JSON contains raw/base64 binary content: %s", encoded)
	}
}

func sourceContentIndex(ref artifacts.ArtifactRef) int {
	if n, ok := ref.Source["content_index"].(int); ok {
		return n
	}
	if n, ok := ref.Source["content_index"].(float64); ok {
		return int(n)
	}
	return -1
}

// TestMCPTypedContent_CanonicalizesMIMEParametersWithoutNamespaceSplit keeps
// the stored MIME metadata useful to consumers while proving that equivalent
// declarations still share one base-media-type namespace and content ref.
func TestMCPTypedContent_CanonicalizesMIMEParametersWithoutNamespaceSplit(t *testing.T) {
	t.Parallel()
	store := newContentArtifactStore(t)
	scope := contentArtifactScope()
	data := []byte{1, 2, 3}

	first, err := artifactcontent.Materialize(context.Background(), store, scope, MCPToolValue{
		Parts: []ContentPart{{Kind: ContentKindImage, Image: &ImageRef{
			Data: data, MIMEType: " IMAGE/PNG ; Charset=UTF-8 ",
		}}},
	}, "mcp")
	if err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	firstValue := first.(MCPToolValue)
	firstRef := firstValue.Parts[0].Image.Artifact
	if firstRef == nil {
		t.Fatal("first projection did not contain an artifact ref")
	}
	if firstRef.MIMEType != "image/png; charset=UTF-8" {
		t.Fatalf("canonical MIME = %q, want image/png; charset=UTF-8", firstRef.MIMEType)
	}

	second, err := artifactcontent.Materialize(context.Background(), store, scope, MCPToolValue{
		Parts: []ContentPart{{Kind: ContentKindImage, Image: &ImageRef{
			Data: data, MIMEType: "image/png",
		}}},
	}, "mcp")
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	secondValue := second.(MCPToolValue)
	secondRef := secondValue.Parts[0].Image.Artifact
	if secondRef == nil {
		t.Fatal("second projection did not contain an artifact ref")
	}
	if firstRef.ID != secondRef.ID {
		t.Fatalf("MIME parameter variant split content namespace: first ID %q, second ID %q", firstRef.ID, secondRef.ID)
	}
	refs, err := store.List(context.Background(), scope.Triple())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("canonical MIME variants created %d refs, want one", len(refs))
	}
}

type failingContentStore struct {
	artifacts.ArtifactStore
	err error
}

func (s failingContentStore) PutBytes(context.Context, artifacts.ArtifactScope, []byte, artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, s.err
}

// TestMCPTypedContent_FailureAndIdentityFailClosed proves storage failures,
// missing identity, cancellation, and oversized content never fall back to a
// raw planner value.
func TestMCPTypedContent_FailureAndIdentityFailClosed(t *testing.T) {
	t.Parallel()
	value := MCPToolValue{Parts: []ContentPart{{Kind: ContentKindImage, Image: &ImageRef{Data: []byte("secret"), MIMEType: "image/png"}}}}
	ctx := context.Background()
	scope := contentArtifactScope()
	store := newContentArtifactStore(t)
	forced := errors.New("forced content store failure")
	if _, err := artifactcontent.Materialize(ctx, failingContentStore{ArtifactStore: store, err: forced}, scope, value, "mcp"); !errors.Is(err, forced) {
		t.Fatalf("storage failure = %v, want forced failure", err)
	}
	if _, err := artifactcontent.Materialize(ctx, store, artifacts.ArtifactScope{}, value, "mcp"); !errors.Is(err, artifacts.ErrIdentityRequired) {
		t.Fatalf("missing identity = %v, want ErrIdentityRequired", err)
	}
	empty := MCPToolValue{Parts: []ContentPart{{Kind: ContentKindAudio, Audio: &AudioRef{Data: []byte{}, MIMEType: "audio/wav"}}}}
	if _, err := artifactcontent.Materialize(ctx, store, scope, empty, "mcp"); !errors.Is(err, artifactcontent.ErrEmptyPart) {
		t.Fatalf("empty materialization = %v, want ErrEmptyPart", err)
	}
	emptyBlob := MCPToolValue{Parts: []ContentPart{{Kind: ContentKindEmbedded, Embedded: &EmbeddedRef{Blob: []byte{}, MIMEType: "video/webm"}}}}
	if _, err := artifactcontent.Materialize(ctx, store, scope, emptyBlob, "mcp"); !errors.Is(err, artifactcontent.ErrEmptyPart) {
		t.Fatalf("empty embedded blob materialization = %v, want ErrEmptyPart", err)
	}
	invalidMIME := MCPToolValue{Parts: []ContentPart{
		{Kind: ContentKindImage, Image: &ImageRef{Data: []byte{1}, MIMEType: "image/png"}},
		{Kind: ContentKindAudio, Audio: &AudioRef{Data: []byte{2}, MIMEType: "not a media type"}},
	}}
	if _, err := artifactcontent.Materialize(ctx, store, scope, invalidMIME, "mcp"); err == nil {
		t.Fatal("invalid MIME materialization unexpectedly succeeded")
	}
	preflightRefs, err := store.List(context.Background(), scope.Triple())
	if err != nil {
		t.Fatalf("List after invalid MIME: %v", err)
	}
	if len(preflightRefs) != 0 {
		t.Fatalf("invalid MIME wrote %d preflight artifact(s), want zero", len(preflightRefs))
	}
	loweredEmptyBlob := lowerReadResourceResult(&mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "mem://empty", MIMEType: "video/webm", Blob: []byte{}}}})
	if len(loweredEmptyBlob.Parts) != 1 || loweredEmptyBlob.Parts[0].Embedded == nil || loweredEmptyBlob.Parts[0].Embedded.Blob == nil {
		t.Fatalf("lowered empty embedded blob lost its binary candidate: %+v", loweredEmptyBlob)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := artifactcontent.Materialize(cancelled, store, scope, value, "mcp"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled materialization = %v, want context.Canceled", err)
	}
	large := make([]byte, artifactcontent.DefaultMaxBytesPerPart+1)
	largeValue := MCPToolValue{Parts: []ContentPart{{Kind: ContentKindAudio, Audio: &AudioRef{Data: large, MIMEType: "audio/wav"}}}}
	if _, err := artifactcontent.Materialize(ctx, store, scope, largeValue, "mcp"); !errors.Is(err, artifactcontent.ErrTooLarge) {
		t.Fatalf("oversized materialization = %v, want ErrTooLarge", err)
	}
}

type failAfterContentStore struct {
	artifacts.ArtifactStore
	failAt int
	calls  int
	err    error
}

func (s *failAfterContentStore) PutBytes(ctx context.Context, scope artifacts.ArtifactScope, data []byte, opts artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	if s.calls >= s.failAt {
		return artifacts.ArtifactRef{}, s.err
	}
	s.calls++
	return s.ArtifactStore.PutBytes(ctx, scope, data, opts)
}

// TestMCPTypedContent_PartialStoreFailureFailsClosed records the explicit
// non-atomicity of the ArtifactStore interface: a later part can fail after
// an earlier identity-scoped write. The planner still receives no projection,
// and a retry deduplicates the already-written content.
func TestMCPTypedContent_PartialStoreFailureFailsClosed(t *testing.T) {
	t.Parallel()
	base := newContentArtifactStore(t)
	forced := errors.New("forced second-part failure")
	store := &failAfterContentStore{ArtifactStore: base, failAt: 1, err: forced}
	value := MCPToolValue{Parts: []ContentPart{
		{Kind: ContentKindImage, Image: &ImageRef{Data: []byte{1}, MIMEType: "image/png"}},
		{Kind: ContentKindAudio, Audio: &AudioRef{Data: []byte{2}, MIMEType: "audio/wav"}},
	}}
	projected, err := artifactcontent.Materialize(context.Background(), store, contentArtifactScope(), value, "mcp")
	if !errors.Is(err, forced) {
		t.Fatalf("partial storage err = %v, want forced failure", err)
	}
	if projected != nil {
		t.Fatalf("partial storage returned planner value %#v", projected)
	}
	refs, listErr := base.List(context.Background(), contentArtifactScope().Triple())
	if listErr != nil {
		t.Fatalf("List after partial failure: %v", listErr)
	}
	if len(refs) != 1 {
		t.Fatalf("partial failure left %d refs, want one already-written identity-scoped ref", len(refs))
	}
	if _, err := artifactcontent.Materialize(context.Background(), base, contentArtifactScope(), value, "mcp"); err != nil {
		t.Fatalf("retry after partial failure: %v", err)
	}
	refs, err = base.List(context.Background(), contentArtifactScope().Triple())
	if err != nil {
		t.Fatalf("List after retry: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("retry created %d refs, want two unique binary parts", len(refs))
	}
}

func TestMCPMaterializeValue_UsesIdentityAndRunScope(t *testing.T) {
	t.Parallel()
	store := newContentArtifactStore(t)
	p := &Provider{cfg: Config{ArtifactStore: store}}
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "run")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	got, err := p.materializeValue(ctx, MCPToolValue{Parts: []ContentPart{{Kind: ContentKindImage, Image: &ImageRef{Data: []byte{1}, MIMEType: "image/png"}}}}, "mcp:test:tool:image")
	if err != nil {
		t.Fatalf("materializeValue: %v", err)
	}
	if got.Parts[0].Image == nil || got.Parts[0].Image.Artifact == nil {
		t.Fatalf("missing artifact ref: %+v", got.Parts[0])
	}
	refs, err := store.List(context.Background(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})
	if err != nil || len(refs) != 1 {
		t.Fatalf("session manifest refs = %d err=%v, want 1", len(refs), err)
	}
	if refs[0].Scope.TaskID != "run" {
		t.Errorf("TaskID provenance = %q, want run", refs[0].Scope.TaskID)
	}
}

// TestProvider_TypedContentMaterializationFailureIsPermanent proves a
// successful remote CallTool is not reissued when local artifact storage
// fails. The returned ToolResult is empty so no raw binary reaches the caller.
func TestProvider_TypedContentMaterializationFailureIsPermanent(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	m := newMockServer()
	mcpsdk.AddTool(m.server, &mcpsdk.Tool{
		Name: "binary_materialization_failure",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}, func(context.Context, *mcpsdk.CallToolRequest, any) (*mcpsdk.CallToolResult, any, error) {
		m.alwaysFailCount.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.ImageContent{Data: []byte("secret"), MIMEType: "image/png"},
		}}, nil, nil
	})
	base := newContentArtifactStore(t)
	forced := errors.New("forced materialization failure")
	p, err := New(Config{
		Name:            "materialize-retry",
		URL:             "http://example.invalid",
		TransportMode:   TransportAuto,
		Bus:             bus,
		DefaultIdentity: defaultIdentity(),
		ArtifactStore:   failingContentStore{ArtifactStore: base, err: forced},
		// Zero-valued policy deliberately exercises the production defaults:
		// transport/result failures are normally retryable, but materialization
		// is terminal because the remote call has already completed.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cleanup := pairProvider(t, m, p)
	defer func() {
		_ = p.Close(context.Background())
		cleanup()
	}()

	descs, err := p.Discover(mustIdentity(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "materialize-retry_binary_materialization_failure")
	if d == nil {
		t.Fatalf("missing binary test tool; got %s", names(descs))
	}
	result, err := d.Invoke(mustIdentity(t), []byte(`{}`))
	if err == nil {
		t.Fatal("Invoke unexpectedly succeeded despite ArtifactStore failure")
	}
	if !errors.Is(err, forced) {
		t.Fatalf("Invoke error = %v, want underlying storage failure", err)
	}
	if !errors.Is(err, tools.ErrToolResultMaterialization) {
		t.Fatalf("Invoke error = %v, want materialization marker", err)
	}
	var policyErr *tools.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("Invoke error = %T %v, want PolicyError", err, err)
	}
	if policyErr.Class != tools.ErrClassPermanent {
		t.Fatalf("policy class = %q, want permanent", policyErr.Class)
	}
	if got := m.alwaysFailCount.Load(); got != 1 {
		t.Fatalf("remote CallTool attempts = %d, want exactly one", got)
	}
	if result.Value != nil || result.Meta != nil {
		t.Fatalf("failed result carried planner-visible value: %#v", result)
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("marshal failed result: %v", marshalErr)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("failed result leaked raw binary content: %s", encoded)
	}
}

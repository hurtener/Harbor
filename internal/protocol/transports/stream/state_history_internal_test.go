package stream

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestClampStateHistoryLimit_Internal(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, prototypes.DefaultStateHistoryLimit},
		{-5, prototypes.DefaultStateHistoryLimit},
		{10, 10},
		{prototypes.MaxStateHistoryLimit, prototypes.MaxStateHistoryLimit},
		{prototypes.MaxStateHistoryLimit + 1, prototypes.MaxStateHistoryLimit},
		{10_000, prototypes.MaxStateHistoryLimit},
	}
	for _, c := range cases {
		if got := clampStateHistoryLimit(c.in); got != c.want {
			t.Errorf("clampStateHistoryLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestJSONNumberToInt64_Internal(t *testing.T) {
	for _, c := range []struct {
		in   any
		n    int64
		okay bool
	}{
		{float64(4096), 4096, true},
		{int64(7), 7, true},
		{int(9), 9, true},
		{"nope", 0, false},
		{nil, 0, false},
	} {
		n, ok := jsonNumberToInt64(c.in)
		if n != c.n || ok != c.okay {
			t.Errorf("jsonNumberToInt64(%v) = (%d,%v), want (%d,%v)", c.in, n, ok, c.n, c.okay)
		}
	}
}

func TestPayloadWireValue_Internal(t *testing.T) {
	if payloadWireValue(nil) != nil {
		t.Error("nil payload should map to nil")
	}
	rm := events.RedactedMap{Data: map[string]any{"k": "v"}}
	got := payloadWireValue(rm)
	m, ok := got.(map[string]any)
	if !ok || m["k"] != "v" {
		t.Errorf("RedactedMap should surface its Data map, got %T %v", got, got)
	}
	// A non-RedactedMap SafePayload passes through unchanged.
	safe := events.AdminScopeUsedPayload{Tenant: "t"}
	if payloadWireValue(safe) == nil {
		t.Error("a typed payload should pass through, not nil")
	}
}

func TestExtractArtifactRefSeeds_Variants(t *testing.T) {
	// (a) The canonical llm.ArtifactStub / audit.ArtifactRef shape: a STRING
	// "artifact_ref" id with mime / size_bytes / hash siblings (the exact
	// JSON those types marshal to — see llm.ArtifactStub.MarshalJSON).
	v1 := map[string]any{
		"artifact_ref": "ref-a1",
		"mime":         "image/png",
		"size_bytes":   float64(65536),
		"hash":         "sha256:abc",
		"summary":      "User screenshot",
		"fetch":        map[string]any{"tool": "artifact_fetch", "id": "ref-a1"},
	}
	s1 := extractArtifactRefSeeds(v1)
	if len(s1) != 1 || s1[0].id != "ref-a1" || s1[0].mime != "image/png" || s1[0].size != 65536 || s1[0].sha != "sha256:abc" {
		t.Fatalf("variant a (ArtifactStub): %+v", s1)
	}

	// (b) The dispatch.HeavyTruncationSummary shape: tool/size_bytes/
	// truncated/preview + a STRING artifact_ref. No mime/hash.
	v2 := map[string]any{
		"tool":         "youtube_get_metadata",
		"size_bytes":   float64(40000),
		"truncated":    true,
		"preview":      "…",
		"artifact_ref": "ref-a2",
	}
	s2 := extractArtifactRefSeeds(v2)
	if len(s2) != 1 || s2[0].id != "ref-a2" || s2[0].size != 40000 {
		t.Fatalf("variant b (HeavyTruncationSummary): %+v", s2)
	}

	// (c) The PascalCase multimodal SafePayload shape (ImageMaterialized /
	// FileUploaded carry no json tags, so they marshal Go field names).
	v3 := map[string]any{
		"ArtifactRef": "ref-a3",
		"MIME":        "audio/mpeg",
		"SizeBytes":   float64(1024),
	}
	s3 := extractArtifactRefSeeds(v3)
	if len(s3) != 1 || s3[0].id != "ref-a3" || s3[0].mime != "audio/mpeg" || s3[0].size != 1024 {
		t.Fatalf("variant c (PascalCase multimodal): %+v", s3)
	}

	// (d) Nested + multi-ref: a ref nested under any key is surfaced, and
	// the same id is de-duplicated (appears in artifact_ref AND fetch.id).
	v4 := map[string]any{
		"observation": map[string]any{
			"result": map[string]any{"artifact_ref": "ref-deep", "mime": "text/plain"},
		},
	}
	s4 := extractArtifactRefSeeds(v4)
	if len(s4) != 1 || s4[0].id != "ref-deep" {
		t.Fatalf("variant d (nested): %+v", s4)
	}

	// (e) NEGATIVE: a payload whose maps carry only bare `id` keys (a tool
	// call id, a message id, the fetch hint) must yield NO refs — these are
	// NOT artifact references, and treating them as such produced bogus,
	// unroutable refs + phantom Console cards (the over-broad-walk bug).
	v5 := map[string]any{
		"tool_call": map[string]any{"id": "call_abc", "name": "search"},
		"message":   map[string]any{"id": "msg_123", "role": "assistant"},
		"items":     []any{map[string]any{"id": "item_1"}, map[string]any{"id": "item_2"}},
	}
	if got := extractArtifactRefSeeds(v5); len(got) != 0 {
		t.Fatalf("bare-id maps must NOT be treated as artifact refs, got %+v", got)
	}

	// (f) A scalar / no-ref payload yields nothing.
	if got := extractArtifactRefSeeds("scalar"); len(got) != 0 {
		t.Errorf("scalar payload should yield no seeds, got %+v", got)
	}
	if got := extractArtifactRefSeeds(map[string]any{"k": "v"}); len(got) != 0 {
		t.Errorf("no-ref map should yield no seeds, got %+v", got)
	}
}

// TestExtractArtifactRefSeeds_RealArtifactStubShape ties the extractor to
// the RUNTIME's actual heavy-output stamping type rather than a
// hand-authored fixture (§17.8 anti-rubber-stamp): it marshals a real
// llm.ArtifactStub through its own MarshalJSON, decodes to the generic map
// the durable log persists, and asserts the extractor pulls exactly the
// stub's id/mime/size/hash — and does NOT additionally pull the fetch
// hint's id.
func TestExtractArtifactRefSeeds_RealArtifactStubShape(t *testing.T) {
	stub := llm.ArtifactStub{
		Ref:       "ref-real-1",
		MIME:      "image/png",
		SizeBytes: 65536,
		Hash:      "sha256:deadbeef",
		Summary:   "a screenshot",
		Fetch:     &llm.StubFetch{Tool: "artifact_fetch", ID: "ref-real-1"},
	}
	raw, err := json.Marshal(stub)
	if err != nil {
		t.Fatalf("marshal ArtifactStub: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal to generic map: %v", err)
	}
	seeds := extractArtifactRefSeeds(m)
	if len(seeds) != 1 {
		t.Fatalf("real ArtifactStub shape must yield exactly ONE ref (not a second from fetch.id), got %d: %+v", len(seeds), seeds)
	}
	s := seeds[0]
	if s.id != "ref-real-1" || s.mime != "image/png" || s.size != 65536 || s.sha != "sha256:deadbeef" {
		t.Fatalf("extracted seed does not match the real ArtifactStub fields: %+v", s)
	}
}

func TestClassifyStateHistoryError_AllBranches(t *testing.T) {
	cases := []struct {
		err    error
		code   protoerrors.Code
		status int
	}{
		{events.ErrNoHistory, protoerrors.CodeNotFound, http.StatusNotFound},
		{events.ErrIdentityScopeRequired, protoerrors.CodeIdentityRequired, http.StatusUnauthorized},
		{events.ErrReplayUnavailable, protoerrors.CodeRuntimeError, http.StatusInternalServerError},
		{events.ErrBusClosed, protoerrors.CodeRuntimeError, http.StatusInternalServerError},
		{errors.New("boom"), protoerrors.CodeRuntimeError, http.StatusInternalServerError},
	}
	for _, c := range cases {
		code, status, msg := classifyStateHistoryError(c.err)
		if code != c.code || status != c.status || msg == "" {
			t.Errorf("classify(%v) = (%q,%d,%q), want (%q,%d,non-empty)", c.err, code, status, msg, c.code, c.status)
		}
	}
}

func TestNewStateHistoryHandler_NilDeps(t *testing.T) {
	if _, err := NewStateHistoryHandler(nil, nil); !errors.Is(err, ErrStateHistoryMisconfigured) {
		t.Errorf("nil bus: err = %v, want ErrStateHistoryMisconfigured", err)
	}
	// nil artifact store with a non-nil bus.
	if _, err := NewStateHistoryHandler(busNoHistoryInternal{}, nil); !errors.Is(err, ErrStateHistoryMisconfigured) {
		t.Errorf("nil store: err = %v, want ErrStateHistoryMisconfigured", err)
	}
	// WithStateHistoryLogger option is applied.
	h, err := NewStateHistoryHandler(busNoHistoryInternal{}, stubArtStore{}, WithStateHistoryLogger(slog.Default()))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if h.logger == nil {
		t.Error("WithStateHistoryLogger did not set the logger")
	}
	// A nil logger is ignored (default retained).
	h2, _ := NewStateHistoryHandler(busNoHistoryInternal{}, stubArtStore{}, WithStateHistoryLogger(nil))
	if h2.logger == nil {
		t.Error("nil logger option should retain the default, not clear it")
	}
}

// busNoHistoryInternal implements events.EventBus but NOT HistoryReplayer.
type busNoHistoryInternal struct{}

func (busNoHistoryInternal) Publish(_ context.Context, _ events.Event) error { return nil }
func (busNoHistoryInternal) Subscribe(_ context.Context, _ events.Filter) (events.Subscription, error) {
	return nil, nil
}
func (busNoHistoryInternal) Close(_ context.Context) error { return nil }

// stubArtStore is a no-op ArtifactStore for construction tests.
type stubArtStore struct{}

func (stubArtStore) PutBytes(_ context.Context, _ artifacts.ArtifactScope, _ []byte, _ artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, nil
}
func (stubArtStore) PutText(_ context.Context, _ artifacts.ArtifactScope, _ string, _ artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, nil
}
func (stubArtStore) Get(_ context.Context, _ artifacts.ArtifactScope, _ string) ([]byte, bool, error) {
	return nil, false, nil
}
func (stubArtStore) GetRef(_ context.Context, _ artifacts.ArtifactScope, _ string) (*artifacts.ArtifactRef, bool, error) {
	return nil, false, nil
}
func (stubArtStore) Exists(_ context.Context, _ artifacts.ArtifactScope, _ string) (bool, error) {
	return false, nil
}
func (stubArtStore) Delete(_ context.Context, _ artifacts.ArtifactScope, _ string) (bool, error) {
	return false, nil
}
func (stubArtStore) List(_ context.Context, _ artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	return nil, nil
}
func (stubArtStore) Close(_ context.Context) error { return nil }

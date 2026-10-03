package transfer_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestTransfer_GrantValidationAndReceiptRefusals(t *testing.T) {
	f := newFixture(t)
	g, _ := f.grant(t, 999)
	cases := []func(*types.ArtifactTransferGrant){
		func(g *types.ArtifactTransferGrant) { g.TransferID = "" },
		func(g *types.ArtifactTransferGrant) { g.Version = 2 },
		func(g *types.ArtifactTransferGrant) { g.SHA256 = strings.Repeat("x", 64) },
		func(g *types.ArtifactTransferGrant) { g.MimeType = strings.Repeat("x", 256) },
		func(g *types.ArtifactTransferGrant) { g.MimeType = "not a mime" },
		func(g *types.ArtifactTransferGrant) { g.ExpiresAt = g.IssuedAt.Add(16 * time.Minute) },
	}
	for _, change := range cases {
		bad := g
		change(&bad)
		if _, err := transfer.Sign(bad, f.key); !errors.Is(err, transfer.ErrInvalid) {
			t.Fatalf("invalid grant accepted: %v", err)
		}
	}
	if _, err := transfer.Sign(g, nil); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatalf("nil key=%v", err)
	}
	bad := g
	bad.KeyID = "missing"
	if _, err := f.target.Prepare(owner(t, g.Destination), bad); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("unknown key=%v", err)
	}
	bad = g
	bad.Signature = "%%%"
	if _, err := f.target.Prepare(owner(t, g.Destination), bad); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatalf("bad signature=%v", err)
	}
	if _, err := f.target.Status(t.Context(), "wrong", g.TransferID); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.target.Status(t.Context(), "import", g.TransferID); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := f.target.Revoke(t.Context(), "wrong", g.TransferID); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := f.target.Revoke(t.Context(), "import", g.TransferID); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatal(err)
	}
	ctx := owner(t, g.Destination)
	if _, err := f.target.Revoke(ctx, "import", g.TransferID); !errors.Is(err, transfer.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.target.Prepare(ctx, g); err != nil {
		t.Fatal(err)
	}
	first, err := f.target.Revoke(ctx, "import", g.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.target.Revoke(ctx, "import", g.TransferID)
	if err != nil || first != again {
		t.Fatalf("repeated revoke=%+v %v", again, err)
	}
	fence, err := transfer.NewErasureFence(f.targetCfg.State, f.targetCfg.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if err = fence.FenceSession(ctx, identity.Identity{}); err == nil {
		t.Fatal("empty erasure identity accepted")
	}
	if _, err = transfer.NewErasureFence(nil, f.targetCfg.Artifacts); err == nil {
		t.Fatal("nil state accepted")
	}
	if _, err = transfer.NewErasureFence(f.targetCfg.State, nil); err == nil {
		t.Fatal("unfenced store accepted")
	}
}

func TestTransfer_ImportHTTPRejectsMalformedRequestsBeforeBytes(t *testing.T) {
	f := newFixture(t)
	g, data := f.grant(t, 1000)
	if _, err := f.target.Prepare(owner(t, g.Destination), g); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	type scenario struct {
		name   string
		change func(*http.Request)
		status int
	}
	cases := []scenario{
		{"wrong method", func(r *http.Request) { r.Method = http.MethodGet }, 404},
		{"wrong path", func(r *http.Request) { r.URL.Path = "/other" }, 404},
		{"query substitution", func(r *http.Request) { r.URL.RawQuery = "next=other" }, 404},
		{"oversized grant", func(r *http.Request) { r.Header.Set("X-Harbor-Artifact-Transfer", strings.Repeat("a", 16*1024+1)) }, 400},
		{"invalid base64", func(r *http.Request) { r.Header.Set("X-Harbor-Artifact-Transfer", "%") }, 400},
		{"invalid json", func(r *http.Request) {
			r.Header.Set("X-Harbor-Artifact-Transfer", base64.RawURLEncoding.EncodeToString([]byte("{")))
		}, 400},
		{"trailing json", func(r *http.Request) {
			r.Header.Set("X-Harbor-Artifact-Transfer", base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), raw...), []byte(" {}")...)))
		}, 400},
		{"wrong mime", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 400},
		{"wrong declared size", func(r *http.Request) { r.ContentLength++ }, 400},
		{"wrong actual size", func(r *http.Request) { r.Body = http.NoBody; r.ContentLength = -1 }, 413},
		{"body on probe", func(r *http.Request) { r.Header.Set("X-Harbor-Artifact-Probe", "true") }, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, transfer.ImportPath, bytes.NewReader(data))
			r.Header.Set("X-Harbor-Artifact-Transfer", encoded)
			r.Header.Set("Content-Type", "application/octet-stream")
			tc.change(r)
			w := httptest.NewRecorder()
			f.target.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want%d", w.Code, tc.status)
			}
		})
	}
	if got, err := f.target.Status(owner(t, g.Destination), "import", g.TransferID); err != nil || got.State != "admitted" {
		t.Fatalf("malformed request changed admission=%+v %v", got, err)
	}
}

func TestTransfer_MaterializeValidationAndClosedStorage(t *testing.T) {
	f := newFixture(t)
	g, _ := f.grant(t, 1001)
	r, b := answerRequest()
	ctx := owner(t, g.Source)
	if _, err := f.source.MaterializeAnswer(context.Background(), r, b, 0); !errors.Is(err, transfer.ErrUnauthorized) {
		t.Fatal(err)
	}
	bad := r
	bad.RequestID = ""
	if _, err := f.source.MaterializeAnswer(ctx, bad, b, 0); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatal(err)
	}
	bad = r
	bad.TurnVersion = 0
	if _, err := f.source.MaterializeAnswer(ctx, bad, b, 0); !errors.Is(err, transfer.ErrInvalid) {
		t.Fatal(err)
	}
	bad = r
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := f.source.MaterializeAnswer(ctx, bad, b, 0); !errors.Is(err, transfer.ErrConflict) {
		t.Fatal(err)
	}
	if err := f.sourceCfg.State.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.MaterializeAnswer(ctx, r, b, 0); err == nil {
		t.Fatal("closed state acknowledged export")
	}
}

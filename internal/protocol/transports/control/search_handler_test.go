package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// stubSearch is a minimal SearchSurface used to pin the search_handler
// wiring path. Production tests of the search subsystem live in
// internal/search/... and internal/protocol/search_test.go.
type stubSearch struct {
	resp *types.SearchResponse
	err  error
}

func (s *stubSearch) Dispatch(_ context.Context, _ methods.Method, _ *types.SearchRequest) (*types.SearchResponse, error) {
	return s.resp, s.err
}

func newSearchHandler(t *testing.T, surf control.SearchSurface) http.Handler {
	t.Helper()
	// Build a real ControlSurface so search-handler tests share the
	// same drivers as the task-control suite. Search dispatching
	// bypasses the task-control surface — but it is still mandatory
	// at the Handler boundary (per the constructor contract).
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs, control.WithSearchSurface(surf))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	return mux
}

func TestSearchHandler_HappyPath_DispatchesToSearchSurface(t *testing.T) {
	t.Parallel()
	resp := &types.SearchResponse{
		Rows: []types.SearchResultRow{
			{Index: types.SearchIndexSessions, ID: "s1", TenantID: "t1"},
		},
		Page:            1,
		PageSize:        20,
		PageCount:       1,
		TotalCount:      1,
		HasMore:         false,
		ProtocolVersion: "0.1.0",
	}
	srv := httptest.NewServer(newSearchHandler(t, &stubSearch{resp: resp}))
	defer srv.Close()

	body, _ := json.Marshal(types.SearchRequest{Query: "hello"})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/search.sessions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(httpResp.Body)
		t.Fatalf("status: got %d, want 200, body=%s", httpResp.StatusCode, bodyBytes)
	}
	var got types.SearchResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 1 || got.Rows[0].ID != "s1" {
		t.Errorf("rows: got %v, want [{ID:s1}]", got.Rows)
	}
}

func TestSearchHandler_MappedErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		wantHTTP int
	}{
		{
			name:     "identity_required → 401",
			err:      protoerrors.Newf(protoerrors.CodeIdentityRequired, "missing identity"),
			wantHTTP: http.StatusUnauthorized,
		},
		{
			name:     "scope_mismatch → 403",
			err:      protoerrors.Newf(protoerrors.CodeScopeMismatch, "cross-tenant"),
			wantHTTP: http.StatusForbidden,
		},
		{
			name:     "invalid_request → 400",
			err:      protoerrors.Newf(protoerrors.CodeInvalidRequest, "bad page"),
			wantHTTP: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(newSearchHandler(t, &stubSearch{err: tc.err}))
			defer srv.Close()
			body, _ := json.Marshal(types.SearchRequest{})
			r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/search.tasks", bytes.NewReader(body))
			httpResp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer httpResp.Body.Close()
			if httpResp.StatusCode != tc.wantHTTP {
				bodyBytes, _ := io.ReadAll(httpResp.Body)
				t.Fatalf("status: got %d, want %d, body=%s", httpResp.StatusCode, tc.wantHTTP, bodyBytes)
			}
		})
	}
}

func TestSearchHandler_MalformedJSON_400(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newSearchHandler(t, &stubSearch{}))
	defer srv.Close()
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/search.events", bytes.NewReader([]byte("not json")))
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body: got %d, want 400", httpResp.StatusCode)
	}
}

func TestSearchHandler_WithoutSearchSurface_ReturnsUnknownMethod(t *testing.T) {
	t.Parallel()
	// Build a handler WITHOUT WithSearchSurface — a search call should
	// fall through to the unknown-method path (404).
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	h, err := control.NewHandler(cs)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body, _ := json.Marshal(types.SearchRequest{})
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/search.sessions", bytes.NewReader(body))
	httpResp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusNotFound {
		bodyBytes, _ := io.ReadAll(httpResp.Body)
		t.Fatalf("status: got %d, want 404, body=%s", httpResp.StatusCode, bodyBytes)
	}
}

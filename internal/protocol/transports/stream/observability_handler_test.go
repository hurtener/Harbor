package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/observability/protocol"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
)

// observabilityHandlerID is a documented dummy identity triple — no
// secrets.
var observabilityHandlerID = identityTriple{TenantID: "t-ob", UserID: "u-ob", SessionID: "s-ob"}

// identityTriple mirrors identity.Identity fields without importing the
// identity package at every call site.
type identityTriple struct {
	TenantID  string
	UserID    string
	SessionID string
}

// obsFakeQuerier is the deterministic Querier stand-in for the handler
// tests (the integration tests drive the real rollup store).
type obsFakeQuerier struct {
	rows []rollups.Row
	err  error
}

func (f *obsFakeQuerier) Query(_ context.Context, _ rollups.Query) (rollups.Result, error) {
	if f.err != nil {
		return rollups.Result{}, f.err
	}
	return rollups.Result{Rows: f.rows, NextCursor: ""}, nil
}

// obsFakeQuality is the deterministic QualitySource stand-in.
type obsFakeQuality struct {
	state rollups.State
}

func (f *obsFakeQuality) Quality(_ context.Context) (rollups.Quality, error) {
	return rollups.Quality{
		State:     f.state,
		Watermark: 42,
	}, nil
}

func newObservabilityHandler(t *testing.T, q *obsFakeQuerier, ql *obsFakeQuality, scoped bool) *stream.ObservabilityHandler {
	t.Helper()
	scope := func(ctx context.Context) bool { return scoped }
	svc, err := protocol.NewService(q, ql, scope,
		func(context.Context, events.Event) error { return nil },
		patterns.New())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewObservabilityHandler(svc)
	if err != nil {
		t.Fatalf("NewObservabilityHandler: %v", err)
	}
	return h
}

// doObsRequest issues a POST /v1/observability/query against the handler
// with the documented identity triple on the carrier headers.
func doObsRequest(t *testing.T, h http.Handler, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/observability/query", strings.NewReader(body))
	req.Header.Set(stream.HeaderTenant, observabilityHandlerID.TenantID)
	req.Header.Set(stream.HeaderUser, observabilityHandlerID.UserID)
	req.Header.Set(stream.HeaderSession, observabilityHandlerID.SessionID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestNewObservabilityHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewObservabilityHandler(nil); err == nil {
		t.Fatal("NewObservabilityHandler(nil) succeeded, want ErrObservabilityMisconfigured")
	}
}

func TestObservabilityHandler_Query_ProjectsRowsAndQuality(t *testing.T) {
	from := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	h := newObservabilityHandler(t, &obsFakeQuerier{rows: []rollups.Row{{
		BucketStart: from,
		Dimensions:  rollups.DimensionValues{rollups.DimensionUser: "u-ob"},
		Measures: map[rollups.Measure]rollups.MeasureValue{
			rollups.MeasureLLMCompletions: {N: 7, Scale: 1},
			rollups.MeasureLLMCostMicros:  {N: 2_500_000, Scale: 1_000_000},
		},
	}}}, &obsFakeQuality{state: rollups.StateCurrent}, false)
	code, body := doObsRequest(t, h,
		`{"from":"2026-05-19T09:00:00Z","to":"2026-05-19T10:00:00Z","bucket":"hour","measures":["llm_completions","llm_cost_micros"],"limit":100}`)
	if code != http.StatusOK {
		t.Fatalf("query status = %d, body=%s", code, body)
	}
	var out struct {
		Rows []struct {
			BucketStart string                    `json:"bucket_start"`
			Dimensions  map[string]string         `json:"dimensions"`
			Measures    map[string]map[string]any `json:"measures"`
		} `json:"rows"`
		Quality struct {
			State     string `json:"state"`
			Watermark uint64 `json:"watermark"`
			Coverage  string `json:"coverage"`
		} `json:"quality"`
		ProtocolVersion string `json:"protocol_version"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(out.Rows))
	}
	// Exact integer/decimal values ride verbatim: N + the fixed scale.
	completions := out.Rows[0].Measures["llm_completions"]
	if completions["n"] != float64(7) || completions["scale"] != float64(1) {
		t.Errorf("llm_completions value = %v, want n=7 scale=1", completions)
	}
	cost := out.Rows[0].Measures["llm_cost_micros"]
	if cost["n"] != float64(2_500_000) || cost["scale"] != float64(1_000_000) {
		t.Errorf("llm_cost_micros value = %v, want n=2500000 scale=1000000", cost)
	}
	// The mandatory freshness block rides verbatim.
	if out.Quality.State != "current" || out.Quality.Watermark != 42 {
		t.Errorf("quality = %+v, want current/42", out.Quality)
	}
	if out.Quality.Coverage == "" {
		t.Error("coverage must be present (retention quality is mandatory)")
	}
	if out.ProtocolVersion == "" {
		t.Error("protocol_version is empty")
	}
}

func TestObservabilityHandler_Query_MissingIdentityIs401(t *testing.T) {
	h := newObservabilityHandler(t, &obsFakeQuerier{}, &obsFakeQuality{state: rollups.StateCurrent}, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/observability/query",
		strings.NewReader(`{"from":"2026-05-19T09:00:00Z","to":"2026-05-19T10:00:00Z","bucket":"hour","measures":["llm_completions"],"limit":100}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing identity = %d, want 401; body=%s", rec.Code, rec.Body.Bytes())
	}
}

func TestObservabilityHandler_Query_CrossTenantFilterRequiresScope(t *testing.T) {
	h := newObservabilityHandler(t, &obsFakeQuerier{}, &obsFakeQuality{state: rollups.StateCurrent}, false)
	code, body := doObsRequest(t, h,
		`{"from":"2026-05-19T09:00:00Z","to":"2026-05-19T10:00:00Z","bucket":"hour","measures":["llm_completions"],"limit":100,"filters":{"tenant_ids":["t-other"]}}`)
	if code != http.StatusForbidden {
		t.Fatalf("cross-tenant query without scope = %d, want 403; body=%s", code, body)
	}
}

func TestObservabilityHandler_Query_BudgetExceededMapsTyped(t *testing.T) {
	h := newObservabilityHandler(t, &obsFakeQuerier{err: protocol.ErrBudgetExceeded}, &obsFakeQuality{state: rollups.StateCurrent}, false)
	code, body := doObsRequest(t, h,
		`{"from":"2026-05-19T09:00:00Z","to":"2026-05-19T10:00:00Z","bucket":"hour","measures":["llm_completions"],"limit":100}`)
	if code != http.StatusBadRequest {
		t.Fatalf("budget exceeded = %d, want 400; body=%s", code, body)
	}
	if !strings.Contains(string(body), `"code":"query_budget_exceeded"`) {
		t.Errorf("body missing the typed query_budget_exceeded code: %s", body)
	}
}

func TestObservabilityHandler_Query_BadCursorMapsTyped(t *testing.T) {
	h := newObservabilityHandler(t, &obsFakeQuerier{err: protocol.ErrBadCursor}, &obsFakeQuality{state: rollups.StateCurrent}, false)
	code, body := doObsRequest(t, h,
		`{"from":"2026-05-19T09:00:00Z","to":"2026-05-19T10:00:00Z","bucket":"hour","measures":["llm_completions"],"limit":100,"cursor":"stale"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400; body=%s", code, body)
	}
	if !strings.Contains(string(body), `"code":"invalid_cursor"`) {
		t.Errorf("body missing the typed invalid_cursor code: %s", body)
	}
}

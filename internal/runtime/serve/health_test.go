package serve

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
)

// healthReportingBus embeds the EventBus interface (nil) and adds only the
// PersistenceHealthReporter capability the handler consults.
type healthReportingBus struct {
	events.EventBus
	err error
}

func (b healthReportingBus) PersistenceHealthy() error { return b.err }

func TestHealthHandler_ReflectsPersistenceHealth(t *testing.T) {
	cases := []struct {
		name       string
		bus        events.EventBus
		wantStatus int
		wantBody   string
	}{
		{"nil bus", nil, http.StatusOK, `{"status":"ok"}`},
		{"bus without capability", struct{ events.EventBus }{}, http.StatusOK, `{"status":"ok"}`},
		{"healthy persistence", healthReportingBus{}, http.StatusOK, `{"status":"ok"}`},
		{"stalled persistence", healthReportingBus{err: errors.New("pending")}, http.StatusServiceUnavailable, unhealthyBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			HealthHandler(tc.bus, `{"status":"ok"}`, nil)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tc.wantStatus || rec.Body.String() != tc.wantBody {
				t.Fatalf("status=%d body=%q, want %d %q", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}
		})
	}
}

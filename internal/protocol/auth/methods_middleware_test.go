package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

type unreadableMethodBody struct{}

func (unreadableMethodBody) Read([]byte) (int, error) { panic("method gate read the request body") }
func (unreadableMethodBody) Close() error             { return nil }

func TestMethodMiddleware_CanonicalRoutesBeforeBodyRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verb    string
		path    string
		allowed methods.Method
		want    int
	}{
		{"control", http.MethodPost, "/v1/control/start", methods.MethodStart, http.StatusNoContent},
		{"control denied", http.MethodPost, "/v1/control/cancel", methods.MethodStart, http.StatusForbidden},
		{"dotted control", http.MethodPost, "/v1/control/sessions.turns.list", methods.MethodSessionTurnsList, http.StatusNoContent},
		{"direct", http.MethodPost, "/v1/sessions/list", methods.MethodSessionsList, http.StatusNoContent},
		{"nested direct", http.MethodPost, "/v1/sessions/turns/list", methods.MethodSessionTurnsList, http.StatusNoContent},
		{"direct denied", http.MethodPost, "/v1/sessions/turns/get", methods.MethodSessionTurnsList, http.StatusForbidden},
		{"sse", http.MethodGet, "/v1/events?session=other", methods.MethodEventsSubscribe, http.StatusNoContent},
		{"sse denied", http.MethodGet, "/v1/events", methods.MethodEventsList, http.StatusForbidden},
		{"aggregate", http.MethodPost, "/v1/events/aggregate", methods.MethodEventsAggregate, http.StatusNoContent},
		{"artifact bytes", http.MethodPost, "/v1/control/artifacts.get", methods.MethodArtifactsGet, http.StatusNoContent},
		{"artifact write denied", http.MethodPost, "/v1/control/artifacts.put", methods.MethodArtifactsGet, http.StatusForbidden},
		{"unregistered download", http.MethodGet, "/v1/artifacts/artifact-a/download", methods.MethodArtifactsGet, http.StatusForbidden},
		{"unknown", http.MethodPost, "/v1/future/read", methods.MethodStart, http.StatusForbidden},
		{"unknown control", http.MethodPost, "/v1/control/future.read", methods.MethodStart, http.StatusForbidden},
		{"noncanonical alias", http.MethodPost, "/v1/control/control.start", methods.MethodStart, http.StatusForbidden},
		{"mixed dotted path alias", http.MethodPost, "/v1/sessions.turns/list", methods.MethodSessionTurnsList, http.StatusForbidden},
		{"trailing slash", http.MethodPost, "/v1/control/start/", methods.MethodStart, http.StatusForbidden},
		{"wrong verb", http.MethodGet, "/v1/control/start", methods.MethodStart, http.StatusForbidden},
		{"method in body only", http.MethodPost, "/v1/control", methods.MethodStart, http.StatusForbidden},
		{"unmapped route", http.MethodPost, "/", methods.MethodStart, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := auth.MethodMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(tc.verb, tc.path, nil)
			req.Body = unreadableMethodBody{}
			req = req.WithContext(auth.WithMethodReach(req.Context(), []methods.Method{tc.allowed}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want || called != (tc.want == http.StatusNoContent) {
				t.Fatalf("status=%d called=%v; want %d", rec.Code, called, tc.want)
			}
			if tc.want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "scope_mismatch") {
				t.Fatalf("missing canonical denial code: %s", rec.Body.String())
			}
		})
	}
}

func TestMethodMiddleware_ExhaustiveControlAndDirectMapping(t *testing.T) {
	for _, method := range methods.Methods() {
		paths := []string{"/v1/control/" + string(method)}
		// The control namespace uses /v1/control/{full-canonical-name};
		// control.receipt must never invent a second /v1/control/receipt alias.
		if strings.Contains(string(method), ".") && !strings.HasPrefix(string(method), "control.") {
			paths = append(paths, "/v1/"+strings.ReplaceAll(string(method), ".", "/"))
		}
		for _, path := range paths {
			handler := auth.MethodMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req = req.WithContext(auth.WithMethodReach(req.Context(), []methods.Method{method}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("canonical route %q mapped incorrectly: %d", path, rec.Code)
			}
		}
	}
}

func TestMethodMiddleware_UnknownRouteCompatibilityRequiresAbsentClaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"absent preserves next", context.Background(), http.StatusTeapot},
		{"explicit empty denies", auth.WithMethodReach(context.Background(), []methods.Method{}), http.StatusForbidden},
		{"nonempty denies", auth.WithMethodReach(context.Background(), []methods.Method{methods.MethodStart}), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := auth.MethodMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			}))
			req := httptest.NewRequest(http.MethodPost, "/unknown", nil).WithContext(tc.ctx)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestMethodMiddleware_RejectsConflictingControlPathValue(t *testing.T) {
	handler := auth.MethodMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("conflicting dispatcher method reached handler")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/control/start", nil)
	req.SetPathValue("method", string(methods.MethodCancel))
	req = req.WithContext(auth.WithMethodReach(req.Context(), []methods.Method{methods.MethodStart}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

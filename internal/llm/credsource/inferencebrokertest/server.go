// Package inferencebrokertest ships the committed REFERENCE implementation
// of the inference-plane credential-pull contract: an httptest server that
// serves an LLM provider's API key to the broker-pull [InferenceKeySource].
//
// It exists so Harbor's own tests exercise a fixture derived from the
// canonical contract (CLAUDE.md §17.8 — the fixture server IS the reference
// implementation, not a hand-authored guess) AND so a coordinator author has
// an executable spec to build against. It is NOT a production coordinator: a
// real coordinator mints per-runtime provider keys from a vault; this fixture
// serves a fixed (rotatable) key and asserts the Bearer token.
//
// The served contract:
//
//	GET <url>
//	Authorization: Bearer <expected token>
//	Accept: application/json
//	→ 200 {"format_version":1,"api_key":...,"expires_in":N}
//
// The fixture asserts the method (GET), the Bearer credential (401 on
// mismatch), and serves the current key; [FixtureBroker.Rotate] swaps the
// served key (the rotation leg), [FixtureBroker.SetPosture] drives the
// failure legs (down / malformed / unauthorized / bad-version / redirect).
package inferencebrokertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Posture selects the fixture's response behavior.
type Posture string

const (
	// PostureServe returns 200 with the current key (default).
	PostureServe Posture = "serve"
	// PostureDown returns 503 (a coordinator outage — fail-loud leg).
	PostureDown Posture = "down"
	// PostureMalformed returns 200 with a non-parseable body.
	PostureMalformed Posture = "malformed"
	// PostureUnauthorized returns 401 (bad / rotated service token).
	PostureUnauthorized Posture = "unauthorized"
	// PostureBadVersion returns 200 with an unsupported format_version.
	PostureBadVersion Posture = "bad-version"
	// PostureRedirect returns 302 pointing back at the endpoint — the source
	// must refuse to follow it (a credential endpoint that redirects is a
	// fault, not a hop to follow).
	PostureRedirect Posture = "redirect"
)

// FixtureBroker is the reference coordinator provider-key endpoint.
type FixtureBroker struct {
	server    *httptest.Server
	authToken string

	mu           sync.Mutex
	apiKey       string
	expiresIn    int
	posture      Posture
	hits         int
	authFailures int
	delay        time.Duration
}

// New starts a fixture broker that expects `Authorization: Bearer authToken`
// and serves the given provider key with a 1h expiry. Registers cleanup on t.
func New(t testing.TB, authToken, apiKey string) *FixtureBroker {
	return newFixture(t, authToken, apiKey, 0)
}

// NewSlow is [New] with an artificial per-request delay — the single-flight
// stress leg piles a concurrent burst onto one slow in-flight pull.
func NewSlow(t testing.TB, authToken, apiKey string, delay time.Duration) *FixtureBroker {
	return newFixture(t, authToken, apiKey, delay)
}

func newFixture(t testing.TB, authToken, apiKey string, delay time.Duration) *FixtureBroker {
	t.Helper()
	f := &FixtureBroker{
		authToken: authToken,
		apiKey:    apiKey,
		expiresIn: 3600,
		posture:   PostureServe,
		delay:     delay,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// URL is the credential endpoint the source pulls from.
func (f *FixtureBroker) URL() string { return f.server.URL + "/provider-key" }

// Rotate swaps the served key — the rotation leg: a post-TTL refresh picks
// the new one up without a runtime restart.
func (f *FixtureBroker) Rotate(apiKey string) {
	f.mu.Lock()
	f.apiKey = apiKey
	f.mu.Unlock()
}

// SetPosture drives the failure / recovery legs.
func (f *FixtureBroker) SetPosture(p Posture) {
	f.mu.Lock()
	f.posture = p
	f.mu.Unlock()
}

// SetExpiresIn overrides the advertised expiry (seconds; 0 = none).
func (f *FixtureBroker) SetExpiresIn(secs int) {
	f.mu.Lock()
	f.expiresIn = secs
	f.mu.Unlock()
}

// Hits is the total number of requests reaching the handler (single-flight /
// cache assertions count these).
func (f *FixtureBroker) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// AuthFailures is the number of requests rejected for a bad Bearer token.
func (f *FixtureBroker) AuthFailures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authFailures
}

func (f *FixtureBroker) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	posture := f.posture
	apiKey, expiresIn := f.apiKey, f.expiresIn
	delay := f.delay
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.authToken {
		f.mu.Lock()
		f.authFailures++
		f.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch posture {
	case PostureDown:
		http.Error(w, "coordinator down", http.StatusServiceUnavailable)
	case PostureRedirect:
		// Redirect back to the same (fixed) endpoint path. The target is a
		// literal, never request-derived (gosec G710).
		http.Redirect(w, r, "/provider-key", http.StatusFound)
	case PostureUnauthorized:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case PostureMalformed:
		w.Header().Set("Content-Type", "application/json")
		writeBody(w, []byte(`{"format_version":1,"api_key":`)) // truncated → parse error
	case PostureBadVersion:
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"format_version": 999, "api_key": apiKey})
	default:
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"format_version": 1, "api_key": apiKey, "expires_in": expiresIn})
	}
}

// writeJSON / writeBody are best-effort response writers — a write error on
// an httptest connection is nothing the fixture can recover from.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	writeBody(w, b)
}

func writeBody(w http.ResponseWriter, b []byte) {
	if _, err := w.Write(b); err != nil {
		return
	}
}

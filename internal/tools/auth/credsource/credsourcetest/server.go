// Package credsourcetest ships the committed REFERENCE implementation of
// the coordinator credential-fetch contract: an httptest server
// that serves an OAuth provider's client credential to the `remote`
// credential source.
//
// It exists so Harbor's own tests exercise a fixture derived from the
// canonical contract (CLAUDE.md §17.8 — the fixture server IS the
// reference implementation) AND so a coordinator author has an executable
// spec to build against. It is NOT a production coordinator: a real
// coordinator mints per-runtime credentials from a vault; this fixture
// serves a fixed (rotatable) credential and asserts the Bearer token.
//
// The served contract:
//
//	GET <url>
//	Authorization: Bearer <expected token>
//	Accept: application/json
//	→ 200 {"format_version":1,"client_id":...,"client_secret":...,"expires_in":N}
//
// The fixture asserts the method (GET), the Bearer credential (401 on
// mismatch), and serves the current credential; [FixtureServer.Rotate]
// swaps the served credential (the rotation leg), [FixtureServer.SetPosture]
// drives the failure legs (unreachable / non-200 / malformed).
package credsourcetest

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
	// PostureServe returns 200 with the current credential (default).
	PostureServe Posture = "serve"
	// PostureDown returns 503 (a coordinator outage — fail-loud leg).
	PostureDown Posture = "down"
	// PostureMalformed returns 200 with a non-parseable body.
	PostureMalformed Posture = "malformed"
	// PostureUnauthorized returns 401 (bad / rotated service token).
	PostureUnauthorized Posture = "unauthorized"
	// PostureBadVersion returns 200 with an unsupported format_version
	// (the strict-parse contract-version leg).
	PostureBadVersion Posture = "bad-version"
	// PostureRedirect returns 302 pointing back at the endpoint — the
	// remote source must refuse to follow it (a credential endpoint that
	// redirects is a fault, not a hop to follow).
	PostureRedirect Posture = "redirect"
)

// FixtureServer is the reference coordinator credential endpoint.
type FixtureServer struct {
	server    *httptest.Server
	authToken string

	mu           sync.Mutex
	clientID     string
	clientSecret string
	expiresIn    int
	posture      Posture
	hits         int
	authFailures int
	delay        time.Duration
}

// New starts a fixture credential server that expects `Authorization:
// Bearer authToken` and serves the given client credential with a 1h
// expiry. It registers cleanup on t.
func New(t testing.TB, authToken, clientID, clientSecret string) *FixtureServer {
	return newFixture(t, authToken, clientID, clientSecret, 0)
}

// NewSlow is [New] with an artificial per-request delay — the single-
// flight stress leg piles a concurrent burst onto one slow in-flight
// fetch.
func NewSlow(t testing.TB, authToken, clientID, clientSecret string, delay time.Duration) *FixtureServer {
	return newFixture(t, authToken, clientID, clientSecret, delay)
}

func newFixture(t testing.TB, authToken, clientID, clientSecret string, delay time.Duration) *FixtureServer {
	t.Helper()
	f := &FixtureServer{
		authToken:    authToken,
		clientID:     clientID,
		clientSecret: clientSecret,
		expiresIn:    3600,
		posture:      PostureServe,
		delay:        delay,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// URL is the credential endpoint the `remote` source fetches from.
func (f *FixtureServer) URL() string { return f.server.URL + "/broker-credential" }

// Rotate swaps the served credential — the rotation leg: a post-TTL fetch
// picks the new one up without a runtime restart.
func (f *FixtureServer) Rotate(clientID, clientSecret string) {
	f.mu.Lock()
	f.clientID = clientID
	f.clientSecret = clientSecret
	f.mu.Unlock()
}

// SetPosture drives the failure / recovery legs.
func (f *FixtureServer) SetPosture(p Posture) {
	f.mu.Lock()
	f.posture = p
	f.mu.Unlock()
}

// SetExpiresIn overrides the advertised expiry (seconds; 0 = none).
func (f *FixtureServer) SetExpiresIn(secs int) {
	f.mu.Lock()
	f.expiresIn = secs
	f.mu.Unlock()
}

// Hits is the total number of requests reaching the handler (the
// single-flight / cache assertions count these).
func (f *FixtureServer) Hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// AuthFailures is the number of requests rejected for a bad Bearer token.
func (f *FixtureServer) AuthFailures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authFailures
}

func (f *FixtureServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits++
	posture := f.posture
	clientID, clientSecret, expiresIn := f.clientID, f.clientSecret, f.expiresIn
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
		return
	case PostureRedirect:
		// Redirect back to the same (fixed) endpoint path. A client that
		// followed it would loop; the remote source's CheckRedirect
		// refuses hop 1. The target is a literal, never request-derived
		// (gosec G710).
		http.Redirect(w, r, "/broker-credential", http.StatusFound)
		return
	case PostureUnauthorized:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case PostureMalformed:
		w.Header().Set("Content-Type", "application/json")
		writeBody(w, []byte(`{"format_version":1,"client_id":`)) // truncated → parse error
		return
	case PostureBadVersion:
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{
			"format_version": 999,
			"client_id":      clientID,
			"client_secret":  clientSecret,
		})
		return
	default:
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{
			"format_version": 1,
			"client_id":      clientID,
			"client_secret":  clientSecret,
			"expires_in":     expiresIn,
		})
	}
}

// writeJSON / writeBody are best-effort response writers — a write error
// on an httptest connection is nothing the fixture can recover from, so
// the error is deliberately dropped (satisfying errcheck's check-blank).
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

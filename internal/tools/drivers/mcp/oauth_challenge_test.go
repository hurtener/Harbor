package mcp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseWWWAuthenticate_Table(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cases := []struct {
		name     string
		header   string
		wantOK   bool
		wantURL  string
		wantReal string
	}{
		{
			name:    "bearer with resource_metadata",
			header:  `Bearer resource_metadata="https://resource.example.com/.well-known/oauth-protected-resource", error="invalid_token"`,
			wantOK:  true,
			wantURL: "https://resource.example.com/.well-known/oauth-protected-resource",
		},
		{
			name:     "bearer with realm and resource_metadata (comma inside quotes)",
			header:   `Bearer realm="mcp", resource_metadata="https://r.example.com/.well-known/oauth-protected-resource"`,
			wantOK:   true,
			wantURL:  "https://r.example.com/.well-known/oauth-protected-resource",
			wantReal: "mcp",
		},
		{
			name:   "bearer without resource_metadata",
			header: `Bearer error="invalid_token"`,
			wantOK: true,
		},
		{
			name:   "non-bearer scheme",
			header: `Basic realm="x"`,
			wantOK: false,
		},
		{
			name:   "empty header",
			header: ``,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch, ok := parseWWWAuthenticate(c.header, now)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if ch.ResourceMetadataURL != c.wantURL {
				t.Errorf("resource_metadata = %q, want %q", ch.ResourceMetadataURL, c.wantURL)
			}
			if ch.Realm != c.wantReal {
				t.Errorf("realm = %q, want %q", ch.Realm, c.wantReal)
			}
			if !ch.CapturedAt.Equal(now) {
				t.Errorf("captured_at = %v, want %v", ch.CapturedAt, now)
			}
		})
	}
}

// The captured challenge fixture matches the committed §17.8 spec-derived line.
func TestParseWWWAuthenticate_FromSpecFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "auth", "testdata", "oauthdiscovery", "www_authenticate_challenge.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	line = strings.TrimPrefix(line, "WWW-Authenticate:")
	ch, ok := parseWWWAuthenticate(line, time.Now())
	if !ok {
		t.Fatalf("spec fixture challenge did not parse as Bearer")
	}
	if !strings.HasSuffix(ch.ResourceMetadataURL, "/.well-known/oauth-protected-resource") {
		t.Errorf("resource_metadata = %q", ch.ResourceMetadataURL)
	}
}

// TestParseWWWAuthenticate_InsufficientScope_FromSpecFixture proves the
// error/scope params parse off the committed RFC 6750 §3.1 fixture — a
// wrong-field mutation of the fixture fails here (§17.8).
func TestParseWWWAuthenticate_InsufficientScope_FromSpecFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "auth", "testdata", "oauthdiscovery", "www_authenticate_insufficient_scope.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "WWW-Authenticate:"))
	ch, ok := parseWWWAuthenticate(line, time.Now())
	if !ok {
		t.Fatalf("insufficient_scope fixture did not parse as Bearer")
	}
	if ch.Error != "insufficient_scope" {
		t.Errorf("error = %q, want insufficient_scope", ch.Error)
	}
	if got := splitScopeParam(ch.Scope); len(got) != 2 || got[0] != "read:calendar" || got[1] != "write:calendar" {
		t.Errorf("scope = %v, want [read:calendar write:calendar]", got)
	}
}

// challengeCapturingTransport invokes the callback on a 401 + WWW-Authenticate,
// records the challenge, and NEVER alters the response/error the caller sees.
func TestChallengeCapturingTransport_CapturesAndPreservesSemantics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://r.example.com/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	var (
		mu       sync.Mutex
		captured []AuthChallenge
	)
	client := &http.Client{Transport: &challengeCapturingTransport{
		base: http.DefaultTransport,
		onChallenge: func(ch AuthChallenge) {
			mu.Lock()
			captured = append(captured, ch)
			mu.Unlock()
		},
	}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Error semantics unchanged: the caller still sees the 401.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (capture must not alter response)", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("captured %d challenges, want 1", len(captured))
	}
	if captured[0].ResourceMetadataURL != "https://r.example.com/.well-known/oauth-protected-resource" {
		t.Errorf("captured resource_metadata = %q", captured[0].ResourceMetadataURL)
	}
}

func TestChallengeCapturingTransport_NoChallengeOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var count int
	var mu sync.Mutex
	client := &http.Client{Transport: &challengeCapturingTransport{
		base:        http.DefaultTransport,
		onChallenge: func(AuthChallenge) { mu.Lock(); count++; mu.Unlock() },
	}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("callback fired %d times on a 200, want 0", count)
	}
}

// A 401 without a WWW-Authenticate header does not fire the callback.
func TestChallengeCapturingTransport_401NoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	var count int
	var mu sync.Mutex
	client := &http.Client{Transport: &challengeCapturingTransport{
		base:        http.DefaultTransport,
		onChallenge: func(AuthChallenge) { mu.Lock(); count++; mu.Unlock() },
	}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("callback fired on a 401 with no WWW-Authenticate, want 0")
	}
}

// buildHTTPClient wires the challenge capturer as the OUTERMOST wrapper so it
// observes the final response even alongside header injection.
func TestBuildHTTPClient_WiresChallengeCapture(t *testing.T) {
	var got *AuthChallenge
	var mu sync.Mutex
	client := buildHTTPClient(Config{
		Headers:         map[string]string{"X-Api-Key": "k"},
		OnAuthChallenge: func(ch AuthChallenge) { mu.Lock(); c := ch; got = &c; mu.Unlock() },
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The injected static header still reached the server (capture is
		// outermost, injection inner).
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("static header not injected: %v", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://r.example.com/x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if got == nil || got.ResourceMetadataURL != "https://r.example.com/x" {
		t.Fatalf("challenge not captured through buildHTTPClient: %+v", got)
	}
}

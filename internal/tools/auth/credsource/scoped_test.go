package credsource_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// TestScopedRemote uses the real source, event driver and redactor across an
// authenticated HTTP coordinator contract. All credentials are dummy fixtures.
func TestScopedRemote_ConcurrentIsolationRotationRestart(t *testing.T) {
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	var now atomic.Int64
	now.Store(time.Now().Unix())
	var mu sync.Mutex
	hits := map[string]int{}
	revisions := map[string]int{"tenant-A": 1, "tenant-B": 1}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tenant := req.Header.Get("X-Harbor-Credential-Tenant")
		if req.Method != http.MethodGet || req.Header.Get("X-Harbor-Credential-Scope-Version") != "1" || req.Header.Get("Authorization") != "Bearer "+cDummyServiceToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		revision, allowed := revisions[tenant]
		hits[tenant]++
		mu.Unlock()
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"format_version": 2, "tenant_id": tenant, "client_id": tenant, "client_secret": fmt.Sprintf("dummy-%s-%d", tenant, revision), "expires_in": 3600})
	}))
	defer server.Close()
	red := mkRedactor()
	bus := mkBus(t, red)
	newSource := func() credsource.Source {
		s, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{ProviderName: "fixture", Bus: bus, Redactor: red, Clock: func() time.Time { return time.Unix(now.Load(), 0) }, Remote: &credsource.RemoteConfig{URL: server.URL, AuthTokenEnv: cAuthTokenEnv, Scope: credsource.ExecutionTenantScope}})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.ValidateAtBoot(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	}
	contexts := map[string]context.Context{}
	for _, tenant := range []string{"tenant-A", "tenant-B"} {
		ctx, err := identity.With(context.Background(), identity.Identity{TenantID: tenant, UserID: "user", SessionID: "session"})
		if err != nil {
			t.Fatal(err)
		}
		contexts[tenant] = ctx
	}
	source := newSource()
	burst := func() {
		var wg sync.WaitGroup
		for i := range 100 {
			tenant := []string{"tenant-A", "tenant-B"}[i%2]
			wg.Add(1)
			go func() {
				defer wg.Done()
				cred, err := source.Resolve(contexts[tenant])
				if err != nil || cred.ClientID != tenant {
					t.Errorf("tenant isolation failed for %s: %v", tenant, err)
				}
			}()
		}
		wg.Wait()
	}
	burst()
	burst()
	mu.Lock()
	if hits["tenant-A"] != 1 || hits["tenant-B"] != 1 {
		t.Errorf("cold/warm hits: %v", hits)
	}
	revisions["tenant-A"] = 2
	mu.Unlock()
	if err := source.Invalidate(contexts["tenant-A"]); err != nil {
		t.Fatal(err)
	}
	burst()
	mu.Lock()
	if hits["tenant-A"] != 2 || hits["tenant-B"] != 1 {
		t.Errorf("tenant invalidation hits: %v", hits)
	}
	mu.Unlock()
	c, err := source.Resolve(contexts["tenant-A"])
	if err != nil || c.ClientSecret != "dummy-tenant-A-2" {
		t.Fatalf("rotation: %v", err)
	}
	source = newSource()
	burst()
	mu.Lock()
	if hits["tenant-A"] != 3 || hits["tenant-B"] != 2 {
		t.Errorf("restart hits: %v", hits)
	}
	mu.Unlock()
	now.Add(301)
	burst()
	mu.Lock()
	defer mu.Unlock()
	if hits["tenant-A"] != 4 || hits["tenant-B"] != 3 {
		t.Errorf("expiry hits: %v", hits)
	}

}

func TestScopedRemote_FailClosedAndPermanent(t *testing.T) {
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"wrong tenant", 200, `{"format_version":2,"tenant_id":"other","client_id":"dummy","client_secret":"dummy"}`},
		{"legacy downgrade", 200, `{"format_version":1,"client_id":"dummy","client_secret":"dummy"}`},
		{"malformed", 200, `{`},
		{"unauthorized", 403, `{}`},
		{"conflict", 409, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			red := mkRedactor()
			s, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{ProviderName: "fixture", Bus: mkBus(t, red), Redactor: red, Remote: &credsource.RemoteConfig{URL: server.URL, AuthTokenEnv: cAuthTokenEnv, Scope: credsource.BoundTenantScope, TenantID: "tenant-A"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Resolve(mkCtx(t))
			if !errors.Is(err, credsource.ErrCredentialSourceRejected) || tools.ClassifyError(err, false) != tools.ErrClassPermanent {
				t.Fatalf("error not permanent: %v", err)
			}
			_, err = s.Resolve(context.Background())
			if !errors.Is(err, credsource.ErrCredentialSourceRejected) {
				t.Fatalf("missing identity: %v", err)
			}
		})
	}
}

func TestScopedRemote_CancelAndInvalidateFlight(t *testing.T) {
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"format_version": 2, "tenant_id": "tenant-A", "client_id": "dummy", "client_secret": "dummy"})
	}))
	defer server.Close()
	red := mkRedactor()
	s, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{ProviderName: "fixture", Bus: mkBus(t, red), Redactor: red, Remote: &credsource.RemoteConfig{URL: server.URL, AuthTokenEnv: cAuthTokenEnv, Scope: credsource.ExecutionTenantScope, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Resolve(mkCtx(t)); done <- err }()
	<-started
	if err := s.Invalidate(mkCtx(t)); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, credsource.ErrCredentialSourceRejected) {
		t.Fatalf("in-flight invalidation: %v", err)
	}
}

func TestScopedRemote_CancelledWaiterDoesNotPoisonPeer(t *testing.T) {
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"format_version": 2, "tenant_id": "tenant-A", "client_id": "dummy", "client_secret": "dummy"})
	}))
	defer server.Close()
	red := mkRedactor()
	s, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{ProviderName: "fixture", Bus: mkBus(t, red), Redactor: red, Remote: &credsource.RemoteConfig{URL: server.URL, AuthTokenEnv: cAuthTokenEnv, Scope: credsource.ExecutionTenantScope, Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(mkCtx(t))
	done := make(chan error, 1)
	go func() { _, err := s.Resolve(ctx); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	peer := make(chan error, 1)
	go func() { _, err := s.Resolve(mkCtx(t)); peer <- err }()
	close(release)
	if err := <-peer; err != nil {
		t.Fatalf("peer poisoned: %v", err)
	}
}

func TestScopedRemote_CloseCancelsAndJoins(t *testing.T) {
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	started := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
		close(finished)
	}))
	defer server.Close()
	red := mkRedactor()
	s, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{ProviderName: "fixture", Bus: mkBus(t, red), Redactor: red, Remote: &credsource.RemoteConfig{URL: server.URL, AuthTokenEnv: cAuthTokenEnv, Scope: credsource.ExecutionTenantScope}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Resolve(mkCtx(t)); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("closed fetch succeeded")
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("remote HTTP request survived Close")
	}
	if _, err := s.Resolve(mkCtx(t)); !errors.Is(err, credsource.ErrCredentialSourceRejected) {
		t.Fatalf("closed source admitted caller: %v", err)
	}
}

func TestScopedRemote_RuntimeBearerGenerationDoesNotReuseWarmCredential(t *testing.T) {
	t.Setenv(cAuthTokenEnv, cDummyServiceToken)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.Header.Get("Authorization") != "Bearer "+cDummyServiceToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"format_version": 2, "tenant_id": "tenant-A", "client_id": "dummy", "client_secret": "dummy"})
	}))
	defer server.Close()
	red := mkRedactor()
	s, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{ProviderName: "fixture", Bus: mkBus(t, red), Redactor: red, Remote: &credsource.RemoteConfig{URL: server.URL, AuthTokenEnv: cAuthTokenEnv, Scope: credsource.ExecutionTenantScope}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Resolve(mkCtx(t)); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cAuthTokenEnv, "dummy-rotated-service-token")
	if _, err = s.Resolve(mkCtx(t)); !errors.Is(err, credsource.ErrCredentialSourceRejected) {
		t.Fatalf("rotated runtime bearer reused warm credential: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("generation hits = %d", hits.Load())
	}
}

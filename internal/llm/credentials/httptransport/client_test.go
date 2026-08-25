package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credentials"
)

const fixtureServiceToken = "fixture-runtime-service-token"

func resolverGrant(org string, generation uint64) llm.ExternalGrant {
	now := time.Now().UTC().Truncate(time.Second)
	return llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key-1", Audience: "runtime",
		GrantID: "grant-" + org, RouteMode: llm.ExternalGrantRouteCoordinatorBound,
		OrganizationID: org, RuntimeID: "shared-runtime", AgentID: "agent-" + org,
		TenantID: "tenant-" + org, UserID: "user-" + org, SessionID: "session-" + org,
		LogicalRunID: "run-" + org, LogicalCallID: "call-" + org, AttemptNonce: "nonce-" + org,
		Provider: "provider", ProviderModelID: "model", ProviderConnectionID: "connection-" + org,
		ProviderConnectionGeneration: generation, RouteID: "route-" + org,
		CredentialBindingHandle: "opaque-" + org, CredentialAssetGeneration: generation,
		PolicyGeneration: 1, MaxOutputTokens: 100,
		Lease:    llm.ComputeLease{LeaseID: "lease-" + org, Epoch: 1, TokenUnits: 1000, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), Signature: "fixture-signature-" + org,
	}
}

func verifiedContext(ctx context.Context, client *Client, grant llm.ExternalGrant) context.Context {
	return llm.WithVerifiedGrantContext(ctx, grant, client)
}

func responseFor(request credentials.Request, secret string) credentials.Response {
	return credentials.Response{
		Version: credentials.Version, Provider: request.Grant.Provider,
		CredentialBindingHandle:      request.Grant.CredentialBindingHandle,
		CredentialAssetGeneration:    request.Grant.CredentialAssetGeneration,
		ProviderConnectionGeneration: request.Grant.ProviderConnectionGeneration,
		Secret:                       secret, ExpiresAt: request.Grant.ExpiresAt,
	}
}

func newResolverClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client, err := New(Config{CredentialURL: serverURL, AuthToken: fixtureServiceToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestClient_ConcurrentTwoOrganizationsSameRuntimeNoBleedAndSingleflight(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+fixtureServiceToken {
			t.Errorf("authorization = %q", got)
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		request, err := credentials.UnmarshalCanonicalRequest(raw)
		if err != nil {
			t.Errorf("request: %v", err)
			return
		}
		body, err := credentials.MarshalCanonicalResponse(request, responseFor(request, "secret-"+request.Grant.OrganizationID))
		if err != nil {
			t.Errorf("response: %v", err)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := newResolverClient(t, server.URL)

	grants := []llm.ExternalGrant{resolverGrant("org-a", 1), resolverGrant("org-b", 1)}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 200 {
		grant := grants[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolved, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant)
			if err == nil && resolved.Secret != "secret-"+grant.OrganizationID {
				err = fmt.Errorf("cross-organization secret: %q", resolved.Secret)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("coordinator calls = %d, want one exact-binding fetch per organization", got)
	}
}

func TestClient_RequiresVerifiedExactGrantAndFencesRotation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw := json.RawMessage{}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		request, err := credentials.UnmarshalCanonicalRequest(raw)
		if err != nil {
			t.Error(err)
			return
		}
		body, _ := credentials.MarshalCanonicalResponse(request, responseFor(request, fmt.Sprintf("secret-v%d", request.Grant.CredentialAssetGeneration)))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := newResolverClient(t, server.URL)
	old := resolverGrant("org-a", 1)
	if _, err := client.Resolve(context.Background(), old); !errors.Is(err, ErrResolution) {
		t.Fatalf("unverified error = %v", err)
	}
	wrong := resolverGrant("org-b", 1)
	if _, err := client.Resolve(verifiedContext(context.Background(), client, wrong), old); !errors.Is(err, ErrResolution) {
		t.Fatalf("mismatch error = %v", err)
	}
	resolved, err := client.Resolve(verifiedContext(context.Background(), client, old), old)
	if err != nil || resolved.Secret != "secret-v1" {
		t.Fatalf("old resolve = %#v, %v", resolved, err)
	}
	rotated := old
	rotated.GrantID = "grant-org-a-v2"
	rotated.CredentialBindingHandle = "opaque-org-a-v2"
	rotated.CredentialAssetGeneration = 2
	rotated.ProviderConnectionGeneration = 2
	rotated.Signature = "fixture-signature-org-a-v2"
	resolved, err = client.Resolve(verifiedContext(context.Background(), client, rotated), rotated)
	if err != nil || resolved.Secret != "secret-v2" || calls.Load() != 2 {
		t.Fatalf("rotated resolve = %#v, %v, calls=%d", resolved, err, calls.Load())
	}
}

func TestClient_CancellationDoesNotCrossSingleflightAndCloseClearsCache(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		raw := json.RawMessage{}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		request, _ := credentials.UnmarshalCanonicalRequest(raw)
		body, _ := credentials.MarshalCanonicalResponse(request, responseFor(request, "secret-org-a"))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := newResolverClient(t, server.URL)
	grant := resolverGrant("org-a", 1)
	cancelCtx, cancel := context.WithCancel(verifiedContext(context.Background(), client, grant))
	first := make(chan error, 1)
	go func() { _, err := client.Resolve(cancelCtx, grant); first <- err }()
	<-started
	second := make(chan error, 1)
	go func() {
		_, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant)
		second <- err
	}()
	cancel()
	if err := <-first; !errors.Is(err, ErrResolution) {
		t.Fatalf("canceled caller error = %v", err)
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatalf("shared caller error = %v", err)
	}
	client.mu.Lock()
	if len(client.cache) != 1 {
		t.Fatalf("cache entries = %d", len(client.cache))
	}
	client.mu.Unlock()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.cache) != 0 {
		t.Fatalf("cache retained %d entries", len(client.cache))
	}
}

func TestClient_CacheIsBoundedAndExpiresAtThirtySeconds(t *testing.T) {
	var calls atomic.Int32
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		request, _ := credentials.UnmarshalCanonicalRequest(raw)
		body, _ := credentials.MarshalCanonicalResponse(request, responseFor(request, "secret-"+request.Grant.OrganizationID))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := New(Config{
		CredentialURL: server.URL,
		AuthToken:     fixtureServiceToken,
		Clock:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for i := range maxCacheEntries + 1 {
		grant := resolverGrant(fmt.Sprintf("org-%03d", i), 1)
		grant.IssuedAt = now
		grant.ExpiresAt = now.Add(time.Minute)
		grant.Lease.ExpiresAt = grant.ExpiresAt
		if _, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	client.mu.Lock()
	if got := len(client.cache); got != maxCacheEntries {
		client.mu.Unlock()
		t.Fatalf("cache entries = %d, want %d", got, maxCacheEntries)
	}
	client.mu.Unlock()

	grant := resolverGrant("expiry", 1)
	grant.IssuedAt = now
	grant.ExpiresAt = now.Add(time.Minute)
	grant.Lease.ExpiresAt = grant.ExpiresAt
	if _, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	now = now.Add(defaultCacheTTL + time.Second)
	if _, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != before+1 {
		t.Fatalf("coordinator calls after TTL = %d, want %d", got, before+1)
	}
}

func TestClient_ClosePreventsInflightCacheRepopulation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		request, _ := credentials.UnmarshalCanonicalRequest(raw)
		body, _ := credentials.MarshalCanonicalResponse(request, responseFor(request, "secret-org-a"))
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := newResolverClient(t, server.URL)
	grant := resolverGrant("org-a", 1)
	done := make(chan error, 1)
	go func() {
		_, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant)
		done <- err
	}()
	<-started
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight caller error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.cache) != 0 {
		t.Fatalf("closed client cache repopulated with %d entries", len(client.cache))
	}
}

func TestClient_RedactsTransportAndSecretFailures(t *testing.T) {
	const leakedBody = "fixture-sensitive-provider-body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(leakedBody))
	}))
	defer server.Close()
	client := newResolverClient(t, server.URL)
	grant := resolverGrant("org-a", 1)
	_, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant)
	if !errors.Is(err, ErrResolution) {
		t.Fatalf("error = %v", err)
	}
	for _, forbidden := range []string{fixtureServiceToken, leakedBody, server.URL, grant.CredentialBindingHandle} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked %q: %v", forbidden, err)
		}
	}
}

func TestClient_RefusesUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.com/resolve", "https://user:pass@example.com/resolve", "https://example.com/resolve?q=1"} {
		if _, err := New(Config{CredentialURL: endpoint, AuthToken: fixtureServiceToken}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("endpoint %q error = %v", endpoint, err)
		}
	}
	for name, cfg := range map[string]Config{
		"empty token":      {CredentialURL: "https://example.com/resolve"},
		"negative timeout": {CredentialURL: "https://example.com/resolve", AuthToken: fixtureServiceToken, Timeout: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestClient_ResolveAfterCloseFailsLoud(t *testing.T) {
	client, err := New(Config{
		CredentialURL: "https://example.com/resolve",
		AuthToken:     fixtureServiceToken,
		HTTPClient:    &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	grant := resolverGrant("org-a", 1)
	if _, err := client.Resolve(verifiedContext(context.Background(), client, grant), grant); !errors.Is(err, ErrClosed) {
		t.Fatalf("error = %v", err)
	}
}

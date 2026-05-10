package s3_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	s3driver "github.com/hurtener/Harbor/internal/artifacts/drivers/s3"
)

// TestS3_PresignGet_HappyPath verifies the URL the driver returns is
// fetchable: a HTTP GET against the presigned URL returns the same
// bytes that were Put. End-to-end gate for the read-side hand-off
// described in RFC §6.10.
func TestS3_PresignGet_HappyPath(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{
		TenantID:  "tenant-presign",
		UserID:    "user-1",
		SessionID: "sess-1",
		TaskID:    "task-1",
	}
	payload := []byte("presigned-bytes-payload")
	ref, err := s.PutBytes(context.Background(), scope, payload,
		artifacts.PutOpts{Namespace: "ns.presign"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	presigner, ok := s.(artifacts.Presigner)
	if !ok {
		t.Fatalf("s3 driver did not implement artifacts.Presigner — capability gate broken")
	}

	url, err := presigner.PresignGet(context.Background(), scope, ref.ID, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if url == "" {
		t.Fatalf("PresignGet returned empty URL")
	}

	// Fetch the URL and verify the bytes round-trip.
	httpClient := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("http.Get presigned URL: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned URL fetch: status=%d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("body=%q, want %q", got, payload)
	}
}

// TestS3_PresignGet_ExpiryOutOfRange pins the bound check: expiries
// shorter than 1 minute or longer than 7 days are rejected with a
// clear error. Fail-loudly per AGENTS.md §5; no silent clamping.
func TestS3_PresignGet_ExpiryOutOfRange(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	ref, err := s.PutBytes(context.Background(), scope, []byte("x"),
		artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	presigner := s.(artifacts.Presigner)

	cases := []struct {
		name   string
		expiry time.Duration
	}{
		{"zero", 0},
		{"sub-minute", 30 * time.Second},
		{"negative", -1 * time.Minute},
		{"over-7d", 8 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := presigner.PresignGet(context.Background(), scope, ref.ID, tc.expiry)
			if err == nil {
				t.Fatalf("PresignGet(%s) returned nil error; want out-of-range", tc.expiry)
			}
			if !strings.Contains(err.Error(), "expiry") && !strings.Contains(err.Error(), "out of range") {
				t.Errorf("error should reference expiry/range; got: %v", err)
			}
		})
	}
}

// TestS3_PresignGet_IdentityRequired pins the multi-isolation invariant
// at the Presigner boundary: missing tenant/user/session must be
// rejected with `ErrIdentityRequired`, not silently degraded.
func TestS3_PresignGet_IdentityRequired(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	presigner := s.(artifacts.Presigner)

	cases := []artifacts.ArtifactScope{
		{}, // all empty
		{UserID: "U", SessionID: "S"},
		{TenantID: "T", SessionID: "S"},
		{TenantID: "T", UserID: "U"},
	}
	for i, sc := range cases {
		_, err := presigner.PresignGet(context.Background(), sc, "ns_deadbeef0000", 5*time.Minute)
		if !errors.Is(err, artifacts.ErrIdentityRequired) {
			t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, sc, err)
		}
	}
}

// TestS3_PresignGet_NotFound pins the fail-loudly behavior: presigning
// a non-existent id within a valid scope returns wrapped
// `artifacts.ErrNotFound` rather than a URL that 404s downstream.
func TestS3_PresignGet_NotFound(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	presigner := s.(artifacts.Presigner)

	_, err = presigner.PresignGet(context.Background(), scope, "ns_deadbeef0000", 5*time.Minute)
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("PresignGet on absent id: err=%v, want ErrNotFound", err)
	}
}

// TestS3_PresignGet_CrossTenantIsolation pins the cross-tenant
// invariant: tenant B asking for a URL pointing at tenant A's id
// returns `ErrNotFound` because tenant B's scope-derived key does not
// exist. Verifies the driver's key derivation actually folds the
// tenant identity into the object key (so a stolen URL cannot reach
// across tenants).
func TestS3_PresignGet_CrossTenantIsolation(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scopeA := artifacts.ArtifactScope{
		TenantID: "tenant-A", UserID: "u", SessionID: "s",
	}
	scopeB := artifacts.ArtifactScope{
		TenantID: "tenant-B", UserID: "u", SessionID: "s",
	}
	ref, err := s.PutBytes(context.Background(), scopeA, []byte("tenant-A-secret"),
		artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	presigner := s.(artifacts.Presigner)

	// Tenant B asks to presign tenant A's id — must get ErrNotFound,
	// not a working URL pointing at tenant A's bytes.
	_, err = presigner.PresignGet(context.Background(), scopeB, ref.ID, 5*time.Minute)
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("cross-tenant presign: err=%v, want ErrNotFound", err)
	}

	// Tenant A's own presign still works.
	url, err := presigner.PresignGet(context.Background(), scopeA, ref.ID, 5*time.Minute)
	if err != nil {
		t.Errorf("same-tenant presign: err=%v", err)
	}
	if url == "" {
		t.Errorf("same-tenant presign: empty URL")
	}
}

// TestS3_PresignGet_AfterClose pins ErrStoreClosed propagation: a
// presign call after Close returns ErrStoreClosed (not a URL).
func TestS3_PresignGet_AfterClose(t *testing.T) {
	tc := requireS3(t)
	prefix := uniquePrefix(t)
	t.Cleanup(func() { cleanupPrefix(t, tc, prefix) })

	s, err := s3driver.New(driverConfig(tc, prefix))
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	ref, err := s.PutBytes(context.Background(), scope, []byte("x"),
		artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	presigner := s.(artifacts.Presigner)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = presigner.PresignGet(context.Background(), scope, ref.ID, 5*time.Minute)
	if !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("PresignGet after Close: err=%v, want ErrStoreClosed", err)
	}
}

package integration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	artfs "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

// The artifacts.get end-to-end gate (Phase 209 / D-353).
//
// Everything here drives the REAL control transport over httptest against
// REAL artifact drivers — no mock sits on the seam. Two drivers run, and
// NEITHER implements the Presigner capability: that is the point. The
// only artifact read path that existed before this phase type-asserts
// Presigner and answers CodePresignUnsupported when the assertion misses,
// with exactly one of five drivers implementing it and NOT the default.
// If this method regressed into a capability-dependent read, both driver
// sub-runs here would fail rather than one convenient store carrying the
// suite.

// getTestStores returns the real, non-presigning drivers this suite runs
// against. `fs` is included because it is the single-binary production
// target and it resolves through an on-disk layout rather than a map, so
// a byte read that worked only against an in-memory slice would fail here.
func getTestStores(t *testing.T) map[string]func(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	return map[string]func(t *testing.T) artifacts.ArtifactStore{
		"inmem": func(t *testing.T) artifacts.ArtifactStore {
			t.Helper()
			store, err := artinmem.New(config.ArtifactsConfig{Driver: "inmem"})
			if err != nil {
				t.Fatalf("inmem driver: %v", err)
			}
			return store
		},
		"fs": func(t *testing.T) artifacts.ArtifactStore {
			t.Helper()
			store, err := artfs.New(config.ArtifactsConfig{
				Driver: "fs", FSRoot: filepath.Join(t.TempDir(), "artifacts"),
			})
			if err != nil {
				t.Fatalf("fs driver: %v", err)
			}
			return store
		},
	}
}

// artifactsGetB64 pulls the base64 `content` field off a decoded
// artifacts.get response and decodes it.
func artifactsGetB64(t *testing.T, body map[string]any) []byte {
	t.Helper()
	raw, ok := body["content"]
	if !ok || raw == nil {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("content is %T, want a base64 string", raw)
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("content is not valid base64: %v", err)
	}
	return out
}

func artifactsGetNum(t *testing.T, body map[string]any, key string) int64 {
	t.Helper()
	v, ok := body[key]
	if !ok {
		t.Fatalf("response is missing %q (body=%v)", key, body)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%q is %T, want a number", key, v)
	}
	return int64(f)
}

// artifactsGetTruncated reads the response's `truncated` flag, failing when
// the field is absent or not a bool — an omitted bound signal is a wire
// regression, not a `false`.
//
// It names the field rather than taking it as a parameter: `truncated` is the
// only bool the response carries, so a `key string` parameter was generality
// no caller could use, and `unparam` says so once enough call sites exist.
func artifactsGetTruncated(t *testing.T, body map[string]any) bool {
	t.Helper()
	v, ok := body["truncated"]
	if !ok {
		t.Fatalf("response is missing \"truncated\" (body=%v)", body)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("\"truncated\" is %T, want a bool", v)
	}
	return b
}

// TestE2E_ArtifactsGet_RoundTripsOnEveryDriverOverTheWire is the phase's
// central end-to-end claim: put then get, through the real transport,
// on stores that cannot presign.
func TestE2E_ArtifactsGet_RoundTripsOnEveryDriverOverTheWire(t *testing.T) {
	t.Parallel()
	for name, open := range getTestStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := open(t)
			deps := newArtifactsStack(t, store)
			defer deps.cleanup()

			verified := identity.Identity{TenantID: "tenant-a", UserID: "u1", SessionID: "s1"}
			srv := httptest.NewServer(withVerifiedIdentity(deps.mux, verified, nil))
			defer srv.Close()

			scope := map[string]any{"tenant": "tenant-a", "user": "u1", "session": "s1"}
			payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")

			status, body := callArtifacts(t, srv.URL, methods.MethodArtifactsPut, map[string]any{
				"scope": scope, "bytes": payload,
			})
			if status != http.StatusOK {
				t.Fatalf("artifacts.put status = %d, body=%v", status, body)
			}
			id := body["ref"].(map[string]any)["id"].(string)

			// The same driver refuses get_ref — the asymmetry this method
			// closes, asserted rather than assumed.
			refStatus, refBody := callArtifacts(t, srv.URL, methods.MethodArtifactsGetRef, map[string]any{
				"scope": scope, "id": id,
			})
			if refStatus != http.StatusNotImplemented {
				t.Fatalf("artifacts.get_ref on %s: status = %d, want 501 presign_unsupported (body=%v)", name, refStatus, refBody)
			}

			// The byte read works.
			getStatus, getBody := callArtifacts(t, srv.URL, methods.MethodArtifactsGet, map[string]any{
				"scope": scope, "id": id,
			})
			if getStatus != http.StatusOK {
				t.Fatalf("artifacts.get on %s: status = %d, body=%v", name, getStatus, getBody)
			}
			if got := artifactsGetB64(t, getBody); string(got) != string(payload) {
				t.Fatalf("artifacts.get on %s: content = %q, want %q", name, got, payload)
			}
			if got := artifactsGetNum(t, getBody, "total_size_bytes"); got != int64(len(payload)) {
				t.Fatalf("total_size_bytes = %d, want %d", got, len(payload))
			}
			if got := artifactsGetNum(t, getBody, "returned_bytes"); got != int64(len(payload)) {
				t.Fatalf("returned_bytes = %d, want %d", got, len(payload))
			}
			if artifactsGetTruncated(t, getBody) {
				t.Fatal("truncated = true on a complete read")
			}

			// A bounded read is truthful, and an offset window returns the
			// requested byte range.
			bStatus, bBody := callArtifacts(t, srv.URL, methods.MethodArtifactsGet, map[string]any{
				"scope": scope, "id": id, "max_bytes": 10,
			})
			if bStatus != http.StatusOK {
				t.Fatalf("bounded artifacts.get status = %d, body=%v", bStatus, bBody)
			}
			if !artifactsGetTruncated(t, bBody) {
				t.Fatal("a bounded read reported truncated=false")
			}
			if got := artifactsGetB64(t, bBody); string(got) != "0123456789" {
				t.Fatalf("bounded content = %q, want %q", got, "0123456789")
			}

			wStatus, wBody := callArtifacts(t, srv.URL, methods.MethodArtifactsGet, map[string]any{
				"scope": scope, "id": id, "offset": 10, "max_bytes": 6,
			})
			if wStatus != http.StatusOK {
				t.Fatalf("offset artifacts.get status = %d, body=%v", wStatus, wBody)
			}
			if got := artifactsGetB64(t, wBody); string(got) != "abcdef" {
				t.Fatalf("offset window content = %q, want %q", got, "abcdef")
			}
			if got := artifactsGetNum(t, wBody, "offset"); got != 10 {
				t.Fatalf("offset echo = %d, want 10", got)
			}

			// The last window reports truncated=false — the property a
			// `returned < total` implementation gets wrong.
			lStatus, lBody := callArtifacts(t, srv.URL, methods.MethodArtifactsGet, map[string]any{
				"scope": scope, "id": id, "offset": len(payload) - 6, "max_bytes": 6,
			})
			if lStatus != http.StatusOK {
				t.Fatalf("last-window artifacts.get status = %d, body=%v", lStatus, lBody)
			}
			if artifactsGetTruncated(t, lBody) {
				t.Fatal("the last window reported truncated=true")
			}
			if got := artifactsGetB64(t, lBody); string(got) != "uvwxyz" {
				t.Fatalf("last window content = %q, want %q", got, "uvwxyz")
			}
		})
	}
}

// TestE2E_ArtifactsGet_IdentityPropagatesAndIsolates is the §17.3
// identity-propagation requirement plus the failure modes: a foreign
// tenant is refused at the edge, an id from another identity answers
// not-found, and a request with NO verified identity is refused rather
// than trusting the body.
func TestE2E_ArtifactsGet_IdentityPropagatesAndIsolates(t *testing.T) {
	t.Parallel()
	store, err := artinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem driver: %v", err)
	}
	deps := newArtifactsStack(t, store)
	defer deps.cleanup()

	tenantA := identity.Identity{TenantID: "tenant-a", UserID: "u1", SessionID: "s1"}
	srvA := httptest.NewServer(withVerifiedIdentity(deps.mux, tenantA, nil))
	defer srvA.Close()

	scopeA := map[string]any{"tenant": "tenant-a", "user": "u1", "session": "s1"}
	secret := []byte("tenant a's private bytes")
	status, body := callArtifacts(t, srvA.URL, methods.MethodArtifactsPut, map[string]any{
		"scope": scopeA, "bytes": secret,
	})
	if status != http.StatusOK {
		t.Fatalf("artifacts.put status = %d, body=%v", status, body)
	}
	idA := body["ref"].(map[string]any)["id"].(string)

	// Tenant B, holding BOTH admin-tier claims, naming its own scope and
	// tenant A's id: not-found. Nothing about tenant A's holdings leaks.
	tenantB := identity.Identity{TenantID: "tenant-b", UserID: "u1", SessionID: "s1"}
	srvB := httptest.NewServer(withVerifiedIdentity(deps.mux, tenantB,
		[]auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet}))
	defer srvB.Close()

	scopeB := map[string]any{"tenant": "tenant-b", "user": "u1", "session": "s1"}
	bStatus, bBody := callArtifacts(t, srvB.URL, methods.MethodArtifactsGet, map[string]any{
		"scope": scopeB, "id": idA,
	})
	if bStatus != http.StatusNotFound {
		t.Fatalf("cross-tenant get by id: status = %d, want 404 (body=%v)", bStatus, bBody)
	}
	// An UNKNOWN id must be indistinguishable from a foreign one — the
	// difference between them is exactly what a prober would harvest.
	uStatus, uBody := callArtifacts(t, srvB.URL, methods.MethodArtifactsGet, map[string]any{
		"scope": scopeB, "id": "default_ffffffffffff",
	})
	if uStatus != bStatus {
		t.Fatalf("an unknown id (%d) and a foreign id (%d) answer differently — that difference reveals existence", uStatus, bStatus)
	}
	if fmt.Sprint(uBody["code"]) != fmt.Sprint(bBody["code"]) {
		t.Fatalf("unknown id code %v differs from foreign id code %v", uBody["code"], bBody["code"])
	}

	// Tenant B naming tenant A in the BODY: refused flat at the edge with
	// both claims held. This is the reachable foreign-tenant shape (the
	// body-identity gate rejects a differing user/session before this).
	fStatus, fBody := callArtifacts(t, srvB.URL, methods.MethodArtifactsGet, map[string]any{
		"scope": scopeA, "id": idA,
	})
	if fStatus != http.StatusForbidden {
		t.Fatalf("foreign-tenant get: status = %d, want 403 (body=%v)", fStatus, fBody)
	}
	if got := fmt.Sprint(fBody["code"]); got != "scope_mismatch" {
		t.Fatalf("foreign-tenant get: code = %q, want scope_mismatch", got)
	}

	// FAILURE MODE: a request that establishes NO identity at all. The
	// body must not be trusted as a fallback — a request that reached the
	// surface without an established identity must not be
	// indistinguishable from one that carried a verified triple.
	//
	// The mux is mounted bare here (no withVerifiedIdentity wrapper) and
	// the request carries no identity carrier, so the transport's own
	// resolution has nothing to seat.
	srvNone := httptest.NewServer(deps.mux)
	defer srvNone.Close()
	reqBody, err := json.Marshal(map[string]any{"scope": scopeA, "id": idA})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	//nolint:noctx // end-to-end wire fixture; the test's own deadline bounds the run.
	req, err := http.NewRequest(http.MethodPost,
		srvNone.URL+"/v1/control/"+string(methods.MethodArtifactsGet), bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST without an identity carrier: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var nBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&nBody)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a request with no established identity resolved bytes (body=%v)", nBody)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-identity get: status = %d, want 401 (body=%v)", resp.StatusCode, nBody)
	}
}

// withCarrierVerifiedIdentity seats each request's OWN identity carrier as
// its verified identity, so ONE mounted handler serves every tenant.
//
// The sibling helper `withVerifiedIdentity` bakes one fixed identity into
// the middleware, which forces a server per tenant. That is fine for a
// two- or three-identity test and wrong for a concurrency stress: N
// servers means N listeners and N accept loops competing with whatever
// else the package is running in parallel, and it also makes the stress
// WEAKER — N tenants against N handler instances does not exercise the
// thing being asserted. Sharing one handler is both cheaper and a
// stricter test of the no-cross-talk property.
func withCarrierVerifiedIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := identity.WithVerified(r.Context(), identity.Identity{
			TenantID:  r.Header.Get("X-Harbor-Tenant"),
			UserID:    r.Header.Get("X-Harbor-User"),
			SessionID: r.Header.Get("X-Harbor-Session"),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestE2E_ArtifactsGet_ConcurrentAcrossTenantsOverTheWire is the §17.3
// cross-package concurrency stress: N concurrent readers, each in its own
// tenant, all through ONE shared transport + surface, each asserting it
// received ITS OWN bytes. N=32 > the mandated 10.
//
// One server rather than one per tenant. That is the point rather than a
// saving: N tenants hitting N handler instances proves nothing about
// isolation, while N tenants sharing ONE handler and ONE surface is
// exactly the shape a cross-talk bug would show up in. It also keeps the
// test's resource footprint to a single listener — an earlier draft held
// 32 open simultaneously and the extra accept loops starved a sibling
// PTY test in the same package past its render budget, turning a green
// suite intermittently red for a reason that had nothing to do with
// artifacts.
func TestE2E_ArtifactsGet_ConcurrentAcrossTenantsOverTheWire(t *testing.T) {
	t.Parallel()
	const n = 32

	store, err := artinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("inmem driver: %v", err)
	}
	deps := newArtifactsStack(t, store)
	defer deps.cleanup()

	srv := httptest.NewServer(withCarrierVerifiedIdentity(deps.mux))
	defer srv.Close()

	type tenantFixture struct {
		scope   map[string]any
		id      string
		payload string
	}
	fixtures := make([]tenantFixture, n)
	for i := range fixtures {
		scope := map[string]any{
			"tenant":  fmt.Sprintf("tenant-%02d", i),
			"user":    fmt.Sprintf("user-%02d", i),
			"session": fmt.Sprintf("session-%02d", i),
		}
		payload := fmt.Sprintf("payload-for-tenant-%02d-%s", i, "................................")
		status, body := callArtifacts(t, srv.URL, methods.MethodArtifactsPut, map[string]any{
			"scope": scope, "bytes": []byte(payload),
		})
		if status != http.StatusOK {
			t.Fatalf("seed put for tenant %d: status = %d, body=%v", i, status, body)
		}
		fixtures[i] = tenantFixture{
			scope: scope, payload: payload,
			id: body["ref"].(map[string]any)["id"].(string),
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range fixtures {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			f := fixtures[i]
			status, body := callArtifacts(t, srv.URL, methods.MethodArtifactsGet, map[string]any{
				"scope": f.scope, "id": f.id,
			})
			if status != http.StatusOK {
				errs[i] = fmt.Errorf("tenant %d: status %d body %v", i, status, body)
				return
			}
			got := artifactsGetB64(t, body)
			if string(got) != f.payload {
				errs[i] = fmt.Errorf("tenant %d: cross-talk — content %q want %q", i, got, f.payload)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

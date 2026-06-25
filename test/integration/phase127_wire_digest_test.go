// Wire-surface digest cross-subsystem integration test (CLAUDE.md §17).
//
// The wire-surface digest surface is:
//
//   - internal/protocol/wiresurface.Digest() — the canonical name-level
//     fingerprint computed from the Protocol single sources.
//   - The additive types.RuntimeInfo.WireSurfaceDigest field, populated by
//     protocol.PostureSurface.handleInfo and returned on runtime.info.
//   - The committed wire manifest's top-level wire_surface_digest
//     (web/console/src/lib/protocol/wire-manifest.gen.json), stamped by
//     cmd/harbor-protocol-ts-lockstep.
//
// This test is the same-PR §13 consumer at the runtime boundary: it boots a
// real assembled Runtime via harbortest/devstack.Assemble (real inmem
// drivers + a real ES256 auth keypair), constructs a real PostureSurface
// wired to those drivers via the production posture seams, mounts it through
// the real transports.NewMux, and asserts end-to-end:
//
//  1. runtime.info over the wire returns a wire_surface_digest equal to
//     wiresurface.Digest() AND equal to the committed manifest's top-level
//     wire_surface_digest (read from the repo file) — the runtime, the
//     canonical function, and the vendored manifest agree.
//  2. Failure mode — a request whose body identity does not match the
//     verified JWT is rejected 401 / identity_required before any digest is
//     returned (identity is mandatory at the edge).
//  3. N>=100 concurrent operators read runtime.info against the single shared
//     surface and every reported digest is byte-identical, with the goroutine
//     baseline restored after teardown (no leak, no context bleed).
//
// Real drivers everywhere on the seam — no mocks (CLAUDE.md §17.3). Runs
// under -race.
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/protocol/wiresurface"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// committedManifestDigest reads the top-level wire_surface_digest from the
// committed wire manifest (relative to test/integration).
func committedManifestDigest(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "web", "console", "src", "lib", "protocol", "wire-manifest.gen.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed manifest %q: %v", path, err)
	}
	var m struct {
		WireSurfaceDigest string `json:"wire_surface_digest"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode committed manifest: %v", err)
	}
	if m.WireSurfaceDigest == "" {
		t.Fatal("committed manifest carries no top-level wire_surface_digest")
	}
	return m.WireSurfaceDigest
}

func TestE2E_Phase127_WireDigestOverTheWire(t *testing.T) {
	cfg := runtimePostureConfig(t)
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{SkipRunLoop: true})
	defer stack.Close()

	if stack.Surface == nil || stack.Validator == nil || stack.SigningKey == nil {
		t.Fatal("devstack did not assemble the Surface / Validator / SigningKey")
	}

	// A real MetricsRegistry fed by a real bus→metrics bridge — the exact
	// production wiring the production posture seam projects.
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(stack.Cfg.Telemetry,
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		t.Fatalf("telemetry.NewMetricsRegistry: %v", err)
	}
	defer func() { _ = metricsShutdown(context.Background()) }()
	bridgeStop, err := telemetry.BridgeBusToMetrics(context.Background(), stack.Bus, metricsReg, eventsAdminFilter())
	if err != nil {
		t.Fatalf("telemetry.BridgeBusToMetrics: %v", err)
	}
	defer bridgeStop()

	posture := buildPostureSurface(t, stack, metricsReg)

	mux, err := transports.NewMux(stack.Surface, stack.Bus,
		transports.WithValidator(stack.Validator),
		transports.WithPostureSurface(posture),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	devID := identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	}

	canonical := wiresurface.Digest()
	manifestDigest := committedManifestDigest(t)

	// The canonical function and the committed manifest agree within this
	// build (the lockstep gate guarantees it; assert it here too so a stale
	// manifest fails the E2E directly).
	if canonical != manifestDigest {
		t.Fatalf("wiresurface.Digest()=%q != committed manifest digest=%q — run 'make protocol-ts-gen'",
			canonical, manifestDigest)
	}

	// --- (1) runtime.info over the wire returns the agreeing digest -------
	t.Run("Wire_RuntimeInfo_ReportsCanonicalDigest", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		body, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeInfo, body, token)
		if status != http.StatusOK {
			t.Fatalf("runtime.info: status %d, want 200; body=%s", status, raw)
		}
		var info types.RuntimeInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("decode RuntimeInfo: %v; body=%s", err, raw)
		}
		if info.WireSurfaceDigest != canonical {
			t.Errorf("runtime.info wire_surface_digest = %q, want wiresurface.Digest() = %q",
				info.WireSurfaceDigest, canonical)
		}
		if info.WireSurfaceDigest != manifestDigest {
			t.Errorf("runtime.info wire_surface_digest = %q, want committed manifest digest = %q",
				info.WireSurfaceDigest, manifestDigest)
		}
	})

	// --- (2) failure mode: missing/mismatched identity is rejected --------
	t.Run("Wire_MissingIdentity_FailsClosedBeforeDigest", func(t *testing.T) {
		token := signPostureToken(t, stack.SigningKey, devID, nil)
		// A body identity that does not match the verified JWT (user
		// mismatch) is rejected identity_required → 401 before any digest
		// is produced.
		badBody, _ := json.Marshal(types.RuntimeInfoRequest{
			Identity: types.IdentityScope{
				Tenant: devID.TenantID, User: "someone-else", Session: devID.SessionID,
			},
		})
		status, raw := postPosture(t, srv.URL, methods.MethodRuntimeInfo, badBody, token)
		if status != http.StatusUnauthorized {
			t.Fatalf("body-identity mismatch: status %d, want 401; body=%s", status, raw)
		}
		// No digest leaks in the rejection body.
		if len(raw) > 0 {
			var info types.RuntimeInfo
			_ = json.Unmarshal(raw, &info)
			if info.WireSurfaceDigest != "" {
				t.Errorf("rejection body leaked a wire_surface_digest: %q", info.WireSurfaceDigest)
			}
		}
	})

	// --- (3) N>=100 concurrent reads return a byte-identical digest -------
	t.Run("Concurrency_IdenticalDigest", func(t *testing.T) {
		time.Sleep(20 * time.Millisecond)
		baseline := runtime.NumGoroutine()

		const n = 128
		var wg sync.WaitGroup
		digests := make([]string, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				token := signPostureToken(t, stack.SigningKey, devID, nil)
				body, _ := json.Marshal(types.RuntimeInfoRequest{
					Identity: types.IdentityScope{
						Tenant: devID.TenantID, User: devID.UserID, Session: devID.SessionID,
					},
				})
				status, raw := postPosture(t, srv.URL, methods.MethodRuntimeInfo, body, token)
				if status != http.StatusOK {
					digests[i] = "ERR:" + string(raw)
					return
				}
				var info types.RuntimeInfo
				if err := json.Unmarshal(raw, &info); err != nil {
					digests[i] = "ERR:decode"
					return
				}
				digests[i] = info.WireSurfaceDigest
			}(i)
		}
		close(start)
		wg.Wait()

		for i, d := range digests {
			if d != canonical {
				t.Fatalf("operator %d reported digest %q, want %q", i, d, canonical)
			}
		}

		time.Sleep(50 * time.Millisecond)
		if after := runtime.NumGoroutine(); after > baseline+8 {
			t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
		}
	})
}

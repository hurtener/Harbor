package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"

	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// TestE2E_NonAdminToken_SteeringContract wires the real Protocol control
// transport (real ControlSurface over a real steering Registry, a real
// task registry over an inmem StateStore, a real inmem event bus, a real
// patterns audit redactor) behind the production auth.Middleware with a
// JWKS-backed validator, and drives it over a real httptest server with
// REAL JWTs signed by the IdP's private key. It proves the non-admin
// session-scoped token contract end-to-end:
//
//   - a non-admin (no-scope) token can steer runs it OWNS — INJECT_CONTEXT
//     lands on the inbox (200);
//   - the same non-admin owner is REJECTED 403 scope_mismatch on the
//     admin-only PRIORITIZE;
//   - the admin token CAN PRIORITIZE the same run (200) — admin is the one
//     path;
//   - a non-admin token for a DIFFERENT user in the same tenant cannot
//     steer another user's run even when they share a session-id string —
//     the body-vs-JWT transport gate rejects it 401 (the collision is
//     blocked at the wire edge, defence in depth over deriveSteeringScope);
//   - an unauthenticated request is rejected 401;
//   - a concurrency stress run: N non-admin owners each steer their OWN
//     distinct run concurrently with no cross-talk.
//
// Real drivers across the seam, identity propagation, ≥1 failure mode,
// concurrency stress — the §17 integration-test contract.
func TestE2E_NonAdminToken_SteeringContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		issuer   = "https://idp.example.com"
		audience = "harbor-runtime"
		kid      = "nonadmin-steering-rsa-1"
		tenant   = "tenant-a"
	)

	// The IdP keypair: the private key signs tokens; its public half is
	// published as a JWK Set the Runtime fetches (encoded independently of
	// the parser-under-test via writeRSAJWKS in jwks_serve_test.go).
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	jwksPath := writeRSAJWKS(t, &priv.PublicKey, kid)

	red := patterns.New()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	steerReg := steering.NewRegistry()
	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	validator, err := auth.NewJWKSValidator(ctx, config.IdentityConfig{
		JWTAlgorithms: []string{"RS256"},
		Issuer:        issuer,
		Audience:      audience,
		JWKSFile:      jwksPath,
	}, auth.ValidatorDeps{Redactor: red, Bus: bus})
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	handler, err := control.NewHandler(surface, control.WithEventBus(bus), control.WithRedactor(red))
	if err != nil {
		t.Fatalf("control.NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(control.RoutePattern, handler)
	srv := httptest.NewServer(auth.Middleware(validator)(mux))
	t.Cleanup(srv.Close)

	sign := func(id identity.Identity, scopes []string) string {
		now := time.Now()
		claims := jwt.MapClaims{
			"iss": issuer, "aud": audience, "sub": id.UserID,
			"exp":     now.Add(time.Hour).Unix(),
			"nbf":     now.Add(-time.Minute).Unix(),
			"tenant":  id.TenantID,
			"user":    id.UserID,
			"session": id.SessionID,
		}
		if scopes != nil {
			claims["scopes"] = scopes
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		s, serr := tok.SignedString(priv)
		if serr != nil {
			t.Fatalf("sign: %v", serr)
		}
		return s
	}

	// call submits a control over the wire and returns (status, bodyCode).
	call := func(method, token string, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/"+method, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, derr := srv.Client().Do(req)
		if derr != nil {
			t.Fatalf("request: %v", derr)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &env)
		return resp.StatusCode, env.Code
	}

	// The target run is owned by user-A. Its inbox is live.
	owner := identity.Identity{TenantID: tenant, UserID: "user-A", SessionID: "session-x"}
	run := identity.Quadruple{Identity: owner, RunID: "run-1"}
	if _, err := steerReg.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	ownerBody := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q,"run":%q},"payload":{"note":"hi"}}`,
		owner.TenantID, owner.UserID, owner.SessionID, run.RunID)

	nonAdmin := sign(owner, []string{}) // verified, no scopes — a non-admin token.
	adminTok := sign(owner, []string{"admin"})

	// (1) Non-admin owner CAN inject_context on its own run.
	t.Run("nonadmin_owner_can_inject", func(t *testing.T) {
		status, code := call("inject_context", nonAdmin, ownerBody)
		if status != http.StatusOK {
			t.Fatalf("inject_context by non-admin owner: status=%d code=%q, want 200", status, code)
		}
	})

	// (2) Non-admin owner is REJECTED 403 scope_mismatch on prioritize.
	t.Run("nonadmin_owner_cannot_prioritize", func(t *testing.T) {
		body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q,"run":%q},"payload":{"priority":9}}`,
			owner.TenantID, owner.UserID, owner.SessionID, run.RunID)
		status, code := call("prioritize", nonAdmin, body)
		if status != http.StatusForbidden || code != "scope_mismatch" {
			t.Fatalf("prioritize by non-admin owner: status=%d code=%q, want 403 scope_mismatch", status, code)
		}
	})

	// (3) Admin CAN prioritize the same run — admin is the one path.
	t.Run("admin_can_prioritize", func(t *testing.T) {
		body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q,"run":%q},"payload":{"priority":9}}`,
			owner.TenantID, owner.UserID, owner.SessionID, run.RunID)
		status, code := call("prioritize", adminTok, body)
		if status != http.StatusOK {
			t.Fatalf("prioritize by admin: status=%d code=%q, want 200", status, code)
		}
	})

	// (4) Collision: a non-admin token for user-B (SAME tenant + session-id
	// string) cannot steer user-A's run — the body-vs-JWT transport gate
	// rejects it 401 before authority is even derived.
	t.Run("session_id_collision_rejected", func(t *testing.T) {
		userB := identity.Identity{TenantID: tenant, UserID: "user-B", SessionID: "session-x"}
		collisionTok := sign(userB, []string{})
		// The body names user-A's run (the target); the JWT is user-B.
		status, _ := call("inject_context", collisionTok, ownerBody)
		if status != http.StatusUnauthorized {
			t.Fatalf("collision (user-B steering user-A's run): status=%d, want 401", status)
		}
	})

	// (5) Unauthenticated request is rejected at the auth edge.
	t.Run("unauthenticated_rejected", func(t *testing.T) {
		status, _ := call("inject_context", "", ownerBody)
		if status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated inject_context: status=%d, want 401", status)
		}
	})

	// (6) Concurrency stress — N non-admin owners each steer their OWN
	// distinct run concurrently. Each control must land ONLY on its own
	// inbox (no cross-talk).
	t.Run("concurrent_nonadmin_owners_no_crosstalk", func(t *testing.T) {
		const workers = 16
		inboxes := make([]*steering.Inbox, workers)
		for i := range workers {
			id := identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("u-%02d", i), SessionID: fmt.Sprintf("s-%02d", i)}
			q := identity.Quadruple{Identity: id, RunID: fmt.Sprintf("run-%02d", i)}
			inbox, oerr := steerReg.Open(q)
			if oerr != nil {
				t.Fatalf("Open(%d): %v", i, oerr)
			}
			inboxes[i] = inbox
		}

		var wg sync.WaitGroup
		wg.Add(workers)
		errs := make(chan error, workers)
		for i := range workers {
			go func(i int) {
				defer wg.Done()
				id := identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("u-%02d", i), SessionID: fmt.Sprintf("s-%02d", i)}
				tok := sign(id, []string{})
				body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q,"run":%q},"payload":{"note":"hi"}}`,
					id.TenantID, id.UserID, id.SessionID, fmt.Sprintf("run-%02d", i))
				if status, code := call("inject_context", tok, body); status != http.StatusOK {
					errs <- fmt.Errorf("worker %d: status=%d code=%q", i, status, code)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
		for i := range workers {
			if got := inboxes[i].Len(); got != 1 {
				t.Errorf("inbox %d Len()=%d, want 1 — cross-talk or drop", i, got)
			}
		}
	})
}

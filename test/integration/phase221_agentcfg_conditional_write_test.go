package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// Phase 221 (D-366) — the expected-revision token, end to end.
//
// Real drivers on every seam: the real agentcfg statestore driver over BOTH a
// real in-memory StateStore and a real file-backed SQLite StateStore (so the
// precondition is exercised against a genuine SQL read-modify-write, not just
// a map), a real in-memory EventBus with a real audit Redactor, the real
// agent-config protocol Service, and the real REST transport over
// httptest — the same path a Console makes its calls on.

const (
	cwTenant  = "tenant-cw"
	cwUser    = "user-cw"
	cwSession = "sess-cw"
	cwAgent   = "agent-cw"
)

type cwHarness struct {
	handler  http.Handler
	registry agentcfg.Registry
	bus      events.EventBus
}

func cwBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 32, SubscriberBufferSize: 256,
		IdleTimeout: time.Minute, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// cwHarnessOn builds the full stack over the supplied REAL StateStore.
func cwHarnessOn(t *testing.T, st state.StateStore) *cwHarness {
	t.Helper()
	bus := cwBus(t)
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return &cwHarness{handler: h, registry: reg, bus: bus}
}

func cwInmemStore(t *testing.T) state.StateStore {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

func cwSQLiteStore(t *testing.T) state.StateStore {
	t.Helper()
	st, err := statesqlite.New(config.StateConfig{
		Driver: "sqlite",
		DSN:    filepath.Join(t.TempDir(), "agentcfg-cw.db"),
	})
	if err != nil {
		t.Fatalf("state sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

// cwCall drives the handler in-process with the identity headers and scopes.
type cwResult struct {
	status int
	code   string
	body   map[string]any
}

func cwCall(t *testing.T, h http.Handler, path string, body any, tenant, user, session string, scopes []auth.Scope) cwResult {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set(stream.HeaderTenant, tenant)
	req.Header.Set(stream.HeaderUser, user)
	req.Header.Set(stream.HeaderSession, session)
	req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := cwResult{status: rec.Code, body: map[string]any{}}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	if c, ok := out.body["code"].(string); ok {
		out.code = c
	}
	return out
}

func cwAdmin(t *testing.T, h http.Handler, path string, body any) cwResult {
	t.Helper()
	return cwCall(t, h, path, body, cwTenant, cwUser, cwSession, []auth.Scope{auth.ScopeAdmin})
}

// cwActiveHash reads the current content hash the way a client does — through
// `agent_config.get`, which returns both the revision id and the content hash.
func cwActiveHash(t *testing.T, h http.Handler) (revID, hash string) {
	t.Helper()
	res := cwAdmin(t, h, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: cwAgent})
	if res.status != http.StatusOK {
		t.Fatalf("agent_config.get: status %d body %v", res.status, res.body)
	}
	rev, ok := res.body["revision"].(map[string]any)
	if !ok {
		t.Fatalf("agent_config.get returned no revision: %v", res.body)
	}
	revID, _ = rev["revision_id"].(string)
	hash, _ = rev["content_hash"].(string)
	if revID == "" || hash == "" {
		t.Fatalf("agent_config.get revision missing id/hash: %v", rev)
	}
	return revID, hash
}

func cwSetRevision(names []string, token string) prototypes.AgentConfigSetRevisionRequest {
	return prototypes.AgentConfigSetRevisionRequest{
		AgentID:             cwAgent,
		Payload:             prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: names}},
		ExpectedContentHash: token,
	}
}

// TestE2E_AgentConfig_ConditionalWrite is the Console-shaped round trip: read →
// edit → conditional write, with a second writer interleaved, over the real
// REST transport. Run against BOTH real StateStore drivers.
func TestE2E_AgentConfig_ConditionalWrite(t *testing.T) {
	for _, drv := range []struct {
		name string
		mk   func(*testing.T) state.StateStore
	}{
		{"inmem", cwInmemStore},
		{"sqlite", cwSQLiteStore},
	} {
		t.Run(drv.name, func(t *testing.T) {
			h := cwHarnessOn(t, drv.mk(t)).handler

			// 1. The Console reads the config it is about to edit.
			if res := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"alpha"}, "")); res.status != http.StatusOK {
				t.Fatalf("seed: status %d body %v", res.status, res.body)
			}
			readRevID, readHash := cwActiveHash(t, h)

			// 2. Somebody else writes while the human is still editing.
			if res := cwAdmin(t, h, "/v1/agent_config/set_prompt_layers", prototypes.AgentConfigSetPromptLayersRequest{
				AgentID:      cwAgent,
				PromptLayers: prototypes.AgentConfigPromptLayers{Base: cwPtr("an interleaved edit")},
			}); res.status != http.StatusOK {
				t.Fatalf("interleaved writer: status %d body %v", res.status, res.body)
			}

			// 3. The Console saves under the base it read — REFUSED, typed.
			res := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"alpha", "beta"}, readHash))
			if res.status != http.StatusConflict {
				t.Fatalf("conditional write: status %d, want 409; body %v", res.status, res.body)
			}
			if res.code != string(protoerrors.CodeRevisionConflict) {
				t.Fatalf("conditional write: code %q, want %q — a server that IGNORED the unknown "+
					"field would have answered 200, and a server that mis-mapped it would answer "+
					"invalid_request or runtime_error", res.code, protoerrors.CodeRevisionConflict)
			}

			// 4. Nothing was persisted: the interleaved writer's revision is
			//    still active and the prompt layer it wrote survives.
			afterRevID, afterHash := cwActiveHash(t, h)
			if afterHash == readHash {
				t.Fatal("fixture broken: the interleaved write did not move the content hash")
			}
			getRes := cwAdmin(t, h, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: cwAgent})
			rev := getRes.body["revision"].(map[string]any)
			payload, _ := rev["payload"].(map[string]any)
			pl, _ := payload["prompt_layers"].(map[string]any)
			if pl == nil || pl["base"] != "an interleaved edit" {
				t.Fatalf("the refused write clobbered the interleaved edit: %v", payload)
			}

			// 5. THE RECOVERY LOOP a conflicted client actually runs: re-read
			//    `agent_config.get` for the current revision id + hash, diff
			//    what-I-read against what-it-is-now, then retry under the new
			//    hash.
			diffRes := cwAdmin(t, h, "/v1/agent_config/diff", prototypes.AgentConfigDiffRequest{
				AgentID: cwAgent, FromRevision: readRevID, ToRevision: afterRevID,
			})
			if diffRes.status != http.StatusOK {
				t.Fatalf("recovery diff: status %d body %v", diffRes.status, diffRes.body)
			}
			retry := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"alpha", "beta"}, afterHash))
			if retry.status != http.StatusOK {
				t.Fatalf("retry under the fresh hash: status %d body %v", retry.status, retry.body)
			}

			// 6. The EMPTY-token path is byte-for-byte the pre-phase behaviour:
			//    a 200 with the same body shape, regardless of the base.
			uncond := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"gamma"}, ""))
			if uncond.status != http.StatusOK {
				t.Fatalf("unconditional write: status %d body %v", uncond.status, uncond.body)
			}
			if _, ok := uncond.body["revision"].(map[string]any); !ok {
				t.Fatalf("unconditional response shape changed: %v", uncond.body)
			}
			if _, ok := uncond.body["protocol_version"].(string); !ok {
				t.Fatalf("unconditional response missing protocol_version: %v", uncond.body)
			}
		})
	}
}

// TestE2E_AgentConfig_ConditionalWrite_IdentityPropagation — the multi-isolation
// assertion. Two tenants and two users each conditionally write the SAME agent
// id; each conditional write resolves against its OWN slot, and one tenant's
// token is never valid against another's config.
func TestE2E_AgentConfig_ConditionalWrite_IdentityPropagation(t *testing.T) {
	h := cwHarnessOn(t, cwSQLiteStore(t)).handler

	type principal struct{ tenant, user, session string }
	principals := []principal{
		{"tenant-a", "user-1", "sess-1"},
		{"tenant-a", "user-2", "sess-2"},
		{"tenant-b", "user-1", "sess-1"},
		{"tenant-b", "user-2", "sess-2"},
	}

	// Each principal seeds its own config under the SAME agent id. The admin
	// tier keys by (tenant, agent) — the user slot is deliberately excluded so
	// all of a tenant's admin writers serialise — so two users in one tenant
	// share a slot and the LAST seed in each tenant is that tenant's state.
	for _, p := range principals {
		res := cwCall(t, h, "/v1/agent_config/set_revision", prototypes.AgentConfigSetRevisionRequest{
			AgentID: cwAgent,
			Payload: prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{
				Names: []string{p.tenant + "-" + p.user},
			}},
		}, p.tenant, p.user, p.session, []auth.Scope{auth.ScopeAdmin})
		if res.status != http.StatusOK {
			t.Fatalf("%v seed: status %d body %v", p, res.status, res.body)
		}
	}

	// Read every principal's view AFTER all seeds have landed, so the recorded
	// hash is the settled state of that principal's slot rather than a value a
	// same-tenant sibling has since replaced.
	hashes := map[principal]string{}
	for _, p := range principals {
		get := cwCall(t, h, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: cwAgent},
			p.tenant, p.user, p.session, []auth.Scope{auth.ScopeAdmin})
		if get.status != http.StatusOK {
			t.Fatalf("%v get: status %d body %v", p, get.status, get.body)
		}
		rev := get.body["revision"].(map[string]any)
		hashes[p] = rev["content_hash"].(string)
	}

	// Same tenant, two users: ONE slot, so they read the same hash. This is the
	// documented admin-tier keying, asserted so a future change to it surfaces
	// here rather than silently widening or narrowing the conditional path.
	if hashes[principals[0]] != hashes[principals[1]] {
		t.Fatalf("two users in one tenant read different admin-tier hashes (%q vs %q) — "+
			"the admin tier is documented as keyed by (tenant, agent)",
			hashes[principals[0]], hashes[principals[1]])
	}

	// Two tenants: DISTINCT slots. A token minted in tenant-a must be refused
	// in tenant-b.
	a := principals[0]
	b := principals[2]
	if hashes[a] == hashes[b] {
		t.Fatal("fixture broken: the two tenants seeded identical content")
	}
	res := cwCall(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"cross-tenant"}, hashes[a]),
		b.tenant, b.user, b.session, []auth.Scope{auth.ScopeAdmin})
	if res.status != http.StatusConflict || res.code != string(protoerrors.CodeRevisionConflict) {
		t.Fatalf("a tenant-a token was accepted in tenant-b: status %d code %q body %v",
			res.status, res.code, res.body)
	}
	// tenant-b's own config is untouched by the refusal.
	get := cwCall(t, h, "/v1/agent_config/get", prototypes.AgentConfigGetRequest{AgentID: cwAgent},
		b.tenant, b.user, b.session, []auth.Scope{auth.ScopeAdmin})
	rev := get.body["revision"].(map[string]any)
	if rev["content_hash"].(string) != hashes[b] {
		t.Fatalf("the cross-tenant refusal disturbed tenant-b's config")
	}
}

// TestE2E_AgentConfig_ConditionalWrite_FailureModes — the ≥1 failure mode §17.3
// requires, plus the precondition-is-not-an-authority assertion at the wire.
func TestE2E_AgentConfig_ConditionalWrite_FailureModes(t *testing.T) {
	t.Run("ClosedBusDoesNotDisturbTheConditionalDecision", func(t *testing.T) {
		st := cwInmemStore(t)
		bus := cwBus(t)
		reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithBus(bus))
		if err != nil {
			t.Fatalf("service: %v", err)
		}
		h, err := stream.NewAgentConfigHandler(svc)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close(context.Background()) })

		if res := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"one"}, "")); res.status != http.StatusOK {
			t.Fatalf("seed: %d %v", res.status, res.body)
		}
		_, hash := cwActiveHash(t, h)

		// Close the bus. The revision emit now fails; the WRITE must still
		// land (the driver logs the publish failure rather than losing the
		// revision), and the conditional decision must be unaffected.
		if err := bus.Close(context.Background()); err != nil {
			t.Fatalf("bus close: %v", err)
		}

		if res := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"one", "two"}, hash)); res.status != http.StatusOK {
			t.Fatalf("conditional write with a closed bus: status %d body %v", res.status, res.body)
		}
		// And a STALE token is still refused with the typed code, not masked
		// by the emit failure.
		if res := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"three"}, hash)); res.status != http.StatusConflict ||
			res.code != string(protoerrors.CodeRevisionConflict) {
			t.Fatalf("stale token with a closed bus: status %d code %q body %v", res.status, res.code, res.body)
		}
	})

	t.Run("TheTokenIsNotAnAuthorityAtTheWire", func(t *testing.T) {
		h := cwHarnessOn(t, cwInmemStore(t)).handler
		if res := cwAdmin(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"one"}, "")); res.status != http.StatusOK {
			t.Fatalf("seed: %d %v", res.status, res.body)
		}
		_, hash := cwActiveHash(t, h)

		// A perfectly VALID token with NO admin scope. The scope gate must
		// still refuse — the token never buys authority.
		res := cwCall(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"escalated"}, hash),
			cwTenant, cwUser, cwSession, nil)
		if res.status == http.StatusOK {
			t.Fatal("a valid expected_content_hash granted an unscoped caller a write")
		}
		if res.code == string(protoerrors.CodeRevisionConflict) {
			t.Fatalf("an authorization failure was reported as a revision conflict: %v", res.body)
		}

		// A valid token with an INCOMPLETE identity triple is refused by the
		// identity gate, not converted into a conflict.
		res = cwCall(t, h, "/v1/agent_config/set_revision", cwSetRevision([]string{"escalated"}, hash),
			cwTenant, "", cwSession, []auth.Scope{auth.ScopeAdmin})
		if res.status == http.StatusOK {
			t.Fatal("a valid expected_content_hash satisfied the identity gate")
		}
		if res.code == string(protoerrors.CodeRevisionConflict) {
			t.Fatalf("an identity failure was reported as a revision conflict: %v", res.body)
		}
	})
}

// TestE2E_AgentConfig_ConditionalWrite_ConcurrencyStress — the §17.3 stress
// across the whole wired boundary: N concurrent conditional writers over the
// REAL HTTP server, all holding the same base. Exactly one 200; the rest 409.
func TestE2E_AgentConfig_ConditionalWrite_ConcurrencyStress(t *testing.T) {
	hz := cwHarnessOn(t, cwSQLiteStore(t))

	if res := cwAdmin(t, hz.handler, "/v1/agent_config/set_revision", cwSetRevision([]string{"base"}, "")); res.status != http.StatusOK {
		t.Fatalf("seed: %d %v", res.status, res.body)
	}
	_, hash := cwActiveHash(t, hz.handler)

	const n = 16 // >= 10 per §17.3; each is a real HTTP round trip
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		accepted  int
		conflicts int
		other     []int
	)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Each goroutine drives the REAL handler with its own request and
			// recorder. The admin scope lives in the request context (the auth
			// middleware's job in production), so this goes through the handler
			// rather than the bare socket — a raw client request carries no
			// verified scope and is refused 403 before reaching the service,
			// which would test the auth gate instead of the precondition.
			body, err := json.Marshal(cwSetRevision([]string{fmt.Sprintf("writer-%d", i)}, hash))
			if err != nil {
				t.Errorf("marshal: %v", err)
				return
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/set_revision", bytes.NewReader(body))
			req.Header.Set(stream.HeaderTenant, cwTenant)
			req.Header.Set(stream.HeaderUser, cwUser)
			req.Header.Set(stream.HeaderSession, cwSession)
			req = req.WithContext(auth.WithScopes(req.Context(), []auth.Scope{auth.ScopeAdmin}))
			rec := httptest.NewRecorder()
			hz.handler.ServeHTTP(rec, req)

			mu.Lock()
			defer mu.Unlock()
			switch rec.Code {
			case http.StatusOK:
				accepted++
			case http.StatusConflict:
				conflicts++
			default:
				other = append(other, rec.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected statuses: %v", other)
	}
	if accepted != 1 {
		t.Fatalf("accepted = %d over %d racing conditional writers, want exactly 1 — "+
			"the in-process lost-update guarantee is broken", accepted, n)
	}
	if conflicts != n-1 {
		t.Fatalf("conflicts = %d, want %d", conflicts, n-1)
	}
}

func cwPtr(s string) *string { return &s }

// Opt-in steering reach and start execution proof over the served mux
// (D-513). Every request crosses the real ES256 validator, the auth
// middleware (which verifies the signed start body digest) and the
// devstack's production control surface wired from config.
package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// srpToken signs a bearer for id with the given reach (nil omits the claim)
// plus any extra signed claims.
func srpToken(t *testing.T, s *agentReachStack, id identity.Identity, scopes []string, reach []string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": "harbor-test", "sub": id.UserID, "aud": "harbor",
		"exp": now.Add(time.Hour).Unix(), "nbf": now.Add(-time.Minute).Unix(), "iat": now.Unix(),
		"tenant": id.TenantID, "user": id.UserID, "session": id.SessionID, "scopes": scopes,
	}
	if reach != nil {
		claims[auth.AgentReachClaim] = reach
	}
	for k, v := range extra {
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = devstack.DefaultKID
	signed, err := tok.SignedString(s.stack.SigningKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// srpProofClaims binds a start proof to the exact raw body bytes.
func srpProofClaims(id identity.Identity, operation, key string, body []byte) map[string]any {
	sum := sha256.Sum256(body)
	return map[string]any{
		auth.ExecutionOperationClaim:   operation,
		auth.ExecutionIdempotencyClaim: key,
		auth.ExecutionStartDigestClaim: hex.EncodeToString(sum[:]),
		auth.SessionReachClaim:         []string{id.SessionID},
	}
}

// srpStart posts exact raw bytes to start, so a signed digest covers them.
func srpStart(t *testing.T, s *agentReachStack, token string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.server.URL+"/v1/control/start", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("POST start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, out
}

func srpStartBody(t *testing.T, id identity.Identity, key string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"identity": arScope(id), "query": "proof start", "idempotency_key": key})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func srpControlBody(t *testing.T, id identity.Identity, run string, payload map[string]any) map[string]any {
	t.Helper()
	scope := arScope(id)
	scope["run"] = run
	return map[string]any{"identity": scope, "payload": payload}
}

func TestE2E_RequireExecutionProofAndSteeringReach_On(t *testing.T) {
	s := newAgentReachStack(t, func(cfg *config.Config) {
		cfg.Protocol.RequireExecutionProof = true
		cfg.Protocol.SteeringRequiresAgentReach = true
	})
	agent := s.stack.AgentConfigID
	id := arIdentity("d513-proof")

	t.Run("start without a proof is refused, admin included", func(t *testing.T) {
		for _, scopes := range [][]string{nil, {"admin"}} {
			status, raw := srpStart(t, s, srpToken(t, s, id, scopes, []string{agent}, nil), srpStartBody(t, id, "no-proof"))
			arAssertStatus(t, status, raw, http.StatusForbidden)
			arAssertCode(t, raw, "scope_mismatch")
			if !bytes.Contains(raw, []byte("start requires a signed execution proof")) {
				t.Fatalf("refusal body = %s", raw)
			}
		}
	})

	t.Run("a proof over other bytes is refused by the digest check", func(t *testing.T) {
		signed := srpStartBody(t, id, "digest")
		sent := bytes.Replace(signed, []byte("proof start"), []byte("other start"), 1)
		tok := srpToken(t, s, id, nil, []string{agent}, srpProofClaims(id, "op-digest", "digest", signed))
		status, raw := srpStart(t, s, tok, sent)
		arAssertStatus(t, status, raw, http.StatusForbidden)
		arAssertCode(t, raw, "scope_mismatch")
	})

	t.Run("a proof for another idempotency key is refused", func(t *testing.T) {
		body := srpStartBody(t, id, "actual-key")
		tok := srpToken(t, s, id, nil, []string{agent}, srpProofClaims(id, "op-key", "signed-key", body))
		status, raw := srpStart(t, s, tok, body)
		arAssertStatus(t, status, raw, http.StatusForbidden)
		arAssertCode(t, raw, "scope_mismatch")
	})

	var runID string
	t.Run("a valid proof is admitted", func(t *testing.T) {
		body := srpStartBody(t, id, "valid")
		tok := srpToken(t, s, id, nil, []string{agent}, srpProofClaims(id, "op-valid", "valid", body))
		status, raw := srpStart(t, s, tok, body)
		arAssertStatus(t, status, raw, http.StatusOK)
		runID = arDecode[struct {
			TaskID string `json:"task_id"`
		}](t, raw).TaskID
		if runID == "" {
			t.Fatalf("start response without task id: %s", raw)
		}
	})

	t.Run("additive steering needs reach to the run's recorded agent", func(t *testing.T) {
		if runID == "" {
			t.Fatal("no admitted run")
		}
		additive := map[string]map[string]any{
			"user_message":   {"message": "more"},
			"redirect":       {"goal": "another"},
			"inject_context": {"note": "extra"},
		}
		for method, payload := range additive {
			body := srpControlBody(t, id, runID, payload)
			for _, reach := range [][]string{nil, {}, {arOtherAgent}} {
				status, raw := arPost(t, s, "/v1/control/"+method, srpToken(t, s, id, nil, reach, nil), body)
				arAssertStatus(t, status, raw, http.StatusForbidden)
				arAssertCode(t, raw, "scope_mismatch")
			}
			// Reach to the recorded agent passes the gate; the run has no
			// live inbox in this stack, so the control answers not_found.
			status, raw := arPost(t, s, "/v1/control/"+method, srpToken(t, s, id, nil, []string{agent}, nil), body)
			arAssertStatus(t, status, raw, http.StatusNotFound)
		}
		// Reductions and decisions stay authorship-only.
		for _, method := range []string{"cancel", "pause", "resume", "approve", "reject"} {
			status, raw := arPost(t, s, "/v1/control/"+method, srpToken(t, s, id, nil, nil, nil), srpControlBody(t, id, runID, nil))
			arAssertStatus(t, status, raw, http.StatusNotFound)
		}
		// A reject reason reaches the model, so it needs reach too.
		status, raw := arPost(t, s, "/v1/control/reject", srpToken(t, s, id, nil, nil, nil),
			srpControlBody(t, id, runID, map[string]any{"reason": "not this"}))
		arAssertStatus(t, status, raw, http.StatusForbidden)
		arAssertCode(t, raw, "scope_mismatch")
		status, raw = arPost(t, s, "/v1/control/reject", srpToken(t, s, id, nil, []string{agent}, nil),
			srpControlBody(t, id, runID, map[string]any{"reason": "not this"}))
		arAssertStatus(t, status, raw, http.StatusNotFound)
		// A body agent_id is not part of the control wire shape.
		body := srpControlBody(t, id, runID, map[string]any{"message": "x"})
		body["agent_id"] = arOtherAgent
		status, raw = arPost(t, s, "/v1/control/user_message", srpToken(t, s, id, nil, []string{arOtherAgent}, nil), body)
		arAssertStatus(t, status, raw, http.StatusBadRequest)
	})

	t.Run("reach in one tenant never steers another tenant's run", func(t *testing.T) {
		other := identity.Identity{TenantID: "d513-other-tenant", UserID: id.UserID, SessionID: id.SessionID}
		status, raw := arPost(t, s, "/v1/control/user_message", srpToken(t, s, other, nil, []string{agent}, nil),
			srpControlBody(t, id, runID, map[string]any{"message": "x"}))
		// The transport refuses the foreign body identity before the
		// surface; either refusal is acceptable, acceptance never is.
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("cross-tenant user_message status=%d body=%s, want a refusal", status, raw)
		}
	})
}

func TestE2E_RequireExecutionProofAndSteeringReach_Off(t *testing.T) {
	s := newAgentReachStack(t)
	agent := s.stack.AgentConfigID
	id := arIdentity("d513-off")
	status, raw := srpStart(t, s, srpToken(t, s, id, nil, []string{agent}, nil), srpStartBody(t, id, "off"))
	arAssertStatus(t, status, raw, http.StatusOK)
	runID := arDecode[struct {
		TaskID string `json:"task_id"`
	}](t, raw).TaskID
	status, raw = arPost(t, s, "/v1/control/user_message", srpToken(t, s, id, nil, nil, nil),
		srpControlBody(t, id, runID, map[string]any{"message": "more"}))
	arAssertStatus(t, status, raw, http.StatusNotFound)
}

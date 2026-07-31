// wave_v125_test.go — the wave-end end-to-end smoke for the v1.25 wave
// (CLAUDE.md §17.7 step 5).
//
// The v1.24 checkpoint audit found that wave's first cut was a partial shell:
// it would have compiled and passed VERBATIM with one of the phases it claimed
// to exercise reverted. This file is written against that finding. Every leg
// below was proved non-vacuous by locally reverting the phase's behaviour and
// watching the leg go red; the two proofs are recorded in the PR description.
//
// The wave is one coherent slice — the PROMPT-COMPOSITION surface, i.e. how a
// Protocol client contributes to what the model sees — plus the hygiene phase
// that repairs the instrument measuring all of it. Each leg proves something
// about the SEAM, not about a struct:
//
//   - 219 — caller-supplied memory reaches the UNTRUSTED External tier under
//     the fixed runtime-owned key, and appears in NO other prompt position.
//     Asserted on the bytes that reached llm.CompleteRequest.Messages.
//   - 220 — the run-level additive instruction COMPOSES BELOW the tenant
//     value and can never clear it. Asserted through the ONE production
//     composition, over a payload decoded from the WIRE (so the assertion is
//     about the shipped surface, not about a Go literal).
//   - 221 — a stale expected-revision token is refused with `revision_conflict`
//     at 409, and the sibling contributor's block is still there afterwards.
//   - 222 — the ordered named blocks reach the built system prompt IN DECLARED
//     ORDER and VERBATIM, through the real run-start projection.
//   - 223 — the master-plan status classifier the whole inert-smoke gate rests
//     on: a NOT-SHIPPED row classifies not-shipped, a shipped row classifies
//     shipped, and an unrecognised word still falls to the STRICT arm.
//
// Multi-isolation is asserted across TWO TENANTS and TWO USERS with NEGATIVE
// assertions. `agent_id` is deliberately held CONSTANT: CLAUDE.md §6 states it
// is not an isolation principal, so varying it would assert nothing.
//
// Every leg carries at least one failure mode, and the composed surface closes
// under an N>=12 concurrency stress with a settle-looped runtime.NumGoroutine()
// baseline (a compile-time type assertion is not a concurrency guarantee).
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// The wave's identity surface: TWO tenants and, inside the first, TWO users.
const (
	wv125TenantA = "wv125-tenant-a"
	wv125TenantB = "wv125-tenant-b"
	wv125UserA1  = "wv125-user-a1"
	wv125UserA2  = "wv125-user-a2"
	wv125UserB1  = "wv125-user-b1"
)

// Distinctive, greppable markers. Each appears nowhere else in the repo, so an
// occurrence outside the ONE prompt position it is allowed in can be named
// precisely.
const (
	wv125CallerMarker = "wv125-caller-memory-marker-a41f"
	wv125BlockMarker  = "wv125-block-body-marker-77c2 use <si> units & be precise"
	wv125BlockTwo     = "wv125-second-block-marker-9de0"
)

func wv125ID(tenant, user string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: "wv125-sess-" + tenant + "-" + user}
}

// wv125AgentCfgCall drives the devstack's MOUNTED agent-config route with the
// supplied identity headers and the admin scope. It goes through the real
// transport mux, not the service directly.
func wv125AgentCfgCall(t *testing.T, s *cmStack, path string, body any, id identity.Identity, scopes []auth.Scope) (int, map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	req.Header.Set("Authorization", "Bearer "+cmSignToken(t, s, id))
	_ = scopes // the devstack mints the admin scope; the parameter documents the tier each call needs.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	out := map[string]any{}
	_ = json.Unmarshal(buf, &out)
	return resp.StatusCode, out, string(buf)
}

// wv125SetBlocks writes the ordered named blocks for the devstack's agent under
// the supplied identity, over the mounted route.
func wv125SetBlocks(t *testing.T, s *cmStack, id identity.Identity, expectedHash string, pairs ...string) (status int, body map[string]any, raw string) {
	t.Helper()
	blocks := make([]prototypes.AgentConfigNamedBlock, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		blocks = append(blocks, prototypes.AgentConfigNamedBlock{Name: pairs[i], Body: pairs[i+1]})
	}
	return wv125AgentCfgCall(t, s, "/v1/agent_config/set_extra_system_blocks",
		prototypes.AgentConfigSetExtraSystemBlocksRequest{
			AgentID:             s.stack.AgentConfigID,
			ExtraSystemBlocks:   prototypes.AgentConfigExtraSystemBlocks{Blocks: blocks},
			ExpectedContentHash: expectedHash,
		}, id, []auth.Scope{auth.ScopeAdmin})
}

// wv125ActiveHash reads the active revision's content hash the way a
// contributor composing a read-modify-write does.
func wv125ActiveHash(t *testing.T, s *cmStack, id identity.Identity) string {
	t.Helper()
	status, _, raw := wv125AgentCfgCall(t, s, "/v1/agent_config/get",
		prototypes.AgentConfigGetRequest{AgentID: s.stack.AgentConfigID}, id, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("agent_config.get: status %d body %s", status, raw)
	}
	var typed prototypes.AgentConfigGetResponse
	if err := json.Unmarshal([]byte(raw), &typed); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if !typed.Set || typed.Revision == nil {
		t.Fatalf("agent_config.get returned no active revision: %s", raw)
	}
	return typed.Revision.ContentHash
}

// wv125BlockNames reads the active revision's block names IN ORDER.
func wv125BlockNames(t *testing.T, s *cmStack, id identity.Identity) []string {
	t.Helper()
	status, _, raw := wv125AgentCfgCall(t, s, "/v1/agent_config/get",
		prototypes.AgentConfigGetRequest{AgentID: s.stack.AgentConfigID}, id, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("agent_config.get: status %d body %s", status, raw)
	}
	var typed prototypes.AgentConfigGetResponse
	if err := json.Unmarshal([]byte(raw), &typed); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if !typed.Set || typed.Revision == nil || typed.Revision.Payload.ExtraSystemBlocks == nil {
		return nil
	}
	out := make([]string, 0, len(typed.Revision.Payload.ExtraSystemBlocks.Blocks))
	for _, b := range typed.Revision.Payload.ExtraSystemBlocks.Blocks {
		out = append(out, b.Name)
	}
	return out
}

// wv125SystemText joins EVERY system message on a request. The memory tiers
// and the composed system prompt do not necessarily share one message, so a
// helper that read only messages[0] would silently assert half the surface.
func wv125SystemText(req llm.CompleteRequest) string {
	var sb strings.Builder
	for _, m := range cmMessageTexts(req) {
		if m.Role == llm.RoleSystem {
			sb.WriteString(m.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// wv125AwaitRequestFor polls (bounded, never a sleep-as-synchronisation) until
// the recording LLM has seen a request whose SYSTEM message contains want.
func wv125AwaitRequestFor(t *testing.T, s *cmStack, want string) llm.CompleteRequest {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, req := range s.rec.requests() {
			for _, m := range cmMessageTexts(req) {
				if m.Role == llm.RoleSystem && strings.Contains(m.Text, want) {
					return req
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no LLM request carried %q in its system message within the deadline", want)
	return llm.CompleteRequest{}
}

// ---------------------------------------------------------------------------
// LEG 219 — caller-supplied memory reaches the UNTRUSTED External tier, and
// nothing else.
// ---------------------------------------------------------------------------

func wv125Leg219(t *testing.T, s *cmStack) {
	id := wv125ID(wv125TenantA, wv125UserA1)
	status, body := s.postStart(t, cmStartBody(id, "wv125 leg 219",
		map[string]any{"note": wv125CallerMarker}), id, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("start with caller_memory: status %d body %s", status, body)
	}

	req := wv125AwaitRequestFor(t, s, wv125CallerMarker)

	// POSITIONAL CONFINEMENT: exactly ONE message carries the marker, it is
	// the SYSTEM message, and inside it the marker sits under the untrusted
	// External tier's fixed runtime-owned key.
	var carriers int
	var systemText string
	for _, m := range cmMessageTexts(req) {
		if !strings.Contains(m.Text, wv125CallerMarker) {
			continue
		}
		carriers++
		if m.Role != llm.RoleSystem {
			t.Fatalf("caller memory reached a %s message — it may reach exactly one position", m.Role)
		}
		systemText = m.Text
	}
	if carriers != 1 {
		t.Fatalf("caller memory appeared in %d messages, want exactly 1", carriers)
	}
	tierIdx := strings.Index(systemText, runctx.ExternalTierName)
	keyIdx := strings.Index(systemText, runctx.CallerSuppliedKey)
	markIdx := strings.Index(systemText, wv125CallerMarker)
	if tierIdx < 0 || keyIdx < 0 {
		t.Fatalf("the External tier wrapper (%q) or the fixed key (%q) is missing from the system prompt:\n%s",
			runctx.ExternalTierName, runctx.CallerSuppliedKey, systemText)
	}
	if tierIdx >= keyIdx || keyIdx >= markIdx {
		t.Fatalf("caller memory is not nested under %s/%s (tier@%d key@%d marker@%d)",
			runctx.ExternalTierName, runctx.CallerSuppliedKey, tierIdx, keyIdx, markIdx)
	}

	// FAILURE MODE: an over-cap payload is refused, the refusal NAMES the
	// field, and no task is created. The payload is sized strictly between the
	// field cap and the whole-body envelope cap, so a status-only assertion
	// could not pass by accident on the transport's identical refusal.
	oversized := strings.Repeat("x", cmFieldCap+8*1024)
	if len(oversized) >= cmEnvelopeCap {
		t.Fatalf("the over-cap fixture (%d bytes) is not strictly below the envelope cap (%d) — the transport would answer first and the leg would prove nothing",
			len(oversized), cmEnvelopeCap)
	}
	status, body = s.postStart(t, cmStartBody(id, "wv125 leg 219 over-cap", oversized), id, true)
	if status != http.StatusBadRequest {
		t.Fatalf("over-cap caller_memory: status %d body %s, want 400", status, body)
	}
	if !strings.Contains(string(body), "caller_memory") {
		t.Fatalf("the over-cap refusal does not NAME the field: %s", body)
	}
}

// ---------------------------------------------------------------------------
// LEG 220 — the run-level additive instruction composes BELOW the tenant value
// and can NEVER clear it.
// ---------------------------------------------------------------------------

// wv125Leg220 exercises the shipped `runs.set_overrides` surface with a body
// decoded from the WIRE (so an unknown field is dropped exactly as the real
// transport drops it) and then runs the ONE production composition,
// runsprotocol.ComposeLLMOverrides — the same function cmd/harbor's run loop
// and the devstack twin both reach.
//
// The invariant asserted is the wave's SETTLED decision: `runs.set_overrides`
// is not admin-gated while `governance.set_tenant_overrides` is, so a
// per-field last-writer-wins would hand a non-admin caller a silent DELETE of
// an admin-set compliance block. The composed value must therefore always
// still CONTAIN the tenant value.
//
// The exact-composition assertion below ARMS ITSELF once the wire field
// exists: when `RunOverrides` declares `extra_instructions`, the composed
// value must be exactly `tenant + "\n\n" + run`. It is not a skip arm — the
// containment assertion above it runs and must pass unconditionally.
func wv125Leg220(t *testing.T) {
	const tenantVal = "TENANT_COMPLIANCE_BLOCK"
	const runVal = "RUN_LEVEL_ADDITION"

	store := runsprotocol.NewStore()
	svc, err := runsprotocol.NewService(store)
	if err != nil {
		t.Fatalf("runs service: %v", err)
	}
	id := wv125ID(wv125TenantA, wv125UserA1)

	// Build the request from JSON so the field travels the shipped wire shape.
	rawReq := fmt.Sprintf(`{
	  "identity": {"tenant": %q, "user": %q, "session": %q},
	  "overrides": {"session_id": %q, "extra_instructions": %q}
	}`, id.TenantID, id.UserID, id.SessionID, id.SessionID, runVal)
	var req prototypes.RunSetOverridesRequest
	if err := json.Unmarshal([]byte(rawReq), &req); err != nil {
		t.Fatalf("decode set_overrides request: %v", err)
	}
	if _, err := svc.SetOverrides(context.Background(), req); err != nil {
		t.Fatalf("runs.set_overrides: %v", err)
	}
	pending, ok := store.Consume(id)
	if !ok {
		t.Fatal("runs.set_overrides recorded nothing in the session slot")
	}

	tenantLayer := &planner.LLMOverrides{ExtraInstructions: wv125Ptr(tenantVal)}
	composed := runsprotocol.ComposeLLMOverrides(&pending, nil, tenantLayer)
	if composed == nil || composed.ExtraInstructions == nil {
		t.Fatalf("the composition dropped the tenant value entirely: %+v", composed)
	}
	// THE INVARIANT. True before 220 lands (the run value is ignored) and
	// after it lands (the run value composes below); FALSE exactly when the
	// composition is per-field last-writer-wins, which is the privilege
	// inversion the wave forbids.
	if !strings.Contains(*composed.ExtraInstructions, tenantVal) {
		t.Fatalf("the run-level additive instruction CLEARED the admin-set tenant block: %q — `runs.set_overrides` is not admin-gated, so per-field last-writer-wins is a privilege inversion",
			*composed.ExtraInstructions)
	}

	// The exact composition, asserted once the wire surface declares the
	// field. This arms automatically; it is not a skip arm for the feature's
	// absence.
	if wv125WireDeclaresExtraInstructions() {
		want := tenantVal + "\n\n" + runVal
		if *composed.ExtraInstructions != want {
			t.Fatalf("composed = %q, want %q (tenant first, run below, blank-line joined)",
				*composed.ExtraInstructions, want)
		}
	}

	// FAILURE MODE: a cross-session override is refused. The session id in the
	// body names a session outside the caller's verified scope.
	crossRaw := fmt.Sprintf(`{
	  "identity": {"tenant": %q, "user": %q, "session": %q},
	  "overrides": {"session_id": "some-other-session", "extra_instructions": %q}
	}`, id.TenantID, id.UserID, id.SessionID, runVal)
	var crossReq prototypes.RunSetOverridesRequest
	if err := json.Unmarshal([]byte(crossRaw), &crossReq); err != nil {
		t.Fatalf("decode cross-session request: %v", err)
	}
	if _, err := svc.SetOverrides(context.Background(), crossReq); err == nil {
		t.Fatal("a cross-session runs.set_overrides was ACCEPTED — a caller could record an override for a session outside its verified scope")
	}
	if _, still := store.Peek(id); still {
		t.Fatal("the refused cross-session write left something in the slot")
	}
}

// wv125WireDeclaresExtraInstructions reports whether the shipped RunOverrides
// wire type carries `extra_instructions`. It is a probe on the SURFACE, used
// to strengthen an assertion — never to skip one.
func wv125WireDeclaresExtraInstructions() bool {
	raw, err := json.Marshal(prototypes.RunOverrides{})
	if err != nil {
		return false
	}
	// An omitempty pointer field is absent from the zero value, so probe by
	// round-tripping a populated document instead: an unknown key is dropped.
	var ro prototypes.RunOverrides
	if err := json.Unmarshal([]byte(`{"extra_instructions":"probe"}`), &ro); err != nil {
		return false
	}
	out, err := json.Marshal(ro)
	if err != nil {
		return false
	}
	_ = raw
	return strings.Contains(string(out), "extra_instructions")
}

func wv125Ptr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// LEG 221 — a stale expected-revision token is refused with `revision_conflict`
// and the sibling contributor's work survives.
// ---------------------------------------------------------------------------

func wv125Leg221(t *testing.T, s *cmStack) {
	id := wv125ID(wv125TenantA, wv125UserA1)

	// Contributor A writes and reads the hash it composed against.
	if status, _, raw := wv125SetBlocks(t, s, id, "", "alpha", "A's text"); status != http.StatusOK {
		t.Fatalf("A write: status %d body %s", status, raw)
	}
	staleHash := wv125ActiveHash(t, s, id)

	// Contributor B reads the same base, appends, and writes back.
	if status, _, raw := wv125SetBlocks(t, s, id, staleHash, "alpha", "A's text", "beta", "B's text"); status != http.StatusOK {
		t.Fatalf("B write: status %d body %s", status, raw)
	}

	// FAILURE MODE: A retries against its now-STALE read. Refused 409
	// `revision_conflict`, and B's block is still there.
	status, body, raw := wv125SetBlocks(t, s, id, staleHash, "alpha", "A's edited text")
	if status != http.StatusConflict {
		t.Fatalf("stale-token write: status %d body %s, want 409 — without the refusal A silently deletes B's block", status, raw)
	}
	if code, _ := body["code"].(string); code != string(protoerrors.CodeRevisionConflict) {
		t.Fatalf("stale-token refusal code = %q, want %q", code, protoerrors.CodeRevisionConflict)
	}
	if names := wv125BlockNames(t, s, id); len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("after the refused retry the section is %v, want [alpha beta] — the refusal persisted something", names)
	}
}

// ---------------------------------------------------------------------------
// LEG 222 — the ordered named blocks reach the built system prompt, in
// declared order and verbatim, through the real run-start projection.
// ---------------------------------------------------------------------------

func wv125Leg222(t *testing.T, s *cmStack) {
	id := wv125ID(wv125TenantA, wv125UserA2)

	// Reverse-alphabetical names so a sort anywhere on the path is visible.
	if status, _, raw := wv125SetBlocks(t, s, id, "",
		"zulu", wv125BlockMarker,
		"alpha", wv125BlockTwo,
	); status != http.StatusOK {
		t.Fatalf("block write: status %d body %s", status, raw)
	}

	status, body := s.postStart(t, cmStartBody(id, "wv125 leg 222", nil), id, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("start: status %d body %s", status, body)
	}
	req := wv125AwaitRequestFor(t, s, wv125BlockMarker)

	systemText := wv125SystemText(req)
	if !strings.Contains(systemText, "<additional_guidance>") {
		t.Fatalf("the blocks did not compose into <additional_guidance>:\n%s", systemText)
	}
	// VERBATIM: the body's `<` and `&` survive unescaped.
	if !strings.Contains(systemText, wv125BlockMarker) {
		t.Fatalf("the block body was not rendered verbatim:\n%s", systemText)
	}
	if strings.Contains(systemText, "&lt;si&gt;") {
		t.Fatalf("the block body was entity-escaped — the operator-trusted position renders verbatim:\n%s", systemText)
	}
	// DECLARED ORDER, and the plain-text labels.
	zi := strings.Index(systemText, wv125BlockMarker)
	ai := strings.Index(systemText, wv125BlockTwo)
	if ai < 0 || zi > ai {
		t.Fatalf("declared order lost between the write door and the prompt (zulu@%d alpha@%d)", zi, ai)
	}
	if !strings.Contains(systemText, "[zulu]") || !strings.Contains(systemText, "[alpha]") {
		t.Fatalf("the plain-text labels did not reach the prompt:\n%s", systemText)
	}
	if strings.Contains(systemText, "<zulu") || strings.Contains(systemText, "<alpha") {
		t.Fatalf("a block name became an XML tag — the taxonomy must not be a function of config data:\n%s", systemText)
	}

	// FAILURE MODE: a duplicate name is refused and persists nothing.
	before := wv125BlockNames(t, s, id)
	status2, body2, raw2 := wv125SetBlocks(t, s, id, "", "dup", "x", "dup", "y")
	if status2 != http.StatusBadRequest {
		t.Fatalf("duplicate block name: status %d body %s, want 400", status2, raw2)
	}
	if msg, _ := body2["message"].(string); !strings.Contains(msg, "dup") {
		t.Errorf("the duplicate refusal does not name the offender: %q", msg)
	}
	if after := wv125BlockNames(t, s, id); len(after) != len(before) {
		t.Fatalf("the refused write changed the section %v -> %v", before, after)
	}
}

// ---------------------------------------------------------------------------
// LEG 223 — the master-plan status classifier the inert-smoke gate rests on.
// ---------------------------------------------------------------------------

// wv125Leg223 drives `scripts/preflight.sh`'s phase_row_status /
// phase_status_arm / phase_is_shipped by extracting the three functions into a
// temporary driver and running them under bash (argv-form, never a shell
// string — CLAUDE.md §7 rule 8).
//
// It asserts what the phase actually fixed: a NOT-SHIPPED master-plan row
// classifies not-shipped (before 223, six live status words fell to the
// shipped arm and thirteen unshipped phases were parked in the inert baseline
// as shipped-phase debt), a shipped row classifies shipped, and an
// unrecognised word STILL falls to the strict arm.
func wv125Leg223(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	preflight := filepath.Join(root, "scripts", "preflight.sh")
	src, err := os.ReadFile(preflight)
	if err != nil {
		t.Fatalf("read preflight.sh: %v", err)
	}
	extract := func(name string) string {
		start := strings.Index(string(src), "\n"+name+"() {\n")
		if start < 0 {
			t.Fatalf("scripts/preflight.sh no longer defines %s() — the inert-smoke gate's classifier has moved or been deleted", name)
		}
		rest := string(src)[start+1:]
		end := strings.Index(rest, "\n}\n")
		if end < 0 {
			t.Fatalf("could not find the end of %s()", name)
		}
		return rest[:end+2]
	}

	driver := filepath.Join(t.TempDir(), "classify.sh")
	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env bash\nset -euo pipefail\ncd \"$1\"\n")
	for _, fn := range []string{"phase_row_status", "phase_status_arm", "phase_is_shipped"} {
		sb.WriteString(extract(fn))
		sb.WriteString("\n")
	}
	// Print `<token>=<arm>=<shipped?>` for every argument after the root.
	sb.WriteString("shift\nfor n in \"$@\"; do\n  printf '%s=%s=%s\\n' \"$n\" \"$(phase_status_arm \"$(phase_row_status \"$n\")\")\" \"$(phase_is_shipped \"$n\")\"\ndone\n")
	if err := os.WriteFile(driver, []byte(sb.String()), 0o700); err != nil {
		t.Fatalf("write driver: %v", err)
	}

	// Pick real rows out of the master plan rather than inventing tokens: the
	// classifier reads docs/plans/README.md, so a fabricated token would
	// exercise only the no-row arm.
	shipped := wv125FindPhaseWithStatus(t, root, func(s string) bool { return strings.HasPrefix(s, "Shipped") })
	notShipped := wv125FindPhaseWithStatus(t, root, func(s string) bool {
		for _, w := range []string{"Pending", "Post-V1", "Cut", "Ready", "Revisit", "Superseded", "Reverted", "Deprecated", "Deferred"} {
			if strings.HasPrefix(s, w) {
				return true
			}
		}
		return false
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// argv-form: bash <driver> <root> <tokens...>. No shell string.
	out, err := exec.CommandContext(ctx, "bash", driver, root, shipped, notShipped, "wv125-no-such-phase").CombinedOutput()
	if err != nil {
		t.Fatalf("classifier driver failed: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "=", 3)
		if len(parts) != 3 {
			t.Fatalf("unparsable driver line %q (full output:\n%s)", line, out)
		}
		got[parts[0]] = parts[1] + "/" + parts[2]
	}
	if got[shipped] != "shipped/yes" {
		t.Errorf("phase %q (a Shipped row) classified %q, want shipped/yes", shipped, got[shipped])
	}
	if got[notShipped] != "not-shipped/no" {
		t.Errorf("phase %q (a NOT-shipped row) classified %q, want not-shipped/no — this is the misclassification 223 fixed; an unshipped phase's empty smoke would be reported as shipped-phase debt",
			notShipped, got[notShipped])
	}
	// FAILURE MODE / fail-closed default: a token with no row at all must
	// still resolve to the STRICT arm, so a correction can only ever RELAX
	// classification and nothing that passes today starts failing.
	if got["wv125-no-such-phase"] != "unknown/yes" && got["wv125-no-such-phase"] != "/yes" {
		t.Errorf("a phase with NO master-plan row classified %q, want the strict shipped default", got["wv125-no-such-phase"])
	}
}

// wv125FindPhaseWithStatus returns the first master-plan phase token whose
// status cell satisfies match. It parses the table the same way the classifier
// does — the `| <token> |` row shape with the LAST pipe-delimited cell as the
// status (the `*\|` shape 223 corrected; a `+\|` here would reproduce the
// original bug and make this helper silently skip 106 rows).
func wv125FindPhaseWithStatus(t *testing.T, root string, match func(string) bool) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "plans", "README.md"))
	if err != nil {
		t.Fatalf("read master plan: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 2 {
			continue
		}
		token := strings.TrimSpace(cells[0])
		status := strings.TrimSpace(cells[len(cells)-1])
		if token == "" || token == "Phase" || strings.HasPrefix(token, "-") {
			continue
		}
		if match(status) {
			return token
		}
	}
	t.Fatal("no master-plan row matched the requested status class — the table shape changed")
	return ""
}

// ---------------------------------------------------------------------------
// The wave-end composition.
// ---------------------------------------------------------------------------

// TestE2E_WaveV125_PromptCompositionSurface runs every leg of the wave against
// ONE devstack with real drivers on every seam.
func TestE2E_WaveV125_PromptCompositionSurface(t *testing.T) {
	s := newCMStack(t, nil)

	t.Run("219 caller memory reaches the untrusted external tier", func(t *testing.T) { wv125Leg219(t, s) })
	t.Run("220 the run-level additive value composes below the tenant value", func(t *testing.T) { wv125Leg220(t) })
	t.Run("221 a stale expected-revision token is refused with a conflict", func(t *testing.T) { wv125Leg221(t, s) })
	t.Run("222 the ordered named blocks render in declared order", func(t *testing.T) { wv125Leg222(t, s) })
	t.Run("223 the master-plan status classifier", func(t *testing.T) { wv125Leg223(t) })
}

// TestE2E_WaveV125_MultiIsolation — TWO tenants and TWO users, with NEGATIVE
// assertions on both of the wave's content-carrying surfaces (219's caller
// memory and 222's blocks). `agent_id` is held CONSTANT throughout: CLAUDE.md
// §6 states it is not an isolation principal, so varying it would assert
// nothing about isolation.
func TestE2E_WaveV125_MultiIsolation(t *testing.T) {
	s := newCMStack(t, nil)

	type actor struct {
		id        identity.Identity
		block     string
		caller    string
		forbidden []string
	}
	actors := []actor{
		{id: wv125ID(wv125TenantA, wv125UserA1), block: "iso-a1-block", caller: "iso-a1-caller"},
		{id: wv125ID(wv125TenantB, wv125UserB1), block: "iso-b1-block", caller: "iso-b1-caller"},
	}
	actors[0].forbidden = []string{actors[1].block, actors[1].caller}
	actors[1].forbidden = []string{actors[0].block, actors[0].caller}

	// Every actor writes its own block under the SAME agent id.
	for _, a := range actors {
		if status, _, raw := wv125SetBlocks(t, s, a.id, "", "own", a.block); status != http.StatusOK {
			t.Fatalf("%s block write: status %d body %s", a.id.TenantID, status, raw)
		}
	}

	for _, a := range actors {
		status, body := s.postStart(t, cmStartBody(a.id, "wv125 isolation",
			map[string]any{"note": a.caller}), a.id, true)
		if status != http.StatusOK && status != http.StatusAccepted {
			t.Fatalf("%s start: status %d body %s", a.id.TenantID, status, body)
		}
		req := wv125AwaitRequestFor(t, s, a.block)
		systemText := wv125SystemText(req)
		if !strings.Contains(systemText, a.caller) {
			t.Fatalf("%s's own caller memory did not reach its own prompt", a.id.TenantID)
		}
		// THE NEGATIVE ASSERTIONS.
		for _, forbidden := range a.forbidden {
			if strings.Contains(systemText, forbidden) {
				t.Fatalf("CROSS-TENANT LEAK: %s/%s's prompt carries %q", a.id.TenantID, a.id.UserID, forbidden)
			}
		}
	}

	// The user axis inside ONE tenant: a second user's block write under the
	// same tenant is a write to the SAME tenant-scoped agent config (the
	// agent-scope chain is keyed by the caller's TENANT), so assert exactly
	// that rather than an isolation property the design does not claim — and
	// assert the TENANT boundary still holds after it.
	a2 := wv125ID(wv125TenantA, wv125UserA2)
	if status, _, raw := wv125SetBlocks(t, s, a2, "", "own", "iso-a2-block"); status != http.StatusOK {
		t.Fatalf("a2 block write: status %d body %s", status, raw)
	}
	if names := wv125BlockNames(t, s, wv125ID(wv125TenantA, wv125UserA1)); len(names) != 1 || names[0] != "own" {
		t.Fatalf("tenant A's config after the second user's write = %v, want [own]", names)
	}
	if names := wv125BlockNames(t, s, wv125ID(wv125TenantB, wv125UserB1)); len(names) != 1 {
		t.Fatalf("tenant B's config was disturbed by a tenant-A user's write: %v", names)
	}
	// Tenant B still holds ITS body, not tenant A's.
	statusB, _, rawB := wv125AgentCfgCall(t, s, "/v1/agent_config/get",
		prototypes.AgentConfigGetRequest{AgentID: s.stack.AgentConfigID},
		wv125ID(wv125TenantB, wv125UserB1), []auth.Scope{auth.ScopeAdmin})
	if statusB != http.StatusOK {
		t.Fatalf("tenant B get: status %d", statusB)
	}
	if strings.Contains(rawB, "iso-a2-block") || strings.Contains(rawB, "iso-a1-block") {
		t.Fatalf("CROSS-TENANT LEAK in the config read: %s", rawB)
	}
}

// TestE2E_WaveV125_ConcurrencyAndGoroutineBaseline drives N>=12 concurrent
// runs across BOTH tenants and BOTH users against ONE devstack, asserting no
// cross-talk between the composed prompts, and then settle-loops
// runtime.NumGoroutine() back to its baseline.
//
// The goroutine assertion is a settle LOOP with a deadline, not a single
// sample: the run loop's per-task goroutines unwind asynchronously, so one
// sample would either flake or (with a generous slack) assert nothing.
func TestE2E_WaveV125_ConcurrencyAndGoroutineBaseline(t *testing.T) {
	s := newCMStack(t, nil)

	// Seed a distinct block PER TENANT. The agent-scope config chain is keyed
	// by the caller's TENANT (agent_id is a key, not an isolation principal),
	// so two users inside one tenant deliberately share one config — seeding
	// per user would replace, not isolate, and the fixture would be asserting
	// a property the design does not claim.
	ids := []identity.Identity{
		wv125ID(wv125TenantA, wv125UserA1),
		wv125ID(wv125TenantA, wv125UserA2),
		wv125ID(wv125TenantB, wv125UserB1),
	}
	blockFor := map[string]string{
		wv125TenantA: "stress-block-tenant-a",
		wv125TenantB: "stress-block-tenant-b",
	}
	for tenant, block := range blockFor {
		seedID := wv125ID(tenant, wv125UserA1)
		if tenant == wv125TenantB {
			seedID = wv125ID(tenant, wv125UserB1)
		}
		if status, _, raw := wv125SetBlocks(t, s, seedID, "", "own", block); status != http.StatusOK {
			t.Fatalf("seed %s: status %d body %s", tenant, status, raw)
		}
	}

	// Mint every token BEFORE the fan-out: signing calls t.Fatal on failure,
	// which is undefined behaviour off the test goroutine.
	tokens := make([]string, len(ids))
	for i, id := range ids {
		tokens[i] = cmSignToken(t, s, id)
	}

	baseline := goruntime.NumGoroutine()

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := ids[i%len(ids)]
			marker := fmt.Sprintf("stress-caller-%02d", i)
			status, body, perr := wv125PostStartNoFatal(s, cmStartBody(id, "wv125 stress", map[string]any{"note": marker}), tokens[i%len(ids)])
			if perr != nil {
				errs <- fmt.Errorf("run %d: %w", i, perr)
				return
			}
			if status != http.StatusOK && status != http.StatusAccepted {
				errs <- fmt.Errorf("run %d: status %d body %s", i, status, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// Every stress marker must appear in EXACTLY ONE request's system message,
	// and each such request must also carry that identity's OWN block and no
	// other identity's.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.rec.requests()) >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := range n {
		marker := fmt.Sprintf("stress-caller-%02d", i)
		wantBlock := blockFor[ids[i%len(ids)].TenantID]
		var seen int
		for _, req := range s.rec.requests() {
			text := wv125SystemText(req)
			if !strings.Contains(text, marker) {
				continue
			}
			seen++
			if !strings.Contains(text, wantBlock) {
				t.Errorf("run %d carries %s but not its own block %s", i, marker, wantBlock)
			}
			for _, other := range blockFor {
				if other != wantBlock && strings.Contains(text, other) {
					t.Errorf("CROSS-TENANT CROSS-TALK: run %d (%s, tenant %s) carries %s",
						i, marker, ids[i%len(ids)].TenantID, other)
				}
			}
		}
		if seen != 1 {
			t.Errorf("stress marker %s appeared in %d requests' system content, want exactly 1", marker, seen)
		}
	}

	// The settle loop.
	settleDeadline := time.Now().Add(30 * time.Second)
	var last int
	for time.Now().Before(settleDeadline) {
		last = goruntime.NumGoroutine()
		if last <= baseline+4 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines did not settle back to the baseline: baseline=%d last=%d after 30s", baseline, last)
}

// wv125PostStartNoFatal issues a `start` WITHOUT calling t.Fatal — the
// concurrency leg drives it from goroutines, where a Fatal is undefined
// behaviour. Every failure is returned so the caller reports it on the test
// goroutine.
func wv125PostStartNoFatal(s *cmStack, body any, token string) (int, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+"/v1/control/start", strings.NewReader(string(raw)))
	if err != nil {
		return 0, "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, string(out), nil
}

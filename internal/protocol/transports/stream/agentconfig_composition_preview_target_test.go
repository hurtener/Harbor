package stream_test

// agentconfig_composition_preview_target_test.go — the HA-66 read-only
// composition-preview target resolution on the shared agent-config wire
// handler. The wire contract says omitting ALL of tenant_id / user_id /
// session_id (the CLI and Console send only agent_id) resolves the
// preview to the VERIFIED caller's own triple; these tests pin that
// resolution on a REAL handler + REAL preview service, and pin the
// boundaries around it: a PARTIAL tuple fails loud and is never
// completed, an explicit complete target is forwarded byte-for-byte and
// stays subject to the service's exact-triple / reach / widening gates,
// and a forged body identity cannot steer the default.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/bootpacks"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// previewBootEntry freezes ONE boot-baseline entry the way the eager
// loader would: a parsed pack skill with Origin=pack / Scope=project and
// the canonical semantic hash of its own body (the strict composer
// re-validates the pairing).
func previewBootEntry(name string) bootpacks.Entry {
	skill := skills.Skill{
		Name: name, Title: "Title " + name, Description: "desc " + name,
		Trigger: "trigger " + name, Steps: []string{"step"},
		Origin: skills.OriginPack, Scope: skills.ScopeProject,
	}
	return bootpacks.Entry{Skill: skill, SemanticHash: skills.CanonicalContentHash(skill)}
}

// previewBootIndex is the frozen eager boot-pack index reader seam the
// preview service composes from — a minimal in-test double of
// *bootpacks.Index. Immutable after construction; the strict composer
// treats every entry as a value input (it never mutates the caller's
// slice), so sharing the stored slice across reads is safe under -race.
type previewBootIndex struct {
	byKey map[bootpacks.Key][]bootpacks.Entry
}

func (f *previewBootIndex) Lookup(tenantID, agentID string) ([]bootpacks.Entry, bool) {
	entries, ok := f.byKey[bootpacks.Key{TenantID: tenantID, AgentID: agentID}]
	if !ok {
		return nil, false
	}
	return entries, true
}

// newPreviewHandlerFixture builds the shared agent-config handler with
// the REAL read-only composition-preview service wired (statestore
// registry as the reader seam + a frozen boot index + both signed-reach
// gates — the production posture) over the same real registry the
// session-handler fixture uses. The boot index declares the (t1,
// agent-x) key so a correctly-targeted preview resolves to the
// `available` outcome with the single boot item alpha.
func newPreviewHandlerFixture(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: *acID()}, acAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("activate agent lifecycle: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(ctx)
		_ = bus.Close(ctx)
		_ = st.Close(ctx)
	})
	svc, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	reader, ok := reg.(agentcfgprotocol.AgentConfigReader)
	if !ok {
		t.Fatalf("statestore registry does not satisfy the preview reader seam")
	}
	preview, err := agentcfgprotocol.NewCompositionPreviewService(reader,
		&previewBootIndex{byKey: map[bootpacks.Key][]bootpacks.Entry{
			{TenantID: "t1", AgentID: acAgent}: {previewBootEntry("alpha")},
		}},
		agentcfgprotocol.WithPreviewSessionReach(auth.NewSessionReachAuthorizer()),
		agentcfgprotocol.WithPreviewAgentReach(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("preview service: %v", err)
	}
	h, err := stream.NewAgentConfigHandler(svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()),
		stream.WithCompositionPreviewService(preview),
	)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return h
}

// TestAgentConfigCompositionPreview_OmittedTargetResolvesToCaller is the
// core regression: the CLI and Console send ONLY agent_id — no body
// identity, no target triple — and the preview must resolve to the
// VERIFIED transport caller (t1/u1/s1 from the X-Harbor-* headers) and
// serve the caller's own boot composition. Before the fix the all-empty
// target was forwarded unchanged and the service rejected it with 401.
func TestAgentConfigCompositionPreview_OmittedTargetResolvesToCaller(t *testing.T) {
	h := newPreviewHandlerFixture(t)
	code, body := acReq(t, h, "composition/preview", `{"agent_id":"agent-x"}`, acID(), []auth.Scope{})
	if code != http.StatusOK {
		t.Fatalf("omitted-target preview status = %d body=%s, want 200", code, body)
	}
	var resp prototypes.AgentConfigCompositionPreviewResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode preview response: %v body=%s", err, body)
	}
	if resp.Outcome != "available" {
		t.Fatalf("omitted-target outcome = %q, want available (the caller's own boot composition)", resp.Outcome)
	}
	if resp.Widened {
		t.Fatalf("omitted-target preview must not be widened")
	}
	if len(resp.Items) != 1 || resp.Items[0].Name != "alpha" {
		t.Fatalf("omitted-target items = %+v, want the single boot item alpha", resp.Items)
	}
}

// TestAgentConfigCompositionPreview_PartialTargetFailsLoudNeverFilled
// proves a PARTIALLY specified target stays invalid: the handler never
// fills the missing fields of a partial tuple, so the preview service's
// identity.Validate rejects it loud (401 identity_required) exactly as
// before the fix.
func TestAgentConfigCompositionPreview_PartialTargetFailsLoudNeverFilled(t *testing.T) {
	h := newPreviewHandlerFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"tenant only", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"tenant_id":"t1","agent_id":"agent-x"}`},
		{"user only", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"user_id":"u1","agent_id":"agent-x"}`},
		{"session only", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"session_id":"s1","agent_id":"agent-x"}`},
		{"tenant+user", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"tenant_id":"t1","user_id":"u1","agent_id":"agent-x"}`},
		{"user+session", `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"user_id":"u1","session_id":"s1","agent_id":"agent-x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := acReq(t, h, "composition/preview", tc.body, acID(), []auth.Scope{})
			if code != http.StatusUnauthorized || errCode(t, resp) != protoerrors.CodeIdentityRequired {
				t.Fatalf("partial target (%s) = (%d,%s), want 401 identity_required", tc.name, code, resp)
			}
		})
	}
}

// TestAgentConfigCompositionPreview_ExplicitCompleteTargetForwardedAndGoverned
// proves an explicit complete target is forwarded byte-for-byte — never
// resolved to the caller — and remains subject to the preview service's
// exact-triple boundary, the audited admin/fleet widening, and the
// non-oracular cross-tenant refusal.
func TestAgentConfigCompositionPreview_ExplicitCompleteTargetForwardedAndGoverned(t *testing.T) {
	h := newPreviewHandlerFixture(t)
	cases := []struct {
		name     string
		body     string
		scopes   []auth.Scope
		wantSt   int
		wantOut  string
		wantWide bool
	}{
		{
			name:    "ordinary caller explicit own triple",
			body:    `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"tenant_id":"t1","user_id":"u1","session_id":"s1","agent_id":"agent-x"}`,
			scopes:  []auth.Scope{},
			wantSt:  http.StatusOK,
			wantOut: "available",
		},
		{
			name:    "ordinary caller explicit foreign same-tenant triple",
			body:    `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"tenant_id":"t1","user_id":"u2","session_id":"s2","agent_id":"agent-x"}`,
			scopes:  []auth.Scope{},
			wantSt:  http.StatusOK,
			wantOut: "unavailable",
		},
		{
			name:     "admin explicit same-tenant widened triple",
			body:     `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"tenant_id":"t1","user_id":"u2","session_id":"s2","agent_id":"agent-x"}`,
			scopes:   []auth.Scope{auth.ScopeAdmin},
			wantSt:   http.StatusOK,
			wantOut:  "available",
			wantWide: true,
		},
		{
			name:    "admin explicit cross-tenant triple refused non-oracular",
			body:    `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"tenant_id":"t-other","user_id":"u2","session_id":"s2","agent_id":"agent-x"}`,
			scopes:  []auth.Scope{auth.ScopeAdmin},
			wantSt:  http.StatusOK,
			wantOut: "unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := acReq(t, h, "composition/preview", tc.body, acID(), tc.scopes)
			if code != tc.wantSt {
				t.Fatalf("status = %d body=%s, want %d", code, resp, tc.wantSt)
			}
			var preview prototypes.AgentConfigCompositionPreviewResponse
			if err := json.Unmarshal(resp, &preview); err != nil {
				t.Fatalf("decode preview response: %v body=%s", err, resp)
			}
			if preview.Outcome != tc.wantOut {
				t.Fatalf("outcome = %q, want %q (body=%s)", preview.Outcome, tc.wantOut, resp)
			}
			if preview.Widened != tc.wantWide {
				t.Fatalf("widened = %v, want %v (body=%s)", preview.Widened, tc.wantWide, resp)
			}
		})
	}
}

// TestAgentConfigCompositionPreview_ForgedBodyIdentityCannotSteerDefault
// proves the default target derives from the VERIFIED transport identity,
// never the body: a forged body triple (a foreign tenant, or a foreign
// user/session on the caller's tenant) is refused by the shared
// body-identity gate before the target resolution runs — even with the
// target fields omitted, a body that renames the caller cannot steer the
// default.
func TestAgentConfigCompositionPreview_ForgedBodyIdentityCannotSteerDefault(t *testing.T) {
	h := newPreviewHandlerFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"foreign tenant", `{"identity":{"tenant":"foreign","user":"u1","session":"s1"},"agent_id":"agent-x"}`},
		{"foreign user+session", `{"identity":{"tenant":"t1","user":"u2","session":"s2"},"agent_id":"agent-x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, resp := acReq(t, h, "composition/preview", tc.body, acID(), []auth.Scope{})
			if code != http.StatusUnauthorized || errCode(t, resp) != protoerrors.CodeIdentityRequired {
				t.Fatalf("forged body (%s) = (%d,%s), want 401 identity_required", tc.name, code, resp)
			}
		})
	}
}

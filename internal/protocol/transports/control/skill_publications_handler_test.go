package control_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

type recordingSkillPublicationsSurface struct {
	method  methods.Method
	request any
	resp    any
}

func (s *recordingSkillPublicationsSurface) Dispatch(_ context.Context, method methods.Method, req any) (any, error) {
	s.method = method
	s.request = req
	if s.resp != nil {
		return s.resp, nil
	}
	return &types.SkillPublicationAvailableResponse{ProtocolVersion: types.ProtocolVersion}, nil
}

func TestSkillPublicationsHandler_StrictDecodeAndDispatch(t *testing.T) {
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	surface := &recordingSkillPublicationsSurface{}
	h, err := control.NewHandler(cs, control.WithSkillPublicationsSurface(surface))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	body := `{"identity":{"tenant":"t1","user":"u1","session":"s1"}}`
	rec := do(t, h, "/v1/control/skills.publications.available", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if surface.method != methods.MethodSkillsPublicationsAvailable {
		t.Fatalf("method = %q, want %q", surface.method, methods.MethodSkillsPublicationsAvailable)
	}
	if _, ok := surface.request.(*types.SkillPublicationAvailableRequest); !ok {
		t.Fatalf("request type = %T, want *SkillPublicationAvailableRequest", surface.request)
	}

	bad := `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"authority":"admin"}`
	rec = do(t, h, "/v1/control/skills.publications.available", bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSkillPublicationsHandler_UnconfiguredSurfaceIsUnavailable(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	rec := do(t, h, "/v1/control/skills.publications.list", `{"identity":{"tenant":"t1","user":"u1","session":"s1"}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSkillPublicationsHandler_DecodesMutationEnvelopeWithoutGrantingAuthority(t *testing.T) {
	cs, cleanup := newTestSurface(t)
	t.Cleanup(cleanup)
	surface := &recordingSkillPublicationsSurface{resp: &types.SkillPublicationPublishResponse{ProtocolVersion: types.ProtocolVersion}}
	h, err := control.NewHandler(cs, control.WithSkillPublicationsSurface(surface))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	request := types.SkillPublicationPublishRequest{
		Identity:       types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
		Name:           "ops",
		Skill:          types.SkillPublicationSkill{Name: "ops", Trigger: "when ops", Steps: []string{"run"}},
		IdempotencyKey: "publish-1",
		ExpectedAbsent: true,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "/v1/control/skills.publications.publish", string(encoded))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	decoded, ok := surface.request.(*types.SkillPublicationPublishRequest)
	if !ok {
		t.Fatalf("request type = %T, want *SkillPublicationPublishRequest", surface.request)
	}
	if decoded.Identity != request.Identity || decoded.Name != request.Name {
		t.Fatalf("decoded request = %#v, want body fields preserved as data", decoded)
	}

	// The transport has not turned request data into authority: the concrete
	// SkillPublicationsSurface performs the verified identity/scope check.
	if strings.Contains(rec.Body.String(), "authority") {
		t.Fatalf("response echoed untrusted authority field: %s", rec.Body.String())
	}
}

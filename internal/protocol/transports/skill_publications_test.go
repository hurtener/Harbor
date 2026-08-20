package transports_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

type muxSkillPublicationsSurface struct{}

func (muxSkillPublicationsSurface) Dispatch(_ context.Context, _ methods.Method, _ any) (any, error) {
	return &types.SkillPublicationAvailableResponse{ProtocolVersion: types.ProtocolVersion}, nil
}

func TestNewMux_WithSkillPublicationsSurface_MountsControlRoute(t *testing.T) {
	deps := newTestDeps(t)
	defer deps.cleanup()
	mux, err := transports.NewMux(deps.surface, deps.bus,
		transports.WithoutValidator(),
		transports.WithSkillPublicationsSurface(muxSkillPublicationsSurface{}),
	)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/control/skills.publications.available", nil)
	req.Header.Set("X-Harbor-Tenant", "tenant")
	req.Header.Set("X-Harbor-User", "user")
	req.Header.Set("X-Harbor-Session", "session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publication route status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

package sessions_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
)

type configurationEraseFailure struct {
	state.StateStore
	failSave, failDelete bool
}

func (s *configurationEraseFailure) Save(ctx context.Context, rec state.StateRecord) error {
	if s.failSave {
		return errors.New("configuration fence unavailable")
	}
	return s.StateStore.Save(ctx, rec)
}

func (s *configurationEraseFailure) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	if s.failDelete {
		return 0, errors.New("configuration delete unavailable")
	}
	return s.StateStore.DeleteScope(ctx, id)
}

func TestCascadeEraser_SeparateConfigurationStore(t *testing.T) {
	for _, failure := range []string{"none", "fence", "delete"} {
		t.Run(failure, func(t *testing.T) {
			f := newErasureFixture(t, nil)
			st, err := state.Open(t.Context(), config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			cfg := &configurationEraseFailure{StateStore: st}
			reg, err := agentcfg.Open(t.Context(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: f.bus})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reg.Close(context.Background()) })
			id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "erased"}
			q := identity.Quadruple{Identity: id}
			ctx, err := identity.With(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.reg.Open(ctx, id.SessionID, id); err != nil {
				t.Fatal(err)
			}
			if _, err := reg.SetRevision(ctx, q, "agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
				t.Fatal(err)
			}
			overlays, err := sessionoverlay.NewStore(st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = overlays.Close(context.Background()) })
			if _, err := overlays.SetUserPrompt(ctx, q, "agent", "erase this setting"); err != nil {
				t.Fatal(err)
			}
			other := q
			other.SessionID = "survives"
			if _, err := overlays.SetUserPrompt(ctx, other, "agent", "keep this setting"); err != nil {
				t.Fatal(err)
			}
			eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{Registry: f.reg, State: f.store, ConfigurationState: cfg, Artifacts: f.arts, Bus: f.bus})
			if err != nil {
				t.Fatal(err)
			}
			cfg.failSave, cfg.failDelete = failure == "fence", failure == "delete"
			if failure != "none" {
				if _, err := eraser.Erase(ctx, id); err == nil || !strings.Contains(err.Error(), "erase configuration") {
					t.Fatalf("configuration failure not surfaced: %v", err)
				}
				cfg.failSave, cfg.failDelete = false, false
			}
			if _, err := eraser.Erase(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, _, err := overlays.Get(ctx, q, "agent"); !errors.Is(err, sessionoverlay.ErrSessionErased) {
				t.Fatalf("erased setting readable: %v", err)
			}
			if _, err := overlays.SetUserPrompt(ctx, q, "agent", "late update"); !errors.Is(err, sessionoverlay.ErrSessionErased) {
				t.Fatalf("late update accepted: %v", err)
			}
			if remaining, nerr := st.DeleteScope(ctx, id); nerr != nil || remaining != 0 {
				t.Fatalf("configuration records survived erasure: %d, %v", remaining, nerr)
			}
			if got, found, err := overlays.Get(ctx, other, "agent"); err != nil || !found || got.UserPrompt != "keep this setting" {
				t.Fatalf("sibling changed: %v %v", found, err)
			}
			if _, found, err := reg.Active(ctx, q, "agent", agentcfg.ConfigScopeAgent); err != nil || !found {
				t.Fatalf("agent configuration erased: %v %v", found, err)
			}
		})
	}
}

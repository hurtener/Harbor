package assemble_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
)

func TestConfigurationState_DefaultSharesExecutionStore(t *testing.T) {
	stack, err := assemble.Assemble(t.Context(), minimalCfg(t), assemble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stack.Close(context.Background()) }()
	if stack.ConfigurationState != stack.State {
		t.Fatal("omitted configuration_state changed legacy storage")
	}
}

func TestConfigurationState_ExplicitSameStoreAliases(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "shared.sqlite")}
	cfg.ConfigurationState = cfg.State
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stack.Close(context.Background()) }()
	if stack.ConfigurationState != stack.State {
		t.Fatal("same configured store was opened twice")
	}
}

func TestConfigurationState_OpenFailureDoesNotFallBack(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.ConfigurationState = config.StateConfig{Driver: "sqlite", DSN: t.TempDir()}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
	if stack != nil {
		defer func() { _ = stack.Close(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "configuration_state") {
		t.Fatalf("configuration failure was hidden: %v", err)
	}
}

func TestConfigurationState_RestartPreservesSettingsNotConversation(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "configuration.sqlite")
			if driver == "postgres" {
				dsn = os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN not set")
				}
			}
			cfg := minimalCfg(t)
			cfg.ConfigurationState = config.StateConfig{Driver: driver, DSN: dsn}
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "configuration-restart-" + string(state.NewEventID()), UserID: "operator", SessionID: "conversation"}}
			open := func() (*assemble.Stack, agentcfg.Registry) {
				t.Helper()
				stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = stack.Close(context.Background()) })
				reg, err := agentcfg.Open(t.Context(), agentcfg.Config{}, agentcfg.Deps{State: stack.ConfigurationState, Bus: stack.Bus})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reg.Close(context.Background()) })
				return stack, reg
			}
			first, reg := open()
			prompt := "operator settings survive restart"
			payload := agentcfg.ConfigPayload{PromptLayers: &agentcfg.PromptLayers{Base: &prompt}}
			revision, err := reg.SetRevision(t.Context(), q, "test-agent", agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			record := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: "conversation-fixture", Bytes: []byte("private chat summary and tool evidence")}
			if err := first.State.Save(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			if _, err := first.ConfigurationState.Load(t.Context(), q, record.Kind); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("conversation reached configuration store: %v", err)
			}
			if err := reg.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if driver == "postgres" {
				cfg.ConfigurationState.MigrationMode = "verify"
			}
			second, reg := open()
			got, found, err := reg.Active(t.Context(), q, "test-agent", agentcfg.ConfigScopeAgent)
			if err != nil || !found || got.ContentHash != revision.ContentHash {
				t.Fatalf("configuration lost on restart: found=%t err=%v", found, err)
			}
			for name, store := range map[string]state.StateStore{"execution": second.State, "configuration": second.ConfigurationState} {
				if _, err := store.Load(t.Context(), q, record.Kind); !errors.Is(err, state.ErrNotFound) {
					t.Fatalf("%s retained conversation after restart: %v", name, err)
				}
			}
			other := q
			other.TenantID += "-other"
			if _, found, err := reg.Active(t.Context(), other, "test-agent", agentcfg.ConfigScopeAgent); err != nil || found {
				t.Fatalf("tenant isolation: found=%t err=%v", found, err)
			}
		})
	}
}

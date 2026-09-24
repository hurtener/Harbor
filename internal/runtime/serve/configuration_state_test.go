package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestBuildMux_SeparateConfigurationStateRequiresStore(t *testing.T) {
	_, err := BuildMux(MuxInput{Cfg: &config.Config{ConfigurationState: config.StateConfig{Driver: "postgres", DSN: "test"}}})
	if err == nil || !strings.Contains(err.Error(), "configuration_state") {
		t.Fatalf("missing configuration store did not fail closed: %v", err)
	}
}

func TestBoot_SeparateConfigurationState_ProtocolRestart(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "agent-config.sqlite")
			if driver == "postgres" {
				dsn = os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN not set")
				}
			}
			// DSNs stay in the secret environment rather than the test YAML.
			t.Setenv("HARBOR_CONFIGURATION_STATE_DRIVER", driver)
			t.Setenv("HARBOR_CONFIGURATION_STATE_DSN", dsn)
			signer := newTestSigner(t)
			opts := baseOptions(t)
			opts.AuthValidatorFactory = signer.factory()
			id := identity.Identity{TenantID: "restart-" + filepath.Base(t.TempDir()), UserID: "operator", SessionID: "session"}
			opts.MCPDefaultIdentity = id
			const agent = "harbor-dev-agent"
			request := func(h *Handle, tenant, route, body string) *httptest.ResponseRecorder {
				t.Helper()
				token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
					"sub": id.UserID, "exp": time.Now().Add(time.Hour).Unix(),
					"tenant": tenant, "user": id.UserID, "session": id.SessionID,
					"scopes": []string{"admin"}, auth.AgentReachClaim: []string{agent},
				})
				token.Header["kid"] = signer.kid
				signed, err := token.SignedString(signer.priv)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/agent_config/"+route, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+signed)
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				h.Handler().ServeHTTP(rr, req)
				return rr
			}
			first := bootTest(t, t.Context(), opts)
			set := request(first, id.TenantID, "set_revision", `{"agent_id":"harbor-dev-agent","payload":{"prompt_layers":{"base":"operator setting preserved exactly"}}}`)
			if set.Code != http.StatusOK {
				t.Fatalf("set = %d: %s", set.Code, set.Body.String())
			}
			var written prototypes.AgentConfigSetRevisionResponse
			if err := json.Unmarshal(set.Body.Bytes(), &written); err != nil {
				t.Fatal(err)
			}
			first.Close(context.Background())
			second := bootTest(t, t.Context(), opts)
			get := request(second, id.TenantID, "get", `{"agent_id":"harbor-dev-agent"}`)
			if get.Code != http.StatusOK {
				t.Fatalf("get after restart = %d: %s", get.Code, get.Body.String())
			}
			var got prototypes.AgentConfigGetResponse
			if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !got.Set || got.Revision == nil || got.Revision.ContentHash != written.Revision.ContentHash {
				t.Fatal("served restart lost or replaced operator configuration")
			}
			other := request(second, id.TenantID+"-other", "get", `{"agent_id":"harbor-dev-agent"}`)
			if strings.Contains(other.Body.String(), "operator setting preserved exactly") {
				t.Fatal("cross-tenant configuration disclosure")
			}
		})
	}
}

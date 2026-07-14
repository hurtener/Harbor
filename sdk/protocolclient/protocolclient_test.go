package protocolclient_test

import (
	"errors"
	"net/http"
	"testing"

	client "github.com/hurtener/Harbor/sdk/protocolclient"
)

func TestFacade_ForwardsConstructionSurface(t *testing.T) {
	httpClient := &http.Client{}
	identity := client.IdentityScope{Tenant: "tenant", User: "user", Session: "session"}
	connection := client.Connection{
		BaseURL:  "http://127.0.0.1:18080",
		Token:    client.StaticToken("test-token", identity),
		Identity: identity,
	}
	protocol, err := client.New(connection, client.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if protocol.Identity().Session != "session" {
		t.Fatalf("identity = %+v", protocol.Identity())
	}
	clone := protocol.WithSession("other")
	if clone.Identity().Session != "other" || protocol.Identity().Session != "session" {
		t.Fatalf("clone=%+v original=%+v", clone.Identity(), protocol.Identity())
	}
	if _, err := client.New(client.Connection{}); !errors.Is(err, client.ErrInvalidConnection) {
		t.Fatalf("invalid connection error = %v", err)
	}
}

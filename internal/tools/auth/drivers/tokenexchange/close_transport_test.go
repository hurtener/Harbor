package tokenexchange_test

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
	"github.com/hurtener/Harbor/internal/tools/auth/drivers/tokenexchange"
)

type connectionStates struct {
	idle   chan net.Conn
	closed chan net.Conn
}

func newConnectionStates() *connectionStates {
	return &connectionStates{
		idle:   make(chan net.Conn, 16),
		closed: make(chan net.Conn, 16),
	}
}

func (s *connectionStates) observe(conn net.Conn, state http.ConnState) {
	var dst chan net.Conn
	switch state {
	case http.StateIdle:
		dst = s.idle
	case http.StateClosed:
		dst = s.closed
	default:
		return
	}
	select {
	case dst <- conn:
	default:
	}
}

func waitForConnection(t *testing.T, events <-chan net.Conn, want net.Conn) net.Conn {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case conn := <-events:
			if want == nil || conn == want {
				return conn
			}
		case <-timer.C:
			t.Fatal("timed out waiting for HTTP connection state")
		}
	}
}

func providerConfig(name, tokenURL string) auth.ProviderConfig {
	return auth.ProviderConfig{
		Name:             name,
		CredentialSource: credsource.Static(tDummyBrokerClient, tDummyBrokerSecret),
		Scopes:           []string{"Mail.Read"},
		TokenURL:         tokenURL,
		Extra:            map[string]string{"audience": "https://graph.microsoft.com"},
	}
}

func TestProvider_CloseOwnedTransport_ClosesIdleConnectionAndIsIdempotent(t *testing.T) {
	states := newConnectionStates()
	broker := newFakeBrokerWithConnState(t, states.observe)
	provider, _, _ := mkProvider(t, broker)

	if _, err := provider.Token(mkCtx(t, aliceID()), tools.ToolSourceID("any")); err != nil {
		t.Fatalf("Token: %v", err)
	}
	idle := waitForConnection(t, states.idle, nil)
	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitForConnection(t, states.closed, idle)
	if err := provider.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}

func TestProvider_CloseOwnedTransport_CancelsAndJoinsActiveExchange(t *testing.T) {
	broker := newFakeBroker(t)
	broker.started = make(chan struct{}, 1)
	broker.gate = make(chan struct{})
	broker.setPosture("slow")
	defer func() {
		select {
		case <-broker.gate:
		default:
			close(broker.gate)
		}
	}()
	provider, _, _ := mkProvider(t, broker)

	tokenDone := make(chan error, 1)
	go func() {
		_, err := provider.Token(mkCtx(t, aliceID()), tools.ToolSourceID("any"))
		tokenDone <- err
	}()
	select {
	case <-broker.started:
	case <-time.After(3 * time.Second):
		t.Fatal("broker exchange did not become active")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := provider.Close(closeCtx); err != nil {
		t.Fatalf("Close active provider: %v", err)
	}
	select {
	case err := <-tokenDone:
		if err == nil {
			t.Fatal("active Token unexpectedly succeeded after provider Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active Token was not joined by provider Close")
	}
	close(broker.gate)
}

type closeCountingTransport struct {
	base   *http.Transport
	closes atomic.Int32
}

func (t *closeCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req)
}

func (t *closeCountingTransport) CloseIdleConnections() {
	t.closes.Add(1)
	t.base.CloseIdleConnections()
}

func TestProvider_CloseSuppliedTransport_DoesNotCrossProviders(t *testing.T) {
	broker := newFakeBroker(t)
	transport := &closeCountingTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(transport.base.CloseIdleConnections)
	sharedClient := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	deps, _, _ := mkDeps(t)
	deps.HTTPClient = sharedClient

	first, err := tokenexchange.New(providerConfig("shared-client-first", broker.tokenURL()), deps)
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	second, err := tokenexchange.New(providerConfig("shared-client-second", broker.tokenURL()), deps)
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })

	firstCtx := mkCtx(t, aliceID())
	secondCtx := mkCtx(t, identity.Identity{TenantID: "tenant-B", UserID: "user-bob", SessionID: "session-002"})
	if _, err := first.Token(firstCtx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if _, err := second.Token(secondCtx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if got := transport.closes.Load(); got != 0 {
		t.Fatalf("first provider closed caller-owned shared transport %d time(s)", got)
	}
	if err := second.Revoke(secondCtx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("second Revoke after first Close: %v", err)
	}
	if _, err := second.Token(secondCtx, tools.ToolSourceID("any")); err != nil {
		t.Fatalf("second Token after first Close: %v", err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatalf("Close second: %v", err)
	}
	if got := transport.closes.Load(); got != 0 {
		t.Fatalf("providers closed caller-owned shared transport %d time(s)", got)
	}
}

// Package integration_test — cross-subsystem E2E for the per-tool OAuth binding
// on the resource path + the owner-scoped credential-sink uninstall (§17). It
// wires the REAL MCP driver (streamable HTTP against the official go-sdk MCP
// server), the REAL owner-tagged auth.ProviderSet, and the store-boundary
// owner-scoped uninstall together, proving:
//
//   - a per-tool-bound resource read resolves that provider's bearer on the
//     outbound request (identity propagated), while an unbound resource falls
//     back to the connection-level binding;
//   - a CROSS-OWNER uninstall is refused at the store boundary
//     (ErrProviderOwnerCollision) and the provider keeps authenticating;
//   - an OWNER-SCOPED uninstall closes the provider so the still-bound
//     connection's next read fails LOUD (the failure mode).
//
// §17.8: the fixture is the official go-sdk MCP server over the real wire, not a
// hand-authored JSON-RPC shape.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcpdriver "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// closableBearerProvider is a real auth.OAuthProvider: a per-identity bearer
// tagged with a provider name until Close, after which Token fails LOUD — the
// production behaviour of a provider whose owner-scoped uninstall closed it.
type closableBearerProvider struct {
	tag    string
	hosts  []string
	closed atomic.Bool
}

var errBoundProviderClosed = errors.New("integration: bound provider closed")

func (c *closableBearerProvider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	if c.closed.Load() {
		return auth.Token{}, errBoundProviderClosed
	}
	id, ok := identity.From(ctx)
	if !ok {
		return auth.Token{}, auth.ErrIdentityRequired
	}
	return auth.Token{AccessToken: c.tag + "-" + id.TenantID + "-" + id.UserID, BindingScope: auth.ScopeUser}, nil
}
func (c *closableBearerProvider) InitiateFlow(context.Context, tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, nil
}
func (c *closableBearerProvider) CompleteFlow(context.Context, string, string) (auth.Token, error) {
	return auth.Token{}, nil
}
func (c *closableBearerProvider) PendingFlow(context.Context, string) (auth.PendingFlowInfo, bool, error) {
	return auth.PendingFlowInfo{}, false, nil
}
func (c *closableBearerProvider) DenyFlow(context.Context, string, string) error   { return nil }
func (c *closableBearerProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (c *closableBearerProvider) Close(context.Context) error {
	c.closed.Store(true)
	return nil
}
func (c *closableBearerProvider) AllowedDownstreamHosts() []string { return c.hosts }

// resourceBearerFront records the Authorization header seen per resource URI on
// resources/read.
type resourceBearerFront struct {
	inner http.Handler
	mu    sync.Mutex
	seen  map[string]string // uri -> Authorization
}

func (f *resourceBearerFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					URI string `json:"uri"`
				} `json:"params"`
			}
			if json.Unmarshal(body, &msg) == nil && msg.Method == "resources/read" && msg.Params.URI != "" {
				f.mu.Lock()
				f.seen[msg.Params.URI] = r.Header.Get("Authorization")
				f.mu.Unlock()
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			r.ContentLength = int64(len(body))
		}
	}
	f.inner.ServeHTTP(w, r)
}

func (f *resourceBearerFront) bearer(uri string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[uri]
}

func mkIntegBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, patternsAudit.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func TestE2E_MCPResourceBinding_OwnerScopedUninstall(t *testing.T) {
	t.Parallel()
	// The official go-sdk MCP server (spec-derived fixture) with two resources.
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-integ-respr", Version: "v0"}, nil)
	for _, uri := range []string{"mem://bound", "mem://unbound"} {
		u := uri
		srv.AddResource(
			&mcpsdk.Resource{URI: u, Name: u, MIMEType: "text/plain"},
			func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{
					{URI: req.Params.URI, MIMEType: "text/plain", Text: "content"},
				}}, nil
			},
		)
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	front := &resourceBearerFront{inner: handler, seen: map[string]string{}}
	hs := httptest.NewServer(front)
	t.Cleanup(hs.Close)
	host := strings.TrimPrefix(hs.URL, "http://")

	// The REAL owner-tagged provider set holds the bound resource provider.
	ownerA := auth.Owner{Tenant: "tenant-a", Agent: "agent-a"}
	ownerB := auth.Owner{Tenant: "tenant-b", Agent: "agent-b"}
	resProv := &closableBearerProvider{tag: "RES", hosts: []string{host}}
	connProv := &closableBearerProvider{tag: "CONN", hosts: []string{host}}
	set := auth.NewProviderSet(nil)
	if err := set.Install(ownerA, "resprov", resProv); err != nil {
		t.Fatalf("install: %v", err)
	}

	// The REAL MCP driver bound to the SAME provider instance the set holds
	// (the per-tool binding on the resource URI), with a connection-level
	// fallback for the unbound resource.
	p, err := mcpdriver.New(mcpdriver.Config{
		Name:            "integ",
		TransportMode:   mcpdriver.TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             mkIntegBus(t),
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:   connProv,
		ToolOAuthProviders: map[string]auth.OAuthProvider{
			"mem://bound": resProv,
		},
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Bound resource read resolves the per-tool provider bearer; unbound falls
	// back to the connection binding — identity propagated on both.
	if _, _, err := p.ReadResource(ctx, "mem://bound"); err != nil {
		t.Fatalf("ReadResource(bound): %v", err)
	}
	if got := front.bearer("mem://bound"); got != "Bearer RES-t1-u1" {
		t.Fatalf("bound resource bearer = %q, want Bearer RES-t1-u1", got)
	}
	if _, _, err := p.ReadResource(ctx, "mem://unbound"); err != nil {
		t.Fatalf("ReadResource(unbound): %v", err)
	}
	if got := front.bearer("mem://unbound"); got != "Bearer CONN-t1-u1" {
		t.Fatalf("unbound resource bearer = %q, want Bearer CONN-t1-u1 (connection fallback)", got)
	}

	// Cross-owner uninstall is REFUSED at the store boundary; the provider keeps
	// authenticating.
	if err := set.Uninstall(context.Background(), ownerB, "resprov"); !errors.Is(err, auth.ErrProviderOwnerCollision) {
		t.Fatalf("cross-owner uninstall = %v, want ErrProviderOwnerCollision", err)
	}
	if _, _, err := p.ReadResource(ctx, "mem://bound"); err != nil {
		t.Fatalf("ReadResource(bound) after refused cross-owner uninstall: %v", err)
	}

	// Owner-scoped uninstall closes the provider; the still-bound resource read
	// now fails LOUD (never a silent unauthenticated dial).
	if err := set.Uninstall(context.Background(), ownerA, "resprov"); err != nil {
		t.Fatalf("owner-scoped uninstall: %v", err)
	}
	if _, _, err := p.ReadResource(ctx, "mem://bound"); !errors.Is(err, errBoundProviderClosed) {
		t.Fatalf("ReadResource(bound) after owner-scoped drop must fail loud, got %v", err)
	}
	// The unbound resource (connection binding) is unaffected by the drop.
	if _, _, err := p.ReadResource(ctx, "mem://unbound"); err != nil {
		t.Fatalf("ReadResource(unbound) after drop must still authenticate: %v", err)
	}
}

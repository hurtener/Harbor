// Package integration — phase 211 (D-355): the sibling MCP connection WRITES
// resolve under the caller's own scope, end to end.
//
// Real drivers on every seam (§17.3): the real process-global mcp.Registry, the
// real mcpconsole.RegistryAccessor, the real protocol.MCPSurface (its identity
// gate, its body-scope reconciliation, and its admin-write + audit unit), a real
// events/drivers/inmem bus, a real audit/drivers/patterns redactor, and the real
// REST control transport over an httptest.Server for the identity failure mode.
//
// It asserts:
//
//   - A `set_raw_html_trust` write by a caller whose verified tenant does not own
//     the named registration is refused as not_found, and the owning tenant's
//     live flag is untouched.
//   - The refusal is indistinguishable from a name nobody registered.
//   - The owning tenant's write lands and is audited.
//   - A boot-declared (deployment-global) name stays writable — the bounded
//     guarantee this write makes.
//   - The COMPENSATING REVERT path: when the audit emit fails, the revert
//     resolves under the same verified tenant and restores the prior value, so
//     the toggle is never left observably applied but unrecorded.
//   - A missing-identity request over the real transport fails closed.
//   - N=16 concurrent cross-tenant writers against ONE shared registry leave the
//     owning tenant's value intact (§17.3 concurrency stress, under -race).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

const (
	p211OwningTenant = "tenant-p211-owner"
	p211OtherTenant  = "tenant-p211-other"
	p211User         = "user-p211"
	p211Session      = "session-p211"
	p211OwnedServer  = "p211-owned-srv"
	p211BootServer   = "p211-boot-srv"
)

func p211Owner() toolauth.Owner {
	return toolauth.Owner{Tenant: p211OwningTenant, Agent: "agent-p211"}
}

// p211Provider is a deterministic MCP provider — the real driver needs a live
// MCP wire; this satisfies the same Discover/ReadResource contract so the REAL
// registry is exercised end to end.
type p211Provider struct{ id tools.ToolSourceID }

func (p *p211Provider) SourceID() tools.ToolSourceID { return p.id }
func (p *p211Provider) Close(context.Context) error  { return nil }
func (p *p211Provider) DisplayModes() []string       { return nil }
func (p *p211Provider) ReadResource(context.Context, string) ([]byte, string, error) {
	return []byte("stub"), "text/plain", nil
}
func (p *p211Provider) Discover(ctx context.Context) ([]tools.ToolDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []tools.ToolDescriptor{{Tool: tools.Tool{Name: string(p.id) + ".echo"}}}, nil
}

// p211NoopOAuth satisfies the surface's mandatory OAuth accessor — the raw-HTML
// toggle path never touches OAuth.
type p211NoopOAuth struct{}

func (p211NoopOAuth) ListBindings(context.Context, string) ([]protocol.MCPBindingRow, error) {
	return []protocol.MCPBindingRow{}, nil
}
func (p211NoopOAuth) InitiateBinding(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (p211NoopOAuth) RevokeBinding(context.Context, string, string) (bool, error) {
	return false, nil
}

// p211Env bundles one shared registry + one surface over one real bus.
type p211Env struct {
	registry *mcpdrv.Registry
	surface  *protocol.MCPSurface
	bus      events.EventBus
	cleanup  func()
}

// buildP211Env wires the surface with real drivers over ONE shared registry
// holding two registrations: a runtime-added one owned by (p211OwningTenant,
// agent-p211) and a boot-declared (zero-owner) one.
func buildP211Env(t *testing.T) *p211Env {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}

	reg := mcpdrv.NewRegistry()
	bootCtx := p211Ctx(t, p211OwningTenant)
	if rerr := reg.Register(bootCtx, mcpdrv.ServerRegistration{
		Provider:     &p211Provider{id: tools.ToolSourceID(p211OwnedServer)},
		Transport:    "streamable-http",
		URLOrCommand: "https://mcp.example.com/rpc",
		InitialState: mcpdrv.ServerStateOnline,
		Owner:        p211Owner(),
	}); rerr != nil {
		t.Fatalf("register owned: %v", rerr)
	}
	if rerr := reg.Register(bootCtx, mcpdrv.ServerRegistration{
		Provider:     &p211Provider{id: tools.ToolSourceID(p211BootServer)},
		Transport:    "streamable-http",
		URLOrCommand: "https://boot.example.com/rpc",
		InitialState: mcpdrv.ServerStateOnline,
		// No Owner — boot-declared, deployment-global.
	}); rerr != nil {
		t.Fatalf("register boot: %v", rerr)
	}

	accessor, err := mcpconsole.NewRegistryAccessor(reg)
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}
	surface, err := protocol.NewMCPSurface(protocol.MCPDeps{
		MCP:      accessor,
		OAuth:    p211NoopOAuth{},
		Redactor: red,
		Bus:      bus,
	})
	if err != nil {
		t.Fatalf("NewMCPSurface: %v", err)
	}
	return &p211Env{
		registry: reg,
		surface:  surface,
		bus:      bus,
		cleanup:  func() { _ = bus.Close(context.Background()) },
	}
}

// p211Ctx seats a complete triple under tenant as the request's established
// identity — the shape a transport hands a surface.
func p211Ctx(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: tenant, UserID: p211User, SessionID: p211Session,
	})
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}

// p211AdminCtx is p211Ctx plus the admin scope claim the verb requires.
func p211AdminCtx(t *testing.T, tenant string) context.Context {
	t.Helper()
	return protoauth.WithScopes(p211Ctx(t, tenant), []protoauth.Scope{protoauth.ScopeAdmin})
}

// p211Toggle dispatches set_raw_html_trust as tenant.
func p211Toggle(t *testing.T, env *p211Env, tenant, name string, trusted bool) (any, error) {
	t.Helper()
	return env.surface.Dispatch(p211AdminCtx(t, tenant), methods.MethodMCPServersSetRawHTMLTrust,
		&types.MCPServerSetRawHTMLTrustRequest{
			Identity: types.IdentityScope{Tenant: tenant, User: p211User, Session: p211Session},
			Name:     name,
			Trusted:  trusted,
		})
}

// p211Trust reads the live flag back through the read projection, which stays
// bare-name and owner-blind (D-287 / D-301).
func p211Trust(t *testing.T, env *p211Env, name string) bool {
	t.Helper()
	v, err := env.registry.GetServer(p211Ctx(t, p211OtherTenant), name)
	if err != nil {
		t.Fatalf("GetServer(%q): %v", name, err)
	}
	return v.RawHTMLTrusted
}

// p211Code extracts the Protocol error code from a Dispatch error.
func p211Code(t *testing.T, err error) protoerrors.Code {
	t.Helper()
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error %v is not a *protoerrors.Error", err)
	}
	return perr.Code
}

// TestE2E_P211_CrossTenantConnectionWrite_LandsOnTheCallersOwn drives the real
// surface over the real registry: a write by a tenant that does not own the
// named registration is refused as not_found with the live flag untouched, the
// refusal is indistinguishable from an unregistered name, and the owning
// tenant's write lands.
func TestE2E_P211_CrossTenantConnectionWrite_LandsOnTheCallersOwn(t *testing.T) {
	env := buildP211Env(t)
	defer env.cleanup()

	if _, err := p211Toggle(t, env, p211OtherTenant, p211OwnedServer, true); err == nil {
		t.Fatal("a write against another tenant's registration must not succeed")
	} else if code := p211Code(t, err); code != protoerrors.CodeNotFound {
		t.Fatalf("cross-tenant write code = %q, want %q", code, protoerrors.CodeNotFound)
	}
	if got := p211Trust(t, env, p211OwnedServer); got {
		t.Fatalf("live flag after a cross-tenant write = %v, want false (untouched)", got)
	}

	// The refusal discloses nothing: an unregistered name answers identically.
	_, absentErr := p211Toggle(t, env, p211OtherTenant, "p211-nobody-registered-this", true)
	if absentErr == nil || p211Code(t, absentErr) != protoerrors.CodeNotFound {
		t.Fatalf("unregistered-name write err = %v, want not_found", absentErr)
	}

	// The owning tenant's write lands.
	resp, err := p211Toggle(t, env, p211OwningTenant, p211OwnedServer, true)
	if err != nil {
		t.Fatalf("owning-tenant write: %v", err)
	}
	typed, ok := resp.(*types.MCPServerSetRawHTMLTrustResponse)
	if !ok || !typed.Trusted || typed.Name != p211OwnedServer {
		t.Fatalf("owning-tenant response = %#v", resp)
	}
	if got := p211Trust(t, env, p211OwnedServer); !got {
		t.Fatalf("live flag after the owning tenant's write = %v, want true", got)
	}
}

// TestE2E_P211_BootDeclaredNameStaysWritable pins the bounded guarantee: a
// boot-declared server is deployment-global infrastructure whose per-server
// admin preference has no per-owner home and no other door, so the write stays
// available there.
func TestE2E_P211_BootDeclaredNameStaysWritable(t *testing.T) {
	env := buildP211Env(t)
	defer env.cleanup()

	if _, err := p211Toggle(t, env, p211OtherTenant, p211BootServer, true); err != nil {
		t.Fatalf("boot-declared write: %v", err)
	}
	if got := p211Trust(t, env, p211BootServer); !got {
		t.Fatalf("boot-declared live flag = %v, want true", got)
	}
}

// TestE2E_P211_AuditEmitFailure_CompensatingRevertResolves is the failure mode
// (§17.3 item 3) this phase specifically has to keep honest. The admin-write
// unit applies the toggle, then emits the audit event; when the emit fails it
// must COMPENSATE by reverting. The revert calls the same scoped setter a second
// time, so a revert that could fail to resolve where the apply succeeded would
// leave the toggle observably applied but unrecorded. A CLOSED bus is the real
// driver's own emit failure.
func TestE2E_P211_AuditEmitFailure_CompensatingRevertResolves(t *testing.T) {
	env := buildP211Env(t)
	defer env.cleanup()

	// Establish a non-default prior value through the owning tenant so the
	// revert has something specific to restore.
	if _, err := p211Toggle(t, env, p211OwningTenant, p211OwnedServer, true); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if got := p211Trust(t, env, p211OwnedServer); !got {
		t.Fatalf("seeded flag = %v, want true", got)
	}

	// Break the audit emit for real: the bus refuses every publish once closed.
	if err := env.bus.Close(context.Background()); err != nil {
		t.Fatalf("bus close: %v", err)
	}

	_, err := p211Toggle(t, env, p211OwningTenant, p211OwnedServer, false)
	if err == nil {
		t.Fatal("a write whose audit emit fails must not report success")
	}
	if code := p211Code(t, err); code != protoerrors.CodeRuntimeError {
		t.Fatalf("emit-failure code = %q, want %q", code, protoerrors.CodeRuntimeError)
	}
	// The message names the reverted-not-applied case, not the double-failure —
	// which is only true if the compensating revert RESOLVED and succeeded.
	msg := err.Error()
	if !strings.Contains(msg, "audit emit failed") || !strings.Contains(msg, "mutation reverted") {
		t.Fatalf("emit-failure error = %q, want the reverted-not-applied wording", msg)
	}
	if got := p211Trust(t, env, p211OwnedServer); !got {
		t.Fatalf("flag after the compensated write = %v, want the prior true (revert did not land)", got)
	}
}

// TestE2E_P211_MissingIdentity_FailsClosed drives the REAL REST control
// transport: a request with no identity scope never reaches the registry.
func TestE2E_P211_MissingIdentity_FailsClosed(t *testing.T) {
	env := buildP211Env(t)
	defer env.cleanup()

	// A real control surface over a real StateStore-backed task registry — the
	// mux's mandatory dependency.
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      env.bus,
		Redactor: audit.Redactor(auditpatterns.New()),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = taskReg.Close(context.Background()) }()
	cs, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	mux, err := transports.NewMux(cs, env.bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithoutValidator(),
		transports.WithMCPSurface(env.surface),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, err := json.Marshal(map[string]any{"name": p211OwnedServer, "trusted": true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	url := fmt.Sprintf("%s/v1/control/%s", srv.URL, string(methods.MethodMCPServersSetRawHTMLTrust))
	resp, err := postCarrierJSON(url, body, "", "", "")
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("identity-less write returned 200: %s", raw)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if decoded["code"] == "" || decoded["code"] == nil {
		t.Fatalf("identity-less write returned no typed error code: %s", raw)
	}
	if got := p211Trust(t, env, p211OwnedServer); got {
		t.Fatalf("identity-less write changed the live flag to %v", got)
	}
}

// TestE2E_P211_ConcurrentCrossTenantWriters runs N=16 writers per tenant against
// ONE shared registry through the real surface (§17.3 stress, under -race).
// Every write that lands is the owning tenant's, so the terminal value is the
// owning tenant's and the other tenant is refused throughout.
func TestE2E_P211_ConcurrentCrossTenantWriters(t *testing.T) {
	env := buildP211Env(t)
	defer env.cleanup()

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for range n {
		go func() {
			defer wg.Done()
			if _, err := p211Toggle(t, env, p211OwningTenant, p211OwnedServer, true); err != nil {
				t.Errorf("owning-tenant write: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := p211Toggle(t, env, p211OtherTenant, p211OwnedServer, false)
			if err == nil {
				t.Error("cross-tenant write must be refused")
				return
			}
			var perr *protoerrors.Error
			if !errors.As(err, &perr) || perr.Code != protoerrors.CodeNotFound {
				t.Errorf("cross-tenant write err = %v, want not_found", err)
			}
		}()
	}
	wg.Wait()

	if got := p211Trust(t, env, p211OwnedServer); !got {
		t.Fatalf("terminal flag = %v, want the owning tenant's true — a cross-tenant write landed", got)
	}
}

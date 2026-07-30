package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// wireinjectiondescriptor_test.go — the DEV-GATED wire-carried per-user
// credential-INJECTION mapping (HA-37 / D-346). Load-bearing assertions:
//
//   - With the opt-in OFF (default, all of production) a connection carrying an
//     `injection` mapping is REJECTED with ErrWireInjectionNotAllowed, and no
//     attach is attempted — the receiver-injection surface is fail-closed exactly
//     as the wire-OAuth descriptor (D-340) is.
//   - With the opt-in ON the mapping is validated, carried into the AttachRequest
//     (so the shared injection engine fires), and PERSISTED in the revision.
//   - Validation rejects a target key the audit redactor would not hold to `***`
//     (the same predicate the redactor consults) + reserved `_meta` segments.
//   - Injection is mutually exclusive with the bearer bindings (one auth mode).
//
// MUTATION-VERIFICATION: removing the `!s.allowWireInjection` guard in
// gateAndValidateInjectionDescriptor makes TestWireInjection_OptInOff_* FAIL
// (an injection mapping is then accepted with the opt-in off).

// newInjectionHarness wires a Service with a fake attacher + a real registry, and
// the wire-injection opt-in set to `allow`. It reuses fakeAttacher (defined in
// addconnection_test.go, same package).
func newInjectionHarness(t *testing.T, allow bool) (*agentcfgprotocol.Service, *fakeAttacher) {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	coord := pauseresume.New(pauseresume.WithBus(bus))
	attacher := &fakeAttacher{}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithAllowWireInjection(allow),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	return s, attacher
}

func injConn(inj *prototypes.AgentConfigMCPCredentialInjectionDescriptor) prototypes.AgentConfigMCPConnectionDescriptor {
	return prototypes.AgentConfigMCPConnectionDescriptor{
		Name:      "recv",
		Transport: "http",
		URL:       "https://mcp.example.com/sse",
		Injection: inj,
	}
}

// --- Gate OFF: an injection mapping is rejected (the fail-closed default) ---

func TestWireInjection_OptInOff_AddMCPConnection_Rejected(t *testing.T) {
	svc, attacher := newInjectionHarness(t, false)
	conn := injConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider: "vendor-broker", Form: "header", Header: "x-vendor-api-key",
	})
	_, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if !errors.Is(err, agentcfgprotocol.ErrWireInjectionNotAllowed) {
		t.Fatalf("want ErrWireInjectionNotAllowed with the opt-in off, got %v", err)
	}
	if len(attacher.calls) != 0 {
		t.Fatalf("no attach should be attempted on a gated reject, saw %d", len(attacher.calls))
	}
}

func TestWireInjection_OptInOff_NoInjection_Unaffected(t *testing.T) {
	svc, attacher := newInjectionHarness(t, false)
	conn := prototypes.AgentConfigMCPConnectionDescriptor{Name: "plain", Transport: "http", URL: "https://mcp.example.com/sse"}
	resp, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if err != nil {
		t.Fatalf("a connection without injection must be unaffected by the gate: %v", err)
	}
	if resp.State != "online" {
		t.Fatalf("state = %q, want online", resp.State)
	}
	if len(attacher.calls) != 1 || attacher.calls[0].Injection != nil {
		t.Fatalf("plain connection carried an injection binding: %+v", attacher.calls)
	}
}

// --- Gate ON: the mapping is carried into the attach + persisted ---

func TestWireInjection_OptInOn_HeaderForm_AttachCarriesAndPersists(t *testing.T) {
	svc, attacher := newInjectionHarness(t, true)
	conn := injConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider: "vendor-broker", Form: "header", Header: "x-vendor-api-key",
	})
	resp, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if err != nil {
		t.Fatalf("opt-in on: %v", err)
	}
	if resp.State != "online" {
		t.Fatalf("state = %q, want online", resp.State)
	}
	// The AttachRequest carries the injection so the shared engine fires.
	got := attacher.lastCall(t)
	if got.Injection == nil {
		t.Fatal("AttachRequest carried no injection binding")
	}
	if got.Injection.Provider != "vendor-broker" || got.Injection.Form != "header" || got.Injection.Header != "x-vendor-api-key" {
		t.Fatalf("AttachRequest injection = %+v", got.Injection)
	}
	// The response descriptor projects the injection back to the wire.
	if resp.Connection.Injection == nil || resp.Connection.Injection.Header != "x-vendor-api-key" {
		t.Fatalf("response descriptor injection = %+v", resp.Connection.Injection)
	}
	// The revision persists the injection (diff / rollback / list parity).
	rev, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rev.Revision == nil || rev.Revision.Payload.Connections == nil || len(rev.Revision.Payload.Connections.Servers) != 1 {
		t.Fatalf("revision has no persisted connection: %+v", rev.Revision)
	}
	persisted := rev.Revision.Payload.Connections.Servers[0].Injection
	if persisted == nil || persisted.Provider != "vendor-broker" || persisted.Form != "header" || persisted.Header != "x-vendor-api-key" {
		t.Fatalf("persisted injection = %+v", persisted)
	}
}

func TestWireInjection_OptInOn_BasicAndMetaForms(t *testing.T) {
	cases := map[string]*prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		"basic": {Provider: "vendor-broker", Form: "basic", BasicUsername: "svc"},
		"meta":  {Provider: "vendor-broker", Form: "meta", MetaKey: "vendor.api_key"},
	}
	for name, inj := range cases {
		t.Run(name, func(t *testing.T) {
			svc, attacher := newInjectionHarness(t, true)
			resp, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
				Identity: scope(), AgentID: testAgentID, Connection: injConn(inj),
			})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if resp.State != "online" {
				t.Fatalf("%s: state = %q", name, resp.State)
			}
			if got := attacher.lastCall(t).Injection; got == nil || got.Form != inj.Form {
				t.Fatalf("%s: AttachRequest injection = %+v", name, got)
			}
		})
	}
}

// --- Validation (opt-in ON): malformed mappings fail loud ---

func TestWireInjection_OptInOn_ValidationRejects(t *testing.T) {
	cases := map[string]*prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		"empty provider":           {Provider: "", Form: "header", Header: "x-vendor-api-key"},
		"empty form":               {Provider: "vendor-broker", Form: "", Header: "x-vendor-api-key"},
		"unknown form":             {Provider: "vendor-broker", Form: "cookie", Header: "x-vendor-api-key"},
		"header empty":             {Provider: "vendor-broker", Form: "header"},
		"header is authorization":  {Provider: "vendor-broker", Form: "header", Header: "Authorization"},
		"header not credential":    {Provider: "vendor-broker", Form: "header", Header: "x-request-id"},
		"meta empty":               {Provider: "vendor-broker", Form: "meta"},
		"meta leaf not credential": {Provider: "vendor-broker", Form: "meta", MetaKey: "vendor.request_id"},
		"meta reserved segment":    {Provider: "vendor-broker", Form: "meta", MetaKey: "tenant.api_key"},
	}
	for name, inj := range cases {
		t.Run(name, func(t *testing.T) {
			svc, attacher := newInjectionHarness(t, true)
			_, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
				Identity: scope(), AgentID: testAgentID, Connection: injConn(inj),
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
				t.Fatalf("%s: want ErrInvalidConnection, got %v", name, err)
			}
			if len(attacher.calls) != 0 {
				t.Fatalf("%s: no attach should be attempted on a validation reject", name)
			}
		})
	}
}

// --- Mutual exclusivity + transport constraints ---

func TestWireInjection_MutualExclusivity(t *testing.T) {
	svc, _ := newInjectionHarness(t, true)
	inj := &prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "header", Header: "x-vendor-api-key"}

	// injection + oauth_provider (name binding).
	conn := injConn(inj)
	conn.OAuthProvider = "some-provider"
	if _, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	}); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("injection + oauth_provider must be rejected, got %v", err)
	}

	// injection on a stdio transport (no HTTP request to inject into).
	stdio := prototypes.AgentConfigMCPConnectionDescriptor{Name: "s", Transport: "stdio", Command: []string{"/bin/true"}, Injection: inj}
	if _, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: stdio,
	}); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("injection on stdio must be rejected, got %v", err)
	}
}

// --- Concurrent-reuse: the shared Service handles N≥100 concurrent injection
// adds under -race with no data race and each carries its own binding (D-025). ---

func TestWireInjection_ConcurrentAdds_NoRace(t *testing.T) {
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	attacher := &fakeAttacher{}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(pauseresume.New(pauseresume.WithBus(bus))),
		agentcfgprotocol.WithAllowWireInjection(true),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const n = 128
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct agent per goroutine → distinct registry rows, no name collision.
			agentID := fmt.Sprintf("inj-agent-%d", i)
			conn := injConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
				Provider: "vendor-broker", Form: "header", Header: "x-vendor-api-key",
			})
			_, errs[i] = svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
				Identity: scope(), AgentID: agentID, Connection: conn,
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent add %d failed: %v", i, e)
		}
	}
	attacher.mu.Lock()
	got := len(attacher.calls)
	for _, c := range attacher.calls {
		if c.Injection == nil || c.Injection.Header != "x-vendor-api-key" {
			attacher.mu.Unlock()
			t.Fatalf("a concurrent attach carried a wrong/missing injection: %+v", c.Injection)
		}
	}
	attacher.mu.Unlock()
	if got != n {
		t.Fatalf("attacher saw %d calls, want %d", got, n)
	}
}

// --- set_revision: the SECOND persistence door is gated identically (D-346
// review MAJOR fix) — an injection mapping cannot slip past the fail-closed gate
// or the shape validation through a full-payload set_revision. ---

func setRevInjConn(inj *prototypes.AgentConfigMCPCredentialInjectionDescriptor) prototypes.AgentConfigSetRevisionRequest {
	return prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Connections: &prototypes.AgentConfigConnections{
				Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
					{Name: "recv", Transport: "http", URL: "https://mcp.example.com/sse", Injection: inj},
				},
			},
		},
	}
}

func TestWireInjection_SetRevision_OptInOff_RejectedNothingPersisted(t *testing.T) {
	svc, _ := newInjectionHarness(t, false)
	_, err := svc.SetRevision(context.Background(), setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider: "vendor-broker", Form: "header", Header: "x-vendor-api-key",
	}))
	if !errors.Is(err, agentcfgprotocol.ErrWireInjectionNotAllowed) {
		t.Fatalf("set_revision with injection + opt-in off: want ErrWireInjectionNotAllowed, got %v", err)
	}
	// Nothing persisted: the agent has no active revision.
	got, gerr := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.Set {
		t.Fatalf("a gated-off set_revision must persist NOTHING, but an active revision exists: %+v", got.Revision)
	}
}

func TestWireInjection_SetRevision_OptInOn_MalformedRejected(t *testing.T) {
	deep := strings.Repeat("x.", 16) + "token" // 17 segments — exceeds the cap
	cases := map[string]prototypes.AgentConfigSetRevisionRequest{
		"authorization header":     setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "header", Header: "Authorization"}),
		"non-credential header":    setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "header", Header: "x-vendor-debug"}),
		"reserved meta segment":    setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "meta", MetaKey: "tenant.api_key"}),
		"non-credential meta leaf": setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "meta", MetaKey: "vendor.debug"}),
		"meta_key too deep":        setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "meta", MetaKey: deep}),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _ := newInjectionHarness(t, true)
			_, err := svc.SetRevision(context.Background(), req)
			if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
				t.Fatalf("%s: want ErrInvalidConnection, got %v", name, err)
			}
			got, _ := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if got.Set {
				t.Fatalf("%s: a rejected set_revision must persist nothing", name)
			}
		})
	}
}

func TestWireInjection_SetRevision_OptInOn_MutualExclusivityAndStdioRejected(t *testing.T) {
	svc, _ := newInjectionHarness(t, true)
	inj := &prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "header", Header: "x-vendor-api-key"}

	withOAuthProvider := setRevInjConn(inj)
	withOAuthProvider.Payload.Connections.Servers[0].OAuthProvider = "some-provider"
	if _, err := svc.SetRevision(context.Background(), withOAuthProvider); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("injection + oauth_provider via set_revision must be rejected, got %v", err)
	}

	stdio := prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			{Name: "s", Transport: "stdio", Command: []string{"/bin/true"}, Injection: inj},
		}}},
	}
	if _, err := svc.SetRevision(context.Background(), stdio); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("injection on stdio via set_revision must be rejected, got %v", err)
	}
}

func TestWireInjection_SetRevision_OptInOn_ValidPersistsAndRoundTrips(t *testing.T) {
	svc, _ := newInjectionHarness(t, true)
	req := setRevInjConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider: "vendor-broker", Form: "meta", MetaKey: "vendor.api_key",
	})
	first, err := svc.SetRevision(context.Background(), req)
	if err != nil {
		t.Fatalf("valid set_revision: %v", err)
	}
	// get parity.
	got, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	persisted := got.Revision.Payload.Connections.Servers[0].Injection
	if persisted == nil || persisted.Provider != "vendor-broker" || persisted.Form != "meta" || persisted.MetaKey != "vendor.api_key" {
		t.Fatalf("get: persisted injection = %+v", persisted)
	}
	// list parity.
	list, err := svc.ListRevisions(context.Background(), prototypes.AgentConfigListRevisionsRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(list.Revisions) == 0 || list.Revisions[0].Payload.Connections == nil ||
		list.Revisions[0].Payload.Connections.Servers[0].Injection == nil ||
		list.Revisions[0].Payload.Connections.Servers[0].Injection.MetaKey != "vendor.api_key" {
		t.Fatalf("list: injection not surfaced: %+v", list.Revisions)
	}
	// diff parity: a second revision that drops the injection compares cleanly.
	second, err := svc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
			{Name: "recv", Transport: "http", URL: "https://mcp.example.com/sse"},
		}}},
	})
	if err != nil {
		t.Fatalf("second set_revision: %v", err)
	}
	if _, err := svc.Diff(context.Background(), prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID, FromRevision: first.Revision.RevisionID, ToRevision: second.Revision.RevisionID,
	}); err != nil {
		t.Fatalf("Diff over injection-bearing revisions: %v", err)
	}
}

// --- meta_key depth cap (D-346 review MINOR): a pathological deep meta_key that
// could nest a credential below the audit redactor's deep-walk is rejected at
// validation, on the add path too. ---

func TestWireInjection_AddMCPConnection_MetaKeyTooDeep_Rejected(t *testing.T) {
	svc, attacher := newInjectionHarness(t, true)
	deep := strings.Repeat("x.", config.MaxMCPMetaKeyDepth) + "token" // one segment past config.MaxMCPMetaKeyDepth
	_, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID,
		Connection: injConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "vendor-broker", Form: "meta", MetaKey: deep}),
	})
	if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("meta_key exceeding the depth cap must be rejected, got %v", err)
	}
	if len(attacher.calls) != 0 {
		t.Fatal("no attach should be attempted on a depth-cap reject")
	}
}

// --- Annotation paths and the injection `_meta` path share ONE namespace, so
// the wire door checks them TOGETHER. Without this, a flat `vendor` annotation
// alongside `injection.meta_key: vendor.api_key` is accepted and then silently
// resolved at merge time — the operator's annotation discarded, no error, no
// log. ---

func TestWireInjection_AddMCPConnection_AnnotationCollidesWithMetaKey_Rejected(t *testing.T) {
	svc, attacher := newInjectionHarness(t, true)
	conn := injConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider: "vendor-broker", Form: "meta", MetaKey: "vendor.api_key",
	})
	conn.MetaAnnotations = map[string]string{"vendor": "flat"}
	_, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	})
	if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("a flat annotation colliding with the injection _meta path must be rejected, got %v", err)
	}
	for _, want := range []string{"collide", "vendor", "vendor.api_key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err=%q must name %q", err.Error(), want)
		}
	}
	if len(attacher.calls) != 0 {
		t.Fatal("no attach should be attempted on a collision reject")
	}
}

func TestWireInjection_AddMCPConnection_AnnotationSharesMetaKeyNamespace_Accepted(t *testing.T) {
	svc, _ := newInjectionHarness(t, true)
	conn := injConn(&prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider: "vendor-broker", Form: "meta", MetaKey: "vendor.api_key",
	})
	// The arrangement this phase exists to enable: the receiver reads ONE
	// nested namespace and gets both the credential and its non-secret
	// companion, without the companion having to ride the credential channel.
	conn.MetaAnnotations = map[string]string{"vendor.account_id": "acct-42"}
	if _, err := svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn,
	}); err != nil {
		t.Fatalf("a nested annotation sharing the injection namespace was rejected: %v", err)
	}
}

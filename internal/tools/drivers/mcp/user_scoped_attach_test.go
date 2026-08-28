package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// TestUserScopedAttach_AllowsSameLogicalDescriptorPerUser proves the live
// identity boundary, not only the durable desired-state boundary. Both users
// attach the exact same logical name, transport, URL, and discovered tool set
// to one agent. The attach seam derives distinct physical source ids from the
// verified owner, so both providers remain callable and removing A cannot
// withdraw B's catalog or transport.
func TestUserScopedAttach_AllowsSameLogicalDescriptorPerUser(t *testing.T) {
	remote := newMockServer()
	server := httptest.NewServer(mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return remote.server }, nil))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cat := tools.NewCatalog()
	reg := NewRegistry()
	ownerA := auth.Owner{Tenant: "tenant", Agent: "agent", User: "user-a"}
	ownerB := auth.Owner{Tenant: "tenant", Agent: "agent", User: "user-b"}
	const logicalName = "shared-offer"

	prepare := func(owner auth.Owner) *PreparedAttachment {
		t.Helper()
		closers := []func(context.Context) error{}
		prepared, err := Prepare(ctx, config.MCPServerConfig{
			Name: logicalName, TransportMode: string(TransportSSE), URL: server.URL,
		}, AttachDeps{
			Catalog: cat, Registry: reg, Bus: newTestBus(t), DefaultIdentity: defaultIdentity(),
			Closers: &closers, Owner: owner, LogicalName: logicalName,
			DescriptorFingerprint: "same-signed-descriptor",
		})
		if err != nil {
			t.Fatalf("prepare %s: %v", owner.User, err)
		}
		t.Cleanup(func() { _ = prepared.Close(context.Background()) })
		return prepared
	}

	preparedA := prepare(ownerA)
	preparedB := prepare(ownerB)
	if err := preparedA.Activate(ctx); err != nil {
		t.Fatalf("activate user A: %v", err)
	}
	if err := preparedB.Activate(ctx); err != nil {
		t.Fatalf("activate user B with same logical descriptor: %v", err)
	}

	physicalA := PhysicalServerName(logicalName, ownerA)
	physicalB := PhysicalServerName(logicalName, ownerB)
	if physicalA == physicalB || physicalA == logicalName || physicalB == logicalName {
		t.Fatalf("user physical names are not distinct from logical name: A=%q B=%q", physicalA, physicalB)
	}
	if ids := reg.SourceIDs(); len(ids) != 2 {
		t.Fatalf("physical registry sources = %v, want two user-owned providers", ids)
	}
	for _, tc := range []struct {
		name  string
		owner auth.Owner
		key   string
	}{
		{name: "A", owner: ownerA, key: physicalA},
		{name: "B", owner: ownerB, key: physicalB},
	} {
		if got, fingerprint, ok := reg.RegistrationIdentityForOwner(logicalName, tc.owner); !ok || got != tc.owner || fingerprint != "same-signed-descriptor" {
			t.Fatalf("user %s registration identity = (%+v, %q, %t)", tc.name, got, fingerprint, ok)
		}
		logical, ok := reg.LogicalNameOfSource(tools.ToolSourceID(tc.key))
		if !ok || logical != logicalName {
			t.Fatalf("user %s logical source = (%q, %t), want %q", tc.name, logical, ok, logicalName)
		}
	}

	invoke := func(owner auth.Owner, source string, text string) {
		t.Helper()
		tool, ok := cat.Resolve(source + "_echo")
		if !ok {
			t.Fatalf("catalog missing %s tool", owner.User)
		}
		callCtx, err := identity.With(ctx, identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "session-" + owner.User})
		if err != nil {
			t.Fatalf("identity context %s: %v", owner.User, err)
		}
		result, err := tool.Invoke(callCtx, json.RawMessage(`{"text":"`+text+`"}`))
		if err != nil {
			t.Fatalf("invoke %s: %v", owner.User, err)
		}
		if result.Value == nil {
			t.Fatalf("invoke %s returned no value", owner.User)
		}
	}
	invoke(ownerA, physicalA, "A")
	invoke(ownerB, physicalB, "B")

	removed, err := reg.DeregisterExactPublisher(ctx, logicalName, ownerA, "same-signed-descriptor", func() int {
		return cat.(tools.CatalogSourceDeregisterer).DeregisterSource(tools.ToolSourceID(physicalA))
	})
	if err != nil || removed == 0 {
		t.Fatalf("remove user A = removed %d, err %v", removed, err)
	}
	if _, ok := cat.Resolve(physicalA + "_echo"); ok {
		t.Fatal("user A tool remained after exact removal")
	}
	if _, ok := cat.Resolve(physicalB + "_echo"); !ok {
		t.Fatal("user B tool was removed by user A exact teardown")
	}
	if _, _, ok := reg.RegistrationIdentityForOwner(logicalName, ownerA); ok {
		t.Fatal("user A physical registration remained after exact removal")
	}
	if got, _, ok := reg.RegistrationIdentityForOwner(logicalName, ownerB); !ok || got != ownerB {
		t.Fatalf("user B physical registration after A removal = (%+v, %t)", got, ok)
	}
	invoke(ownerB, physicalB, "B-after-A-removal")
}

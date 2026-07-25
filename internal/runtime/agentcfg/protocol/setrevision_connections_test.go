package protocol_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// setrevision_connections_test.go — the full-payload `agent_config.set_revision`
// door holds every MCP connection descriptor to the SAME shape the
// `add_mcp_connection` door enforces. Both doors persist onto the same revision
// spine, so both apply the same validator: a descriptor that reaches the spine
// through set_revision is one the add door would have accepted.

// wireConn is the wire connection descriptor under test (alias for brevity).
type wireConn = prototypes.AgentConfigMCPConnectionDescriptor

// connPayload wraps wire connection descriptors into a full-payload request.
func connPayload(servers ...wireConn) prototypes.AgentConfigSetRevisionRequest {
	return prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Connections: &prototypes.AgentConfigConnections{Servers: servers},
		},
	}
}

// TestSetRevision_Connections_MalformedRejectedNothingPersisted walks the shape
// rules the add door enforces and proves each one holds at the full-payload
// door too, with NOTHING persisted: the active revision stays unset.
func TestSetRevision_Connections_MalformedRejectedNothingPersisted(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		conn wireConn
	}{
		{"empty name", wireConn{}},
		{"blank name", wireConn{Name: "   ", Transport: "http", URL: "https://x.invalid/rpc"}},
		{"unknown transport", wireConn{Name: "srv", Transport: "carrier-pigeon"}},
		{"empty transport", wireConn{Name: "srv", URL: "https://x.invalid/rpc"}},
		{"http without url", wireConn{Name: "srv", Transport: "http"}},
		{"http carrying a command", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", Command: []string{"server-bin"}}},
		{"stdio without a command", wireConn{Name: "srv", Transport: "stdio"}},
		{"stdio with a blank argv[0]", wireConn{Name: "srv", Transport: "stdio", Command: []string{"  "}}},
		{"stdio carrying a url", wireConn{Name: "srv", Transport: "stdio", Command: []string{"server-bin"}, URL: "https://x.invalid/rpc"}},
		{"stdio binding an oauth_provider", wireConn{Name: "srv", Transport: "stdio", Command: []string{"server-bin"}, OAuthProvider: "gh"}},
		{"stdio carrying discovery origins", wireConn{Name: "srv", Transport: "stdio", Command: []string{"server-bin"}, OAuthDiscoveryAllowedOrigins: []string{"https://as.example.net"}}},
		{"malformed discovery origin", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"http://as.example.net"}}},
		{"discovery origin with a path", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", OAuthDiscoveryAllowedOrigins: []string{"https://as.example.net/path"}}},
		{"reserved meta annotation key", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", MetaAnnotations: map[string]string{"tenant": "t"}}},
		{"spec-reserved meta annotation prefix", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", MetaAnnotations: map[string]string{"io.modelcontextprotocol/ui": "x"}}},
		{"empty meta annotation key", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", MetaAnnotations: map[string]string{"": "v"}}},
		{"two auth modes", wireConn{Name: "srv", Transport: "http", URL: "https://x.invalid/rpc", OAuthProvider: "gh", OAuth: &prototypes.AgentConfigOAuthProviderDescriptor{Name: "gh"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := agentcfgprotocol.NewService(newRegistry(t))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if _, err := s.SetRevision(ctx, connPayload(tc.conn)); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
				t.Fatalf("error = %v, want ErrInvalidConnection", err)
			}
			got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if gErr != nil {
				t.Fatalf("Get: %v", gErr)
			}
			if got.Set {
				t.Fatal("a rejected connection set still persisted a revision")
			}
		})
	}
}

// TestSetRevision_Connections_StdioAllowlistGatesBothDoors proves the
// fail-closed stdio command allowlist — the §7 RCE gate — applies at the
// full-payload door exactly as it does at add_mcp_connection: the SAME
// descriptor is refused with the SAME sentinel at both doors, an allowlisted
// command is accepted at both, and a refusal persists nothing. Both doors write
// the same revision spine, so a command one refuses cannot enter through the
// other.
func TestSetRevision_Connections_StdioAllowlistGatesBothDoors(t *testing.T) {
	ctx := context.Background()
	const allowed = "/usr/local/bin/harbor-mcptest-stdio"
	denied := wireConn{Name: "srv", Transport: "stdio", Command: []string{"/bin/sh", "-c", "id"}}

	// --- The full-payload door refuses an un-allowlisted command.
	s, err := agentcfgprotocol.NewService(newRegistry(t),
		agentcfgprotocol.WithStdioAllowlist([]string{allowed}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, connPayload(denied)); !errors.Is(err, agentcfgprotocol.ErrStdioNotAllowed) {
		t.Fatalf("set_revision with an un-allowlisted stdio command: err = %v, want ErrStdioNotAllowed", err)
	}
	got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if got.Set {
		t.Fatal("a refused stdio set_revision still persisted a revision")
	}

	// --- The add door refuses the SAME descriptor with the SAME sentinel.
	addSvc, err := agentcfgprotocol.NewService(newRegistry(t),
		agentcfgprotocol.WithStdioAllowlist([]string{allowed}),
		agentcfgprotocol.WithConnectionAttacher(&fakeAttacher{}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := addSvc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: denied,
	}); !errors.Is(err, agentcfgprotocol.ErrStdioNotAllowed) {
		t.Fatalf("add_mcp_connection with the same command: err = %v, want ErrStdioNotAllowed (both doors agree)", err)
	}

	// --- An ALLOWLISTED command still lands through the full-payload door.
	if _, err := s.SetRevision(ctx, connPayload(wireConn{
		Name: "srv", Transport: "stdio", Command: []string{allowed, "--flag"},
	})); err != nil {
		t.Fatalf("set_revision with an allowlisted stdio command: %v", err)
	}
	got, gErr = s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if !got.Set || got.Revision == nil || got.Revision.Payload.Connections == nil ||
		len(got.Revision.Payload.Connections.Servers) != 1 ||
		got.Revision.Payload.Connections.Servers[0].Command[0] != allowed {
		t.Fatalf("allowlisted stdio descriptor did not persist: %#v", got.Revision)
	}
}

// TestSetRevision_Connections_EmptyStdioAllowlistRefusesEveryStdio pins the
// fail-closed default: a Service with no configured allowlist admits no stdio
// descriptor at all through the full-payload door.
func TestSetRevision_Connections_EmptyStdioAllowlistRefusesEveryStdio(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, connPayload(wireConn{
		Name: "srv", Transport: "stdio", Command: []string{"server-bin"},
	})); !errors.Is(err, agentcfgprotocol.ErrStdioNotAllowed) {
		t.Fatalf("empty allowlist: err = %v, want ErrStdioNotAllowed (fail-closed)", err)
	}
	got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if got.Set {
		t.Fatal("a refused stdio set_revision still persisted a revision")
	}
}

// TestSetRevision_Connections_PersistsNormalizedDescriptor proves the
// full-payload door records the validator's NORMALISED descriptor, not the raw
// wire values — the same bytes (and therefore the same content hash) the add
// and allowance doors record for the same logical input.
func TestSetRevision_Connections_PersistsNormalizedDescriptor(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, connPayload(wireConn{
		Name:      "  srv  ",
		Transport: "http",
		URL:       "  https://x.invalid/rpc  ",
		// Padded and duplicated — the shared origin validator trims and de-dups.
		OAuthDiscoveryAllowedOrigins: []string{" https://as.example.net ", "https://as.example.net", "  "},
	})); err != nil {
		t.Fatalf("set_revision: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	d := got.Revision.Payload.Connections.Servers[0]
	if d.Name != "srv" {
		t.Errorf("persisted name = %q, want the trimmed %q", d.Name, "srv")
	}
	if d.URL != "https://x.invalid/rpc" {
		t.Errorf("persisted url = %q, want the trimmed %q", d.URL, "https://x.invalid/rpc")
	}
	if len(d.OAuthDiscoveryAllowedOrigins) != 1 || d.OAuthDiscoveryAllowedOrigins[0] != "https://as.example.net" {
		t.Errorf("persisted origins = %#v, want the normalised [https://as.example.net]", d.OAuthDiscoveryAllowedOrigins)
	}
}

// TestSetRevision_Connections_RejectsTheWholeSetOnOneOffender proves the write
// is all-or-nothing: a set whose FIRST descriptor is well-formed and whose
// second is not persists neither, and the offending index is named.
func TestSetRevision_Connections_RejectsTheWholeSetOnOneOffender(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = s.SetRevision(ctx, connPayload(
		wireConn{Name: "good", Transport: "http", URL: "https://x.invalid/rpc"},
		wireConn{Name: "bad", Transport: "http"}, // no url
	))
	if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("error = %v, want ErrInvalidConnection", err)
	}
	if !strings.Contains(err.Error(), "connections.servers[1]") {
		t.Errorf("error %q does not name the offending index", err.Error())
	}
	got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if got.Set {
		t.Fatal("a set with one malformed descriptor persisted a revision")
	}
}

// TestSetRevision_Connections_ActiveRevisionUnchangedOnReject proves a rejected
// write leaves an ALREADY-ACTIVE revision exactly as it was — the reject
// happens before the registry write, so there is no half-applied set.
func TestSetRevision_Connections_ActiveRevisionUnchangedOnReject(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	first, err := s.SetRevision(ctx, connPayload(wireConn{
		Name: "srv", Transport: "http", URL: "https://x.invalid/rpc",
		OAuthDiscoveryAllowedOrigins: []string{"https://as.example.net"},
	}))
	if err != nil {
		t.Fatalf("seed set_revision: %v", err)
	}
	if _, err := s.SetRevision(ctx, connPayload(wireConn{
		Name: "srv", Transport: "stdio", Command: []string{"server-bin"}, URL: "https://x.invalid/rpc",
	})); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
		t.Fatalf("error = %v, want ErrInvalidConnection", err)
	}
	got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if !got.Set || got.Revision == nil {
		t.Fatal("the seeded active revision vanished")
	}
	if got.Revision.RevisionID != first.Revision.RevisionID {
		t.Fatalf("active revision = %q, want the pre-reject %q", got.Revision.RevisionID, first.Revision.RevisionID)
	}
	if got.Revision.Payload.Connections == nil ||
		len(got.Revision.Payload.Connections.Servers) != 1 ||
		got.Revision.Payload.Connections.Servers[0].Transport != "http" {
		t.Fatalf("active descriptor mutated by a rejected write: %#v", got.Revision.Payload.Connections)
	}
}

// TestSetRevision_Connections_ValidPersistsAndRoundTrips proves a well-formed
// descriptor still lands and survives get / list / diff intact — including the
// OAuth-discovery allow-list, which is part of the persisted descriptor.
func TestSetRevision_Connections_ValidPersistsAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	first, err := s.SetRevision(ctx, connPayload(wireConn{
		Name: "srv", Transport: "http", URL: "https://x.invalid/rpc",
		OAuthDiscoveryAllowedOrigins: []string{"https://as.example.net"},
		MetaAnnotations:              map[string]string{"vendor.tag": "blue"},
	}))
	if err != nil {
		t.Fatalf("set_revision: %v", err)
	}

	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Set || got.Revision == nil || got.Revision.Payload.Connections == nil {
		t.Fatal("connections section absent after a valid write")
	}
	servers := got.Revision.Payload.Connections.Servers
	if len(servers) != 1 || servers[0].Name != "srv" || servers[0].URL != "https://x.invalid/rpc" {
		t.Fatalf("descriptor round-trip = %#v", servers)
	}
	if len(servers[0].OAuthDiscoveryAllowedOrigins) != 1 || servers[0].OAuthDiscoveryAllowedOrigins[0] != "https://as.example.net" {
		t.Fatalf("discovery allow-list did not round-trip: %#v", servers[0].OAuthDiscoveryAllowedOrigins)
	}
	if servers[0].MetaAnnotations["vendor.tag"] != "blue" {
		t.Fatalf("meta annotations did not round-trip: %#v", servers[0].MetaAnnotations)
	}

	revs, err := s.ListRevisions(ctx, prototypes.AgentConfigListRevisionsRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs.Revisions) == 0 {
		t.Fatal("ListRevisions returned no revisions")
	}

	second, err := s.SetRevision(ctx, connPayload(wireConn{
		Name: "srv", Transport: "http", URL: "https://x.invalid/rpc",
	}))
	if err != nil {
		t.Fatalf("second set_revision: %v", err)
	}
	if _, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: first.Revision.RevisionID, ToRevision: second.Revision.RevisionID,
	}); err != nil {
		t.Fatalf("Diff: %v", err)
	}
}

// TestSetRevision_Connections_NilSectionAccepted pins the carry-forward case: a
// payload that pins no connections section is valid (it declares nothing about
// connections), not an empty declaration to validate.
func TestSetRevision_Connections_NilSectionAccepted(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	base := "hello"
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			PromptLayers: &prototypes.AgentConfigPromptLayers{Base: &base},
		},
	}); err != nil {
		t.Fatalf("set_revision with no connections section: %v", err)
	}
}

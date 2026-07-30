package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// artifact_egress_test.go — the egress-substitution declaration
// (`artifact_byte_eligible` + `artifact_params`) at BOTH persistence
// doors.
//
// The rules are enforced by the ONE shared validator both doors already
// run, so every case below is asserted at `set_revision` AND at
// `add_mcp_connection` with the SAME sentinel — a declaration one door
// refuses cannot enter through the other, and both doors record the same
// normalised bytes so a re-order cannot perturb the revision hash.

// egressConn is a byte-eligible http connection carrying a mapping.
func egressConn(eligible bool, params map[string][]string) wireConn {
	return wireConn{
		Name:                 "docstore",
		Transport:            "http",
		URL:                  "https://docs.invalid/mcp",
		ArtifactByteEligible: eligible,
		ArtifactParams:       params,
	}
}

// TestArtifactEgress_RefusedAtBothDoors_NothingPersisted walks every
// refusal and proves it holds at BOTH doors with nothing persisted.
func TestArtifactEgress_RefusedAtBothDoors_NothingPersisted(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		conn wireConn
	}{
		{
			// The eligibility flag IS the containment boundary, so a mapping
			// without it is refused rather than stored inert — a persisted
			// mapping that does nothing is exactly what an operator would
			// later read as "egress is configured".
			"mapping on a NON-eligible connection",
			egressConn(false, map[string][]string{"ingest": {"doc"}}),
		},
		{
			"empty tool name",
			egressConn(true, map[string][]string{"   ": {"doc"}}),
		},
		{
			"tool mapping no parameters",
			egressConn(true, map[string][]string{"ingest": {}}),
		},
		{
			"empty parameter name",
			egressConn(true, map[string][]string{"ingest": {"doc", "  "}}),
		},
		{
			"duplicate parameter",
			egressConn(true, map[string][]string{"ingest": {"doc", "doc"}}),
		},
		{
			// Base64-encoded artifact bytes belong in an HTTP body, not a
			// stdio frame — the same http-only rule the sibling fields use.
			"stdio carrying a mapping",
			wireConn{Name: "docstore", Transport: "stdio", Command: []string{"server-bin"},
				ArtifactByteEligible: true, ArtifactParams: map[string][]string{"ingest": {"doc"}}},
		},
		{
			// Even the bare flag: a declaration this transport can never
			// exercise advertises a capability nothing services.
			"stdio declaring bare eligibility",
			wireConn{Name: "docstore", Transport: "stdio", Command: []string{"server-bin"},
				ArtifactByteEligible: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// --- The full-payload door.
			s, err := agentcfgprotocol.NewService(newRegistry(t))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if _, err := s.SetRevision(ctx, connPayload(tc.conn)); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
				t.Fatalf("set_revision: err = %v, want ErrInvalidConnection", err)
			}
			got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if gErr != nil {
				t.Fatalf("Get: %v", gErr)
			}
			if got.Set {
				t.Fatal("a rejected egress declaration still persisted a revision")
			}

			// --- The add door, same descriptor, same sentinel.
			addSvc, err := agentcfgprotocol.NewService(newRegistry(t),
				agentcfgprotocol.WithConnectionAttacher(&fakeAttacher{}),
				agentcfgprotocol.WithStdioAllowlist([]string{"server-bin"}))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if _, err := addSvc.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
				Identity: scope(), AgentID: testAgentID, Connection: tc.conn,
			}); !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
				t.Fatalf("add_mcp_connection: err = %v, want ErrInvalidConnection (both doors must agree)", err)
			}
			addGot, gErr := addSvc.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if gErr != nil {
				t.Fatalf("Get: %v", gErr)
			}
			if addGot.Set {
				t.Fatal("a rejected egress declaration still persisted a revision at the add door")
			}
		})
	}
}

// TestArtifactEgress_PersistsAndReadsBackNormalised proves the happy
// path lands and round-trips through `agent_config.get` in NORMALISED
// form — parameters sorted, so both doors record identical bytes for
// identical logical input and a re-order cannot perturb the content
// hash.
func TestArtifactEgress_PersistsAndReadsBackNormalised(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Declared out of order on purpose.
	conn := egressConn(true, map[string][]string{"ingest": {"zeta", "alpha"}})
	if _, err := s.SetRevision(ctx, connPayload(conn)); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Set || got.Revision == nil || got.Revision.Payload.Connections == nil {
		t.Fatalf("the revision did not persist the connections section")
	}
	servers := got.Revision.Payload.Connections.Servers
	if len(servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(servers))
	}
	if !servers[0].ArtifactByteEligible {
		t.Fatalf("artifact_byte_eligible did not round-trip")
	}
	params := servers[0].ArtifactParams["ingest"]
	if len(params) != 2 || params[0] != "alpha" || params[1] != "zeta" {
		t.Fatalf("artifact_params = %v, want deterministically sorted [alpha zeta]", params)
	}
}

// TestArtifactEgress_UndeclaredConnectionRoundTripsUnchanged is the
// no-op guarantee at the wire boundary: a connection that declares
// neither field round-trips with both absent, so an existing revision is
// untouched by this phase.
func TestArtifactEgress_UndeclaredConnectionRoundTripsUnchanged(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	conn := wireConn{Name: "plain", Transport: "http", URL: "https://plain.invalid/mcp"}
	if _, err := s.SetRevision(ctx, connPayload(conn)); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	servers := got.Revision.Payload.Connections.Servers
	if len(servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(servers))
	}
	if servers[0].ArtifactByteEligible {
		t.Errorf("an undeclared connection came back byte-eligible")
	}
	if servers[0].ArtifactParams != nil {
		t.Errorf("an undeclared connection came back with a mapping: %v", servers[0].ArtifactParams)
	}
}

// TestArtifactEgress_EligibleWithoutAMappingIsAccepted — declaring a
// connection eligible without mapping anything yet is a legitimate
// intermediate state (the operator arms the connection, then maps
// parameters once the server's tool names are known). It must not be
// refused as though it were the reverse case.
func TestArtifactEgress_EligibleWithoutAMappingIsAccepted(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, connPayload(egressConn(true, nil))); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Revision.Payload.Connections.Servers[0].ArtifactByteEligible {
		t.Fatalf("bare eligibility did not persist")
	}
}

// TestArtifactEgress_CarriedIntoTheAttachRequest closes the wiring gap
// this exact door has hit before: a field on the descriptor that nothing
// carries forward is INERT. The attacher receives the declaration, so
// the driver's egress engine is actually armed for the connection.
func TestArtifactEgress_CarriedIntoTheAttachRequest(t *testing.T) {
	ctx := context.Background()
	att := &recordingAttacher{}
	s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithConnectionAttacher(att))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID,
		Connection: egressConn(true, map[string][]string{"ingest": {"doc"}}),
	}); err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	if att.req.Name == "" {
		t.Fatalf("the attacher was never called, so this assertion would pass vacuously")
	}
	if !att.req.ArtifactByteEligible {
		t.Errorf("the attach request did not carry artifact_byte_eligible; the declaration is inert")
	}
	if got := att.req.ArtifactParams["ingest"]; len(got) != 1 || got[0] != "doc" {
		t.Errorf("the attach request carried artifact_params = %v, want [doc]", got)
	}
}

// recordingAttacher captures the AttachRequest so the carry-forward can
// be asserted at the seam rather than inferred.
type recordingAttacher struct {
	req agentcfgprotocol.AttachRequest
}

func (a *recordingAttacher) Attach(_ context.Context, req agentcfgprotocol.AttachRequest) error {
	a.req = req
	return nil
}

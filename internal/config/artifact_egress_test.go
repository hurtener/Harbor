package config_test

import (
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// artifact_egress_test.go — the BOOT-side half of the egress-substitution
// declaration: the same rules the two control-plane doors enforce,
// checked one stage earlier so an operator learns at `harbor validate`
// rather than at boot.

func TestValidateMCPArtifactParams_ShapeRefusals(t *testing.T) {
	cases := []struct {
		name string
		in   config.MCPArtifactParams
		want string
	}{
		{"empty tool name", config.MCPArtifactParams{"  ": {"doc"}}, "tool name must not be empty"},
		{"tool maps nothing", config.MCPArtifactParams{"ingest": {}}, "maps no parameter names"},
		{"empty parameter name", config.MCPArtifactParams{"ingest": {"doc", " "}}, "empty parameter name"},
		{"duplicate parameter", config.MCPArtifactParams{"ingest": {"doc", "doc"}}, "twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateMCPArtifactParams(tc.in)
			if err == nil {
				t.Fatalf("a malformed mapping was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to explain %q", err, tc.want)
			}
		})
	}
}

func TestValidateMCPArtifactParams_EmptyIsValid(t *testing.T) {
	if err := config.ValidateMCPArtifactParams(nil); err != nil {
		t.Fatalf("a nil mapping was refused: %v", err)
	}
	if err := config.ValidateMCPArtifactParams(config.MCPArtifactParams{}); err != nil {
		t.Fatalf("an empty mapping was refused: %v", err)
	}
	if err := config.ValidateMCPArtifactParams(config.MCPArtifactParams{"ingest": {"doc", "other"}}); err != nil {
		t.Fatalf("a well-formed mapping was refused: %v", err)
	}
}

// egressCfg builds a minimal valid config carrying one MCP server so the
// egress arms of Validate can be exercised in isolation.
func egressCfg(t *testing.T, srv config.MCPServerConfig) *config.Config {
	t.Helper()
	c := mustLoadValid(t)
	c.Tools.MCPServers = []config.MCPServerConfig{srv}
	return c
}

func TestValidate_MCPArtifactEgress_Refusals(t *testing.T) {
	httpSrv := func(mut func(*config.MCPServerConfig)) config.MCPServerConfig {
		s := config.MCPServerConfig{Name: "docstore", TransportMode: "streamable_http", URL: "https://docs.invalid/mcp"}
		mut(&s)
		return s
	}
	cases := []struct {
		name string
		srv  config.MCPServerConfig
		want string
	}{
		{
			"mapping without eligibility",
			httpSrv(func(s *config.MCPServerConfig) {
				s.ArtifactParams = config.MCPArtifactParams{"ingest": {"doc"}}
			}),
			"requires artifact_byte_eligible",
		},
		{
			"malformed mapping shape",
			httpSrv(func(s *config.MCPServerConfig) {
				s.ArtifactByteEligible = true
				s.ArtifactParams = config.MCPArtifactParams{"ingest": {"doc", "doc"}}
			}),
			"twice",
		},
		{
			"eligibility on an explicit stdio transport",
			config.MCPServerConfig{Name: "docstore", TransportMode: "stdio", Command: []string{"/bin/server"},
				ArtifactByteEligible: true},
			"without an http(s) url",
		},
		{
			// An `auto` transport carrying only a command auto-selects
			// stdio at connect, so it is treated as stdio here — otherwise
			// an operator would believe egress was on while the connection
			// silently never carried it.
			"eligibility on a command-only auto transport",
			config.MCPServerConfig{Name: "docstore", TransportMode: "auto", Command: []string{"/bin/server"},
				ArtifactByteEligible: true},
			"without an http(s) url",
		},
		{
			// Eligible AND mapped on stdio, so the transport rule is what
			// bites rather than the eligibility rule (which fires first for
			// a mapping-without-eligibility and is covered above).
			"mapping on a stdio transport",
			config.MCPServerConfig{Name: "docstore", TransportMode: "stdio", Command: []string{"/bin/server"},
				ArtifactByteEligible: true,
				ArtifactParams:       config.MCPArtifactParams{"ingest": {"doc"}}},
			"without an http(s) url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := egressCfg(t, tc.srv).Validate()
			if err == nil {
				t.Fatalf("an invalid egress declaration passed boot validation")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to explain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "tools.mcp_servers[0]") {
				t.Errorf("err = %q, want it to name the offending server index", err)
			}
		})
	}
}

func TestValidate_MCPArtifactEgress_ValidDeclarationPasses(t *testing.T) {
	c := egressCfg(t, config.MCPServerConfig{
		Name: "docstore", TransportMode: "streamable_http", URL: "https://docs.invalid/mcp",
		ArtifactByteEligible: true,
		ArtifactParams:       config.MCPArtifactParams{"ingest": {"doc"}},
	})
	if err := c.Validate(); err != nil {
		t.Fatalf("a valid egress declaration was refused: %v", err)
	}
}

// TestValidate_MCPArtifactEgressCeiling — a NEGATIVE ceiling is an
// operator mistake, not a synonym for "unbounded", so it is refused
// loud; zero takes the documented default.
func TestValidate_MCPArtifactEgressCeiling(t *testing.T) {
	c := mustLoadValid(t)
	c.Tools.MCPArtifactEgressMaxBytes = -1
	err := c.Validate()
	if err == nil {
		t.Fatalf("a negative egress ceiling passed validation")
	}
	if !strings.Contains(err.Error(), "tools.mcp_artifact_egress_max_bytes") {
		t.Fatalf("err = %q, want it to name the field", err)
	}

	c.Tools.MCPArtifactEgressMaxBytes = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("an unset ceiling was refused: %v", err)
	}
	if got := c.Tools.ResolvedMCPArtifactEgressMaxBytes(); got != config.DefaultMCPArtifactEgressMaxBytes {
		t.Fatalf("unset ceiling resolved to %d, want the %d default", got, config.DefaultMCPArtifactEgressMaxBytes)
	}

	c.Tools.MCPArtifactEgressMaxBytes = 4 << 20
	if err := c.Validate(); err != nil {
		t.Fatalf("a positive ceiling was refused: %v", err)
	}
	if got := c.Tools.ResolvedMCPArtifactEgressMaxBytes(); got != 4<<20 {
		t.Fatalf("configured ceiling resolved to %d, want %d", got, 4<<20)
	}
}

// TestDefaultMCPArtifactEgressMaxBytes_IsNotDerivedFromTheHeavyThreshold
// pins the independence the key's whole rationale rests on. If a future
// change made one a multiple of the other, the "network budget, not a
// token budget" claim would quietly stop being true.
func TestDefaultMCPArtifactEgressMaxBytes_IsNotDerivedFromTheHeavyThreshold(t *testing.T) {
	if config.DefaultMCPArtifactEgressMaxBytes != 8*1024*1024 {
		t.Fatalf("the documented default moved to %d without its documentation", config.DefaultMCPArtifactEgressMaxBytes)
	}
	// It is also independent of the artifact FETCH bounds — the read
	// path's ceiling answers a different question (what a model may pull
	// back INTO its context).
	if config.DefaultMCPArtifactEgressMaxBytes == config.DefaultArtifactFetchHardMaxBytes ||
		config.DefaultMCPArtifactEgressMaxBytes == config.DefaultArtifactFetchMaxBytes {
		t.Fatalf("the egress ceiling collapsed onto a fetch bound; they bound different resources and must be tunable separately")
	}
}

package a2a_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	a2adrv "github.com/hurtener/Harbor/internal/distributed/drivers/a2a"
)

// The security validator is unexported (validatePeerURL); we exercise
// it via the driver constructor, which calls it for every peer.

func TestSecurity_HTTPSPeer_Accepted(t *testing.T) {
	tr, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{{URL: "https://agent.example", TrustTier: 3, LatencyTierMS: 10}},
		},
	})
	if err != nil {
		t.Fatalf("HTTPS peer rejected: %v", err)
	}
	_ = tr.Close(nil) //nolint:staticcheck // Close tolerates nil ctx
}

func TestSecurity_HTTPLoopback_Accepted(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8080", "localhost:9090", "[::1]:8080"} {
		t.Run(host, func(t *testing.T) {
			_, err := a2adrv.New(distributed.Dependencies{
				Tools: config.ToolsConfig{
					A2APeers: []config.A2APeerConfig{{URL: "http://" + host, TrustTier: 3, LatencyTierMS: 10}},
				},
			})
			if err != nil {
				t.Errorf("HTTP loopback %q rejected: %v", host, err)
			}
		})
	}
}

func TestSecurity_HTTPPublic_Rejected(t *testing.T) {
	_, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{{URL: "http://public.example", TrustTier: 3, LatencyTierMS: 10}},
		},
	})
	if !errors.Is(err, a2adrv.ErrInsecureScheme) {
		t.Errorf("expected ErrInsecureScheme, got %v", err)
	}
}

func TestSecurity_HTTPPublicWithInsecureLoopbackFlag_Rejected(t *testing.T) {
	// AllowInsecureLoopback is name-checked against loopback only —
	// it does NOT let an operator HTTP-talk to a public peer.
	_, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{{
				URL:                   "http://public.example",
				TrustTier:             3,
				LatencyTierMS:         10,
				AllowInsecureLoopback: true,
			}},
		},
	})
	if !errors.Is(err, a2adrv.ErrInsecureScheme) {
		t.Errorf("expected ErrInsecureScheme even with AllowInsecureLoopback, got %v", err)
	}
}

func TestSecurity_NonStandardScheme_Rejected(t *testing.T) {
	_, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{{URL: "ftp://agent.example", TrustTier: 3, LatencyTierMS: 10}},
		},
	})
	if !errors.Is(err, a2adrv.ErrInsecureScheme) {
		t.Errorf("expected ErrInsecureScheme for ftp scheme, got %v", err)
	}
}

func TestSecurity_MalformedURL_Rejected(t *testing.T) {
	_, err := a2adrv.New(distributed.Dependencies{
		Tools: config.ToolsConfig{
			A2APeers: []config.A2APeerConfig{{URL: "not-a-url", TrustTier: 3, LatencyTierMS: 10}},
		},
	})
	if err == nil {
		t.Errorf("expected error for malformed URL")
	}
}

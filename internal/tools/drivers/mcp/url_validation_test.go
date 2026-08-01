package mcp

import (
	"errors"
	"testing"
)

func TestConfigValidate_HTTPURLUsesStrictSharedBoundary(t *testing.T) {
	for _, raw := range []string{
		"ftp://mcp.example.test/path",
		"https://user:pass@mcp.example.test/path",
		"https://mcp.example.test/path#fragment",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := New(Config{Name: "bad-url", URL: raw, TransportMode: TransportStreamableHTTP,
				Bus: newTestBus(t), DefaultIdentity: defaultIdentity()})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New URL %q = %v, want ErrInvalidConfig", raw, err)
			}
		})
	}
}

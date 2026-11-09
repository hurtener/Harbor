package mcp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSafeSDKLogger_SuppressesEndpointFromMessageAndAttributes(t *testing.T) {
	var out bytes.Buffer
	logger := safeSDKLogger(slog.New(slog.NewTextHandler(&out, nil)))
	logger.With("url", "https://mcp.example.test/rpc?token=private").Warn(
		"calling tools/list: POST https://mcp.example.test/rpc?token=private failed",
		"error", "Authorization: Bearer private")
	logged := out.String()
	if !strings.Contains(logged, "mcp SDK transport event") || strings.Contains(logged, "private") || strings.Contains(logged, "https://") {
		t.Fatalf("unsafe SDK log: %q", logged)
	}
}

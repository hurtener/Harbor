package mcpconsole_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// probeFailureProvider preserves the real registry/accessor projection while
// injecting the exact typed error a downstream Discover failure carries.
type probeFailureProvider struct {
	*stubProvider
	failure error
}

func (p *probeFailureProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return nil, p.failure
}

func TestRegistryAccessor_Probe_DownstreamSafeSummary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"unauthorized", fmt.Errorf("%w: %w", mcp.ErrTransportFailed, &mcp.DownstreamUnauthorizedError{StatusCode: http.StatusUnauthorized}), "downstream MCP server rejected its credential"},
		{"forbidden", fmt.Errorf("%w: %w", mcp.ErrTransportFailed, &mcp.DownstreamUnauthorizedError{StatusCode: http.StatusForbidden}), "downstream MCP server rejected its credential"},
		{"transport", mcp.ErrTransportFailed, "downstream MCP transport failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const privateEndpoint = "https://receiver.invalid/mcp?api_key=probe-query-secret"
			provider := &probeFailureProvider{
				stubProvider: &stubProvider{id: "probe-server"},
				failure:      fmt.Errorf("POST %s: body=private-response Authorization: Bearer private-bearer: %w", privateEndpoint, tc.failure),
			}
			registry := mcp.NewRegistry()
			if err := registry.Register(context.Background(), mcp.ServerRegistration{Provider: provider, Transport: "streamable-http"}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			accessor, err := mcpconsole.NewRegistryAccessor(registry)
			if err != nil {
				t.Fatalf("NewRegistryAccessor: %v", err)
			}
			row, err := accessor.Probe(idCtx(t), "probe-server")
			if err != nil {
				t.Fatalf("failed probe must remain a populated result: %v", err)
			}
			if row.OK || row.Error != tc.want {
				t.Fatalf("probe result = %+v, want failed result with %q", row, tc.want)
			}
			for _, unsafe := range []string{"probe-query-secret", "private-response", "private-bearer", "receiver.invalid"} {
				if strings.Contains(row.Error, unsafe) {
					t.Fatalf("probe leaked %q: %+v", unsafe, row)
				}
			}
		})
	}
}

package mcpconsole

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

func TestMarkNotFound_DownstreamTypedClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"unauthorized", fmt.Errorf("%w: %w", mcp.ErrTransportFailed, mcp.ErrDownstreamUnauthorized), protocol.ErrAccessorDownstreamUnauthorized},
		{"transport", mcp.ErrTransportFailed, protocol.ErrAccessorDownstreamTransportFailed},
		{"identity before transport", fmt.Errorf("%w: %w", mcp.ErrTransportFailed, mcp.ErrIdentityMissing), protocol.ErrAccessorIdentityRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("https://private.invalid/mcp?token=fixture-secret body: identity missing from ctx server not found: %w", tc.err)
			got := markNotFound(wrapped)
			if !errors.Is(got, tc.want) || !errors.Is(got, tc.err) {
				t.Fatalf("translation lost typed cause: %v", got)
			}
			if errors.Is(got, protocol.ErrAccessorNotFound) {
				t.Fatalf("downstream wording forged not-found: %v", got)
			}
			if tc.want == protocol.ErrAccessorDownstreamUnauthorized && errors.Is(got, protocol.ErrAccessorDownstreamTransportFailed) {
				t.Fatalf("unauthorized lost precedence: %v", got)
			}
			if tc.want == protocol.ErrAccessorIdentityRequired && errors.Is(got, protocol.ErrAccessorDownstreamTransportFailed) {
				t.Fatalf("identity lost precedence: %v", got)
			}
		})
	}
	plain := errors.New("downstream_mcp_unauthorized transport failed")
	if got := markNotFound(plain); got != plain {
		t.Fatalf("text forged typed classification: %v", got)
	}
}

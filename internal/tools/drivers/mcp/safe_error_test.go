package mcp

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestSafeDownstreamCause_HidesEndpointAndPreservesTypedCause(t *testing.T) {
	upstream := fmt.Errorf("GET https://mcp.example.test/rpc?token=private: %w", ErrIdentityMissing)
	wrapped := fmt.Errorf("%w: connect: %w", ErrTransportFailed, safeDownstreamCause(upstream))
	if !errors.Is(wrapped, ErrIdentityMissing) || !errors.Is(wrapped, ErrTransportFailed) {
		t.Fatalf("typed cause lost: %v", wrapped)
	}
	if strings.Contains(wrapped.Error(), "private") || strings.Contains(wrapped.Error(), "https://") {
		t.Fatalf("endpoint leaked: %v", wrapped)
	}
}

func TestAmbiguousToolCall_IsPermanentWithoutLeakingEndpoint(t *testing.T) {
	upstream := fmt.Errorf("POST https://mcp.example.test/rpc?token=private: %w", mcpsdk.ErrSessionMissing)
	err := fmt.Errorf("%w: %w", ErrTransportFailed, ambiguousToolCall{cause: upstream})
	if !errors.Is(err, mcpsdk.ErrSessionMissing) || !errors.Is(err, ErrTransportFailed) {
		t.Fatalf("typed causes lost: %v", err)
	}
	if tools.ClassifyError(err, false) != tools.ErrClassPermanent || strings.Contains(err.Error(), "private") {
		t.Fatalf("ambiguous call was retryable or exposed endpoint: %v", err)
	}
}

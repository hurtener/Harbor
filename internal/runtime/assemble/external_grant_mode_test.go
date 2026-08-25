package assemble

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
)

func TestWireExternalGrant_ExplicitDisabledConflictsWithInjectedEnabledMode(t *testing.T) {
	_, _, err := wireExternalGrant(
		context.Background(),
		config.LLMExternalGrantConfig{Mode: "disabled"},
		llm.ExternalGrantConfig{Mode: llm.ExternalGrantRequired},
		nil,
		nil,
		nil,
		0,
		0,
	)
	if err == nil || !strings.Contains(err.Error(), "conflicts with configured mode") {
		t.Fatalf("wireExternalGrant error=%v, want explicit mode conflict", err)
	}
}

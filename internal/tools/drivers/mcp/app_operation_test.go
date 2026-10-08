package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/appoperation"
)

func TestResolveBearerCtx_AppOperationRefusesUnacknowledgedProviders(t *testing.T) {
	ctx, err := appoperation.WithRead(context.Background(), strings.Repeat("a", 64), "agent", "source", "ui://report")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Provider{
		{source: "source"},
		{source: "source", cfg: Config{OAuthProvider: &stubOAuthProvider{token: "broad-legacy-token"}}},
	} {
		if _, err := p.resolveBearerCtx(ctx, "ui://report"); !errors.Is(err, appoperation.ErrInvalid) {
			t.Fatalf("missing App acknowledgement accepted: %v", err)
		}
	}
}

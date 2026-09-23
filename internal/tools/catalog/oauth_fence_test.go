package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

type fenceOAuthProvider struct {
	stubOAuthProvider
	onToken func()
	calls   int
}

func (p *fenceOAuthProvider) Token(ctx context.Context, source tools.ToolSourceID) (auth.Token, error) {
	p.calls++
	if p.onToken != nil {
		p.onToken()
	}
	return p.stubOAuthProvider.Token(ctx, source)
}

func TestOAuthWrapper_InvocationFence(t *testing.T) {
	for _, phase := range []string{"before_token", "during_token", "cancel_during_token", "provider_error", "current"} {
		t.Run(phase, func(t *testing.T) {
			invalidated := make(chan struct{})
			ctx, cancel := context.WithCancel(tools.WithInvocationFence(ctxWithID(t, catalogTestID), invalidated))
			defer cancel()
			prov := &fenceOAuthProvider{}
			wantErr := tools.ErrInvocationSuperseded
			wantTokens, wantInvocations := 1, 0
			switch phase {
			case "before_token":
				close(invalidated)
				wantTokens = 0
			case "during_token":
				prov.onToken = func() { close(invalidated) }
			case "cancel_during_token":
				prov.onToken = cancel
				wantErr = context.Canceled
			case "provider_error":
				prov.onToken = func() { close(invalidated) }
				wantErr = errors.New("credential persistence failed")
				prov.tokenErr = wantErr
			case "current":
				wantErr, wantInvocations = nil, 1
			}
			invocations := 0
			d := catalog.WrapWithOAuth(tools.ToolDescriptor{
				Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
					invocations++
					return tools.ToolResult{Value: "done"}, nil
				},
			}, prov, catalog.OAuthWrapperOptions{})
			_, err := d.Invoke(ctx, json.RawMessage(`{}`))
			if !errors.Is(err, wantErr) || prov.calls != wantTokens || invocations != wantInvocations {
				t.Fatalf("err=%v tokens=%d invocations=%d; want %v/%d/%d", err, prov.calls, invocations, wantErr, wantTokens, wantInvocations)
			}
		})
	}
}

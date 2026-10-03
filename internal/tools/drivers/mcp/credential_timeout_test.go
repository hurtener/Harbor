package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

type deadlineCredentialProvider struct {
	identityCredProvider
	deadlines []time.Time
	wait      bool
	late      bool
}

func (p *deadlineCredentialProvider) Token(ctx context.Context, source tools.ToolSourceID) (auth.Token, error) {
	deadline, _ := ctx.Deadline()
	p.deadlines = append(p.deadlines, deadline)
	if p.wait {
		<-ctx.Done()
		if p.late {
			return p.identityCredProvider.Token(ctx, source)
		}
		return auth.Token{}, ctx.Err()
	}
	return p.identityCredProvider.Token(ctx, source)
}

func TestMCPToolCredentialAdmission_UsesPolicyDeadlineAndPreservesTransport(t *testing.T) {
	for _, tc := range []struct {
		name          string
		defaultPolicy tools.ToolPolicy
		toolPolicy    tools.ToolPolicy
		parentBudget  time.Duration
		wantBudget    time.Duration
		callerShorter bool
		meta          bool
	}{
		{name: "package default", parentBudget: 90 * time.Second, wantBudget: 30 * time.Second},
		{name: "per-tool override", defaultPolicy: fastPolicy(0), toolPolicy: tools.ToolPolicy{TimeoutMS: 40, RetryOn: []tools.ErrorClass{}}, parentBudget: 2 * time.Second, wantBudget: 40 * time.Millisecond, meta: true},
		{name: "caller shorter", defaultPolicy: fastPolicy(0), toolPolicy: tools.ToolPolicy{TimeoutMS: 2000, RetryOn: []tools.ErrorClass{}}, parentBudget: 500 * time.Millisecond, callerShorter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var overrides map[string]tools.ToolPolicy
			if tc.toolPolicy.TimeoutMS != 0 {
				overrides = map[string]tools.ToolPolicy{"echo": tc.toolPolicy}
			}
			p, server, cleanup := newPolicyTestProvider(t, tc.defaultPolicy, overrides)
			defer cleanup()
			credential := &deadlineCredentialProvider{}
			p.cfg.OAuthProvider = credential
			p.cfg.Injection = &CredentialInjection{Provider: credential, Form: InjectionFormHeader, Header: "x-vendor-api-key"}
			if tc.meta {
				p.cfg.Injection = &CredentialInjection{Provider: credential, Form: InjectionFormMeta, MetaKey: []string{"vendor", "api_key"}}
			}
			desc := resolveTool(t, p, "mock_echo")
			boundarySeen := false
			desc = tools.DecorateInvocation(desc, func(next tools.Invocation) tools.Invocation {
				return func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
					boundarySeen = true
					if got := bearerFrom(ctx); got != "cred-test-tenant-test-user" {
						t.Errorf("boundary bearer = %q", got)
					}
					if !tc.meta && injectedHeadersFrom(ctx)["x-vendor-api-key"] != "cred-test-tenant-test-user" {
						t.Errorf("boundary injection header = %v", injectedHeadersFrom(ctx))
					}
					return next(ctx, args)
				}
			})
			ctx, cancel := context.WithTimeout(mustIdentity(t), tc.parentBudget)
			defer cancel()
			parentDeadline, _ := ctx.Deadline()
			started := time.Now()
			if _, err := desc.Invoke(ctx, json.RawMessage(`{"text":"ok"}`)); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !boundarySeen {
				t.Fatal("successful credential admission did not reach invocation boundary")
			}
			if len(credential.deadlines) != 2 {
				t.Fatalf("credential calls = %d, want bearer and injection", len(credential.deadlines))
			}
			for _, deadline := range credential.deadlines {
				if deadline.IsZero() {
					t.Fatal("credential provider did not receive a deadline")
				}
				if tc.callerShorter {
					if !deadline.Equal(parentDeadline) {
						t.Fatalf("credential deadline %s, want caller deadline %s", deadline, parentDeadline)
					}
				} else if got := deadline.Sub(started); got <= 0 || got > tc.wantBudget+20*time.Millisecond {
					t.Fatalf("credential deadline after %s, want at most %s", got, tc.wantBudget+20*time.Millisecond)
				}
			}
			if tc.meta {
				vendor, ok := server.metaFor("echo")["vendor"].(map[string]any)
				if !ok || vendor["api_key"] != "cred-test-tenant-test-user" {
					t.Fatalf("credential missing from MCP transport meta: %v", server.metaFor("echo"))
				}
			}
		})
	}
}

func TestMCPToolCredentialAdmission_TimeoutPreventsTransport(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "provider error"
		if late {
			name = "late token"
		}
		t.Run(name, func(t *testing.T) {
			p, server, cleanup := newPolicyTestProvider(t, fastPolicy(0), map[string]tools.ToolPolicy{
				"flaky": {TimeoutMS: 20, RetryOn: []tools.ErrorClass{}},
			})
			defer cleanup()
			credential := &deadlineCredentialProvider{wait: true, late: late}
			p.cfg.Injection = &CredentialInjection{Provider: credential, Form: InjectionFormMeta, MetaKey: []string{"vendor", "api_key"}}
			desc := resolveTool(t, p, "mock_flaky")
			boundarySeen := false
			desc = tools.DecorateInvocation(desc, func(next tools.Invocation) tools.Invocation {
				return func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
					boundarySeen = true
					return next(ctx, args)
				}
			})
			ctx, cancel := context.WithTimeout(mustIdentity(t), 500*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := desc.Invoke(ctx, json.RawMessage(`{}`))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Invoke error = %v, want deadline exceeded", err)
			}
			if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
				t.Fatalf("credential admission lasted %s, want policy timeout well before caller timeout", elapsed)
			}
			if server.metaFor("flaky") != nil || server.flakyAttempts.Load() != 0 {
				t.Fatal("timed-out credential reached MCP transport")
			}
			if boundarySeen {
				t.Fatal("timed-out credential entered invocation boundary")
			}
		})
	}
}

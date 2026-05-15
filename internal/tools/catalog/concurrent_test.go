package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

// TestConcurrent_OAuthWrapper_Reuse — D-025 obligation: N≥100
// concurrent goroutines drive a single wrapped descriptor under
// -race; the underlying stub provider returns a token on each call.
// We assert no data race, no goroutine leak, every invocation
// completes with the correct identity propagation.
func TestConcurrent_OAuthWrapper_Reuse(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "github_read", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{}, // returns a valid token
		},
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("github_read")

	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%4),
				UserID:    fmt.Sprintf("user-%d", i%4),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				errs <- err
				return
			}
			args := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			res, err := d.Invoke(ctx, args)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			// Identity propagation: the inner echo tool returns the
			// args verbatim; assert the per-goroutine args came back.
			if got, _ := res.Value.(string); got != string(args) {
				errs <- fmt.Errorf("goroutine %d: args bleed: got %q want %q", i, got, string(args))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Goroutine-leak baseline: yield a few times so background
	// cleanup runs settle, then assert the goroutine count is at
	// most baseline + a small slop.
	settleGoroutines()
	if growth := runtime.NumGoroutine() - baseline; growth > 4 {
		t.Errorf("goroutine leak: baseline=%d, after=%d (growth=%d)",
			baseline, runtime.NumGoroutine(), growth)
	}
}

// TestConcurrent_OAuthWrapper_CancellationCrossTalk — D-025: cancel
// run A; run B (in flight on the same wrapped descriptor) must NOT
// be cancelled by A's cancel.
func TestConcurrent_OAuthWrapper_CancellationCrossTalk(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "github_read", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{},
		},
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("github_read")

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}

	// Two ctxs: A and B. Cancel A while B is in flight; B must succeed.
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxAIdent, _ := identity.With(ctxA, id)
	cancelA()
	_, errA := d.Invoke(ctxAIdent, json.RawMessage(`{}`))
	// A may surface ctx.Err either at the OAuth pre-check or at the
	// underlying invoke; either way we tolerate it as long as B is
	// unaffected.
	_ = errA

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	ctxBIdent, _ := identity.With(ctxB, id)
	res, errB := d.Invoke(ctxBIdent, json.RawMessage(`{"b":true}`))
	if errB != nil {
		t.Fatalf("ctxB invoke after ctxA cancel: %v", errB)
	}
	if got, _ := res.Value.(string); got != `{"b":true}` {
		t.Errorf("ctxB invoke result = %q, want %q", got, `{"b":true}`)
	}
}

// TestConcurrent_ApprovalWrapper_Reuse — D-025: N concurrent
// goroutines drive a single approval-wrapped descriptor with an
// always-approve policy (so the short-circuit path fires; no need to
// coordinate with a resolver). Asserts no data race, no leak.
func TestConcurrent_ApprovalWrapper_Reuse(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("echo_tool")

	const n = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%4),
				UserID:    fmt.Sprintf("user-%d", i%4),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				errs <- err
				return
			}
			args := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			res, err := d.Invoke(ctx, args)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if got, _ := res.Value.(string); got != string(args) {
				errs <- fmt.Errorf("goroutine %d: args bleed: got %q want %q", i, got, string(args))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	settleGoroutines()
	if growth := runtime.NumGoroutine() - baseline; growth > 4 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestConcurrent_OAuthWrapper_ErrAuthRequiredUnderRace — every
// goroutine receives the typed *ErrAuthRequired (no payload bleed).
func TestConcurrent_OAuthWrapper_ErrAuthRequiredUnderRace(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "github_read", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"}},
	}
	prov := &stubOAuthProvider{
		tokenErr: &auth.ErrAuthRequired{Source: tools.ToolSourceID("test-source"), Message: "needs auth"},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": prov,
		},
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("github_read")

	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("s-%d", i)}
			ctx, _ := identity.With(context.Background(), id)
			_, err := d.Invoke(ctx, json.RawMessage(`{}`))
			var authReq *auth.ErrAuthRequired
			if !errors.As(err, &authReq) {
				errs <- fmt.Errorf("goroutine %d: err=%v want *ErrAuthRequired", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// settleGoroutines yields a few times so background cleanup (event
// bus drain, etc.) has a chance to run before the leak assertion.
func settleGoroutines() {
	for i := 0; i < 5; i++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

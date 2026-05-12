// Package conformancetest is Harbor's shared conformance suite for
// any ToolCatalog implementation. Drivers (Phase 27 HTTP, Phase 28
// MCP, Phase 29 A2A) consume this suite verbatim against their own
// CatalogFactory so the contract surface is uniform regardless of
// transport.
//
// Phase 26 ships the suite; the in-process driver (inproc) is the
// first consumer.
package conformancetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// CatalogFactory produces a fresh, empty ToolCatalog.
type CatalogFactory func() tools.ToolCatalog

// Run runs the full conformance suite against newCatalog.
func Run(t *testing.T, newCatalog CatalogFactory) {
	t.Helper()
	t.Run("Catalog_Register_Resolve_List", func(t *testing.T) {
		testRegisterResolveList(t, newCatalog)
	})
	t.Run("Catalog_Register_DuplicateName_Rejects", func(t *testing.T) {
		testDuplicateName(t, newCatalog)
	})
	t.Run("Catalog_Filter_AuthScopes_VisibilityMath", func(t *testing.T) {
		testFilterAuthScopes(t, newCatalog)
	})
	t.Run("Catalog_Filter_LoadingMode_Defaults", func(t *testing.T) {
		testFilterLoadingMode(t, newCatalog)
	})
	t.Run("Catalog_Filter_NameRegex", func(t *testing.T) {
		testFilterNameRegex(t, newCatalog)
	})
	t.Run("Tool_InvalidArgs_ReturnsTypedError", func(t *testing.T) {
		testInvalidArgs(t, newCatalog)
	})
	t.Run("Tool_PolicyDefaults_Retry_BackoffFires", func(t *testing.T) {
		testPolicyDefaultsRetry(t, newCatalog)
	})
	t.Run("Tool_PolicyOverride_TimeoutWins", func(t *testing.T) {
		testPolicyOverride(t, newCatalog)
	})
	t.Run("Tool_Cancellation_PropagatesViaCtx", func(t *testing.T) {
		testCancellation(t, newCatalog)
	})
	t.Run("Tool_Concurrent_Reuse_NoRace", func(t *testing.T) {
		testConcurrentReuse(t, newCatalog)
	})
	t.Run("Tool_Identity_PropagatesThroughInvoke", func(t *testing.T) {
		testIdentityPropagates(t, newCatalog)
	})
}

type echoArgs struct {
	Text string `json:"text"`
}

type echoOut struct {
	Echo string `json:"echo"`
}

func registerEcho(t *testing.T, cat tools.ToolCatalog, name string, opts ...tools.DescriptorOption) {
	t.Helper()
	err := inproc.RegisterFunc[echoArgs, echoOut](cat, name, func(ctx context.Context, in echoArgs) (echoOut, error) {
		return echoOut{Echo: in.Text}, nil
	}, opts...)
	if err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
}

func testRegisterResolveList(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	registerEcho(t, cat, "echo")
	d, ok := cat.Resolve("echo")
	if !ok {
		t.Fatalf("Resolve(echo): not found")
	}
	if d.Tool.Name != "echo" {
		t.Fatalf("expected Name=echo, got %q", d.Tool.Name)
	}
	if d.Tool.Transport != tools.TransportInProcess {
		t.Fatalf("expected Transport=inprocess, got %q", d.Tool.Transport)
	}
	if string(d.Tool.ArgsSchema) == "" {
		t.Fatalf("expected derived ArgsSchema, got empty")
	}
	if string(d.Tool.OutSchema) == "" {
		t.Fatalf("expected derived OutSchema, got empty")
	}
	list := cat.List(tools.CatalogFilter{})
	if len(list) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(list))
	}
	if list[0].Name != "echo" {
		t.Fatalf("expected list[0].Name=echo, got %q", list[0].Name)
	}
}

func testDuplicateName(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	registerEcho(t, cat, "echo")
	err := inproc.RegisterFunc[echoArgs, echoOut](cat, "echo", func(ctx context.Context, in echoArgs) (echoOut, error) {
		return echoOut{}, nil
	})
	if err == nil {
		t.Fatalf("expected ErrToolDuplicateName, got nil")
	}
	if !errors.Is(err, tools.ErrToolDuplicateName) {
		t.Fatalf("expected ErrToolDuplicateName, got: %v", err)
	}
}

func testFilterAuthScopes(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	registerEcho(t, cat, "public")
	registerEcho(t, cat, "gated",
		tools.WithAuthScopes("weather:read", "weather:write"))

	list := cat.List(tools.CatalogFilter{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
	})
	if len(list) != 1 || list[0].Name != "public" {
		t.Fatalf("expected only 'public' visible with no scopes, got %v", names(list))
	}

	list = cat.List(tools.CatalogFilter{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
		GrantedScopes: []string{"weather:read"},
	})
	if len(list) != 1 || list[0].Name != "public" {
		t.Fatalf("expected only 'public' visible with partial scopes, got %v", names(list))
	}

	list = cat.List(tools.CatalogFilter{
		TenantID: "t1", UserID: "u1", SessionID: "s1",
		GrantedScopes: []string{"weather:read", "weather:write"},
	})
	if len(list) != 2 {
		t.Fatalf("expected both visible with full scopes, got %v", names(list))
	}
}

func testFilterLoadingMode(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	registerEcho(t, cat, "always_tool", tools.WithLoading(tools.LoadingAlways))
	registerEcho(t, cat, "deferred_tool", tools.WithLoading(tools.LoadingDeferred))

	list := cat.List(tools.CatalogFilter{})
	if len(list) != 1 || list[0].Name != "always_tool" {
		t.Fatalf("expected only 'always_tool' under default filter, got %v", names(list))
	}

	list = cat.List(tools.CatalogFilter{
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	})
	if len(list) != 2 {
		t.Fatalf("expected 2 tools under full LoadingModes, got %v", names(list))
	}

	list = cat.List(tools.CatalogFilter{
		LoadingModes: []tools.LoadingMode{tools.LoadingDeferred},
	})
	if len(list) != 1 || list[0].Name != "deferred_tool" {
		t.Fatalf("expected only 'deferred_tool' under deferred filter, got %v", names(list))
	}
}

func testFilterNameRegex(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	registerEcho(t, cat, "weather.lookup")
	registerEcho(t, cat, "weather.forecast")
	registerEcho(t, cat, "ticker.quote")

	rgx := regexp.MustCompile("^weather\\.")
	list := cat.List(tools.CatalogFilter{NameRegex: rgx})
	if len(list) != 2 {
		t.Fatalf("expected 2 tools matching ^weather\\., got %v", names(list))
	}
}

func testInvalidArgs(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	registerEcho(t, cat, "echo")
	d, _ := cat.Resolve("echo")
	ctx := mustIdentityCtx(t)
	_, err := d.Invoke(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected ErrToolInvalidArgs, got nil")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got: %v", err)
	}
}

type flakyTool struct {
	attempts   atomic.Int64
	errorUntil int64
}

type flakyArgs struct {
	N int `json:"n"`
}

type flakyOut struct {
	Attempts int64 `json:"attempts"`
}

func (f *flakyTool) Run(ctx context.Context, in flakyArgs) (flakyOut, error) {
	n := f.attempts.Add(1)
	if n <= f.errorUntil {
		return flakyOut{}, fmt.Errorf("transient: attempt %d (target=%d)", n, in.N)
	}
	return flakyOut{Attempts: n}, nil
}

func testPolicyDefaultsRetry(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	tool := &flakyTool{errorUntil: 2}
	err := inproc.RegisterFunc[flakyArgs, flakyOut](cat, "flaky", tool.Run,
		tools.WithPolicy(tools.ToolPolicy{
			MaxRetries:  3,
			BackoffBase: 5 * time.Millisecond,
			BackoffMax:  50 * time.Millisecond,
			BackoffMult: 2,
			TimeoutMS:   1000,
			RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
			Validate:    tools.ValidateBoth,
		}),
	)
	if err != nil {
		t.Fatalf("register with policy: %v", err)
	}
	d, _ := cat.Resolve("flaky")
	ctx := mustIdentityCtx(t)

	start := time.Now()
	result, err := d.Invoke(ctx, []byte(`{"n":1}`))
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, ok := result.Value.(flakyOut)
	if !ok {
		t.Fatalf("expected flakyOut, got %T", result.Value)
	}
	if out.Attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", out.Attempts)
	}
	if dur < 5*time.Millisecond {
		t.Fatalf("expected backoff > 5ms, got %v", dur)
	}
}

func testPolicyOverride(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	err := inproc.RegisterFunc[flakyArgs, flakyOut](cat, "slow", func(ctx context.Context, in flakyArgs) (flakyOut, error) {
		select {
		case <-ctx.Done():
			return flakyOut{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return flakyOut{Attempts: 1}, nil
		}
	}, tools.WithPolicy(tools.ToolPolicy{
		TimeoutMS:   50,
		MaxRetries:  2,
		BackoffBase: 1 * time.Millisecond,
		BackoffMax:  10 * time.Millisecond,
		BackoffMult: 2,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTimeout},
		Validate:    tools.ValidateBoth,
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("slow")
	ctx, cancel := context.WithTimeout(mustIdentityCtx(t), 5*time.Second)
	defer cancel()
	_, err = d.Invoke(ctx, []byte(`{"n":1}`))
	if err == nil {
		t.Fatalf("expected timeout-then-exhaustion error, got nil")
	}
	if !errors.Is(err, tools.ErrToolPolicyExhausted) {
		t.Fatalf("expected ErrToolPolicyExhausted, got: %v", err)
	}
}

func testCancellation(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	err := inproc.RegisterFunc[flakyArgs, flakyOut](cat, "blocking", func(ctx context.Context, in flakyArgs) (flakyOut, error) {
		<-ctx.Done()
		return flakyOut{}, ctx.Err()
	}, tools.WithPolicy(tools.ToolPolicy{
		TimeoutMS:   10000,
		MaxRetries:  0,
		BackoffBase: 1 * time.Millisecond,
		BackoffMult: 2,
		BackoffMax:  10 * time.Millisecond,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
		Validate:    tools.ValidateBoth,
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("blocking")
	// Pre-cancel ctx — the blocking tool's <-ctx.Done() returns
	// immediately and the policy shell propagates ctx.Err(). AGENTS.md
	// §11 forbids time.Sleep for synchronisation; pre-cancel is the
	// equivalent surface assertion (cancel ⇒ context.Canceled out).
	ctx, cancel := context.WithCancel(mustIdentityCtx(t))
	cancel()
	_, err = d.Invoke(ctx, []byte(`{"n":1}`))
	if err == nil {
		t.Fatalf("expected ctx.Err(), got nil")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context cancel, got: %v", err)
	}
}

func testConcurrentReuse(t *testing.T, newCatalog CatalogFactory) {
	const n = 100
	cat := newCatalog()
	var counter atomic.Int64
	err := inproc.RegisterFunc[flakyArgs, flakyOut](cat, "concurrent", func(ctx context.Context, in flakyArgs) (flakyOut, error) {
		c := counter.Add(1)
		if c%3 == 0 {
			return flakyOut{}, fmt.Errorf("transient: simulated")
		}
		return flakyOut{Attempts: int64(in.N)}, nil
	}, tools.WithPolicy(tools.ToolPolicy{
		MaxRetries:  5,
		BackoffBase: 1 * time.Millisecond,
		BackoffMult: 2,
		BackoffMax:  10 * time.Millisecond,
		TimeoutMS:   1000,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
		Validate:    tools.ValidateBoth,
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("concurrent")

	baseline := runtime.NumGoroutine()

	type result struct {
		err error
		out int64
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i%8)
			id := identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("u-%d", i%8), SessionID: fmt.Sprintf("s-%d", i%8)}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			args, _ := json.Marshal(flakyArgs{N: i})
			res, err := d.Invoke(ctx, args)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			out, _ := res.Value.(flakyOut)
			results[i] = result{out: out.Attempts}
		}()
	}
	wg.Wait()

	failures := 0
	for i, r := range results {
		if r.err != nil {
			failures++
			t.Logf("invocation %d failed: %v", i, r.err)
			continue
		}
		if r.out != int64(i) {
			t.Errorf("invocation %d: expected N=%d, got %d (context bleed?)", i, i, r.out)
		}
	}
	if failures > 0 {
		t.Errorf("%d concurrent invocations failed (D-025 retry budget should cover all)", failures)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

type identityProbeOut struct {
	Tenant  string `json:"tenant"`
	User    string `json:"user"`
	Session string `json:"session"`
}

func testIdentityPropagates(t *testing.T, newCatalog CatalogFactory) {
	cat := newCatalog()
	err := inproc.RegisterFunc[flakyArgs, identityProbeOut](cat, "id_probe", func(ctx context.Context, in flakyArgs) (identityProbeOut, error) {
		id, ok := identity.From(ctx)
		if !ok {
			return identityProbeOut{}, fmt.Errorf("no identity in ctx")
		}
		return identityProbeOut{
			Tenant:  id.TenantID,
			User:    id.UserID,
			Session: id.SessionID,
		}, nil
	}, tools.WithPolicy(tools.ToolPolicy{
		MaxRetries:  0,
		BackoffBase: 1 * time.Millisecond,
		BackoffMult: 2,
		BackoffMax:  10 * time.Millisecond,
		TimeoutMS:   1000,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
		Validate:    tools.ValidateBoth,
	}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("id_probe")
	for _, tenant := range []string{"alpha", "beta", "gamma"} {
		id := identity.Identity{TenantID: tenant, UserID: "u1", SessionID: "s1"}
		ctx, err := identity.With(context.Background(), id)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		res, err := d.Invoke(ctx, []byte(`{"n":0}`))
		if err != nil {
			t.Fatalf("invoke for tenant %q: %v", tenant, err)
		}
		out, _ := res.Value.(identityProbeOut)
		if out.Tenant != tenant {
			t.Errorf("expected tenant %q, got %q", tenant, out.Tenant)
		}
	}
}

func names(ts []tools.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func mustIdentityCtx(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "test-tenant", UserID: "test-user", SessionID: "test-session"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

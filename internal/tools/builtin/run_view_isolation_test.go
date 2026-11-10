package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/searchcache"
)

// run_view_isolation_test.go — SECURITY regressions: tool_search, tool_get
// and declarative_action answer strictly from the calling run's view. The
// search index and the catalog are process-wide; another principal's tools
// must never be returned, described, crowd out results, or be dispatched.

// ownerView is a run view admitting only the sources one principal owns —
// the shape of the production owner-scoped projection (owner admission over
// a PlannerView), kept generic for this package.
type ownerView struct {
	base  tools.PlannerView
	owned map[tools.ToolSourceID]bool
}

func (v ownerView) admit(t tools.Tool) bool { return t.Source == "" || v.owned[t.Source] }

func (v ownerView) Resolve(name string) (tools.Tool, bool) {
	t, ok := v.base.Resolve(name)
	if !ok || !v.admit(t) {
		return tools.Tool{}, false
	}
	return t, true
}

func (v ownerView) List() []tools.Tool { return v.filter(v.base.List()) }

func (v ownerView) DiscoveryTools() ([]tools.Tool, bool) {
	all, complete := v.base.DiscoveryTools()
	return v.filter(all), complete
}

func (v ownerView) filter(in []tools.Tool) []tools.Tool {
	out := make([]tools.Tool, 0, len(in))
	for _, t := range in {
		if v.admit(t) {
			out = append(out, t)
		}
	}
	return out
}

type principal struct {
	id     identity.Identity
	source tools.ToolSourceID
}

// sharedServerCatalog attaches the SAME server (identical tool names and
// descriptions) once per principal under a per-owner physical source, plus a
// crowd of other tenants' copies, all into one catalog backed by one real
// FTS5 index. Every principal's copy is deferred so discovery is the only
// path to it.
func sharedServerCatalog(t *testing.T, principals []principal, crowd int, invoked *sync.Map) tools.ToolCatalog {
	t.Helper()
	sc, err := searchcache.New(searchcache.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("searchcache.New: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	cat := tools.NewCatalog(tools.WithSearchCache(sc))
	if err := Register(cat, []string{"tool_search", "tool_get", "declarative_action"}); err != nil {
		t.Fatalf("Register meta-tools: %v", err)
	}
	sources := make([]tools.ToolSourceID, 0, len(principals)+crowd)
	for _, p := range principals {
		sources = append(sources, p.source)
	}
	for i := range crowd {
		sources = append(sources, tools.ToolSourceID(fmt.Sprintf("mailsrv~a-crowd%03d", i)))
	}
	for _, src := range sources {
		var descs []tools.ToolDescriptor
		for _, op := range []string{"send_message", "list_messages"} {
			name := string(src) + "_" + op
			descs = append(descs, tools.ToolDescriptor{
				Tool: tools.Tool{
					Name:        name,
					Description: "Mailbox operation: " + strings.ReplaceAll(op, "_", " "),
					ArgsSchema:  json.RawMessage(`{"type":"object"}`),
					Source:      src,
					Transport:   tools.TransportMCP,
					Loading:     tools.LoadingDeferred,
					SourceTitle: "Acme Mail",
				},
				Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
					invoked.Store(name, true)
					return tools.ToolResult{Value: map[string]any{"ok": true}}, nil
				},
			})
		}
		swap, err := cat.StageSource(src, descs, false)
		if err != nil {
			t.Fatalf("StageSource(%s): %v", src, err)
		}
		swap.Commit()
	}
	return cat
}

func runCtx(t *testing.T, cat tools.ToolCatalog, p principal) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), p.id, "run-"+p.id.TenantID+"-"+p.id.UserID+"-"+p.id.SessionID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	base := tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: p.id.TenantID, UserID: p.id.UserID, SessionID: p.id.SessionID})
	return tools.WithRunToolView(ctx, ownerView{base: base, owned: map[tools.ToolSourceID]bool{p.source: true}})
}

// declared is the model-visible spelling of a physical catalog name.
func declared(physical string) string {
	name, _ := tools.NewModelToolNameProjectionWithReservedControls([]string{physical}).DeclaredName(physical)
	return name
}

func ownsResult(p principal, declaredName string) bool {
	for _, op := range []string{"send_message", "list_messages"} {
		if declaredName == declared(string(p.source)+"_"+op) {
			return true
		}
	}
	return false
}

func isolationPrincipals() []principal {
	return []principal{
		{id: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "s1"}, source: "mailsrv~a-tenanta"},
		{id: identity.Identity{TenantID: "tenant-b", UserID: "user-1", SessionID: "s1"}, source: "mailsrv~a-tenantb"},
		{id: identity.Identity{TenantID: "tenant-a", UserID: "user-2", SessionID: "s1"}, source: "mailsrv~u-tenanta2"},
		{id: identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "s2"}, source: "mailsrv~u-tenanta1s2"},
	}
}

func TestToolSearch_CrossPrincipalIsolation(t *testing.T) {
	principals := isolationPrincipals()
	var invoked sync.Map
	// 40 foreign copies of the same server: an index LIMIT applied before
	// visibility would return only foreign rows.
	cat := sharedServerCatalog(t, principals, 40, &invoked)

	for _, p := range principals {
		t.Run(p.id.TenantID+"/"+p.id.UserID+"/"+p.id.SessionID, func(t *testing.T) {
			ctx := runCtx(t, cat, p)
			for _, q := range []string{"send message", "mailbox", "acme mail", "messages", "mailb", ""} {
				out, err := toolSearchWithScopes(ctx, cat, ToolSearchArgs{Query: q, Limit: 5}, nil)
				if err != nil {
					t.Fatalf("tool_search(%q): %v", q, err)
				}
				for _, r := range out.Tools {
					if strings.HasPrefix(r.Name, "mailsrv") && !ownsResult(p, r.Name) {
						t.Fatalf("tool_search(%q) disclosed another principal's tool %q: %+v", q, r.Name, out.Tools)
					}
				}
				if q == "" {
					continue
				}
				// Deferred tools ARE discoverable within the run's own view,
				// even under a crowd of identical foreign rows.
				if out.Count == 0 {
					t.Fatalf("tool_search(%q) found none of the run's own deferred tools", q)
				}
				for _, r := range out.Tools {
					if ownsResult(p, r.Name) && r.Server != "Acme Mail" {
						t.Fatalf("result %q lacks its server label: %+v", r.Name, r)
					}
				}
			}
			// tool_get and declarative_action: a foreign name is indistinguishable
			// from an unknown one and never runs.
			for _, other := range principals {
				if other.source == p.source {
					continue
				}
				foreignPhysical := string(other.source) + "_send_message"
				foreign := declared(foreignPhysical)
				got, err := toolGetWithScopes(ctx, cat, ToolGetArgs{Name: foreign}, nil)
				if err != nil || got.Found || got.Description != "" || got.ArgsSchema != "" {
					t.Fatalf("tool_get(%q) described another principal's tool: %+v, %v", foreign, got, err)
				}
				action, err := declarativeActionWithScopes(ctx, cat, DeclarativeActionArgs{Tool: foreign, Args: json.RawMessage(`{}`)}, nil)
				if err != nil || action.Dispatched {
					t.Fatalf("declarative_action dispatched another principal's tool %q: %+v, %v", foreign, action, err)
				}
				if _, ran := invoked.Load(foreignPhysical); ran {
					t.Fatalf("another principal's tool %q was invoked", foreign)
				}
			}
			own := declared(string(p.source) + "_send_message")
			got, err := toolGetWithScopes(ctx, cat, ToolGetArgs{Name: own}, nil)
			if err != nil || !got.Found || got.Server != "Acme Mail" {
				t.Fatalf("tool_get(own deferred %q) = %+v, %v", own, got, err)
			}
			action, err := declarativeActionWithScopes(ctx, cat, DeclarativeActionArgs{Tool: own, Args: json.RawMessage(`{}`)}, nil)
			if err != nil || !action.Dispatched {
				t.Fatalf("declarative_action(own %q) = %+v, %v", own, action, err)
			}
			if _, ran := invoked.LoadAndDelete(string(p.source) + "_send_message"); !ran {
				t.Fatalf("declarative_action(own %q) did not run the run's own tool", own)
			}
		})
	}
}

// TestToolSearch_RespectsRunViewNarrowing proves the meta-tools honour the
// production view narrowings, not just owner scope: an allowlist and an
// exclusion hide tools from discovery exactly as from dispatch.
func TestToolSearch_RespectsRunViewNarrowing(t *testing.T) {
	principals := isolationPrincipals()[:1]
	var invoked sync.Map
	cat := sharedServerCatalog(t, principals, 0, &invoked)
	p := principals[0]
	ctx, err := identity.WithRun(context.Background(), p.id, "run-narrow")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	base := tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: p.id.TenantID, UserID: p.id.UserID, SessionID: p.id.SessionID})
	allowed := string(p.source) + "_list_messages"
	hidden := string(p.source) + "_send_message"
	views := map[string]tools.PlannerCatalogView{
		"allowlist": tools.NewAllowlistView(base, []string{allowed, "tool_search", "tool_get", "declarative_action"}),
		"exclusion": tools.NewExclusionView(base, nil, []string{hidden}),
	}
	for name, view := range views {
		t.Run(name, func(t *testing.T) {
			vctx := tools.WithRunToolView(ctx, view)
			out, err := toolSearchWithScopes(vctx, cat, ToolSearchArgs{Query: "mailbox"}, nil)
			if err != nil {
				t.Fatalf("tool_search: %v", err)
			}
			if out.Count != 1 || out.Tools[0].Name != declared(allowed) {
				t.Fatalf("tool_search = %+v, want only %q", out.Tools, allowed)
			}
			if got, _ := toolGetWithScopes(vctx, cat, ToolGetArgs{Name: declared(hidden)}, nil); got.Found {
				t.Fatalf("tool_get described a narrowed-out tool: %+v", got)
			}
			if action, _ := declarativeActionWithScopes(vctx, cat, DeclarativeActionArgs{Tool: declared(hidden), Args: json.RawMessage(`{}`)}, nil); action.Dispatched {
				t.Fatalf("declarative_action dispatched a narrowed-out tool: %+v", action)
			}
		})
	}
	if _, ran := invoked.Load(hidden); ran {
		t.Fatal("a narrowed-out tool was invoked")
	}
}

// TestToolSearch_RunViewIsolation_ConcurrentReuse drives the shared
// registered meta-tool from many runs of different principals at once.
func TestToolSearch_RunViewIsolation_ConcurrentReuse(t *testing.T) {
	principals := isolationPrincipals()
	var invoked sync.Map
	cat := sharedServerCatalog(t, principals, 10, &invoked)
	desc, ok := cat.Resolve("tool_search")
	if !ok {
		t.Fatal("tool_search not registered")
	}
	raw, _ := json.Marshal(ToolSearchArgs{Query: "mailbox", Limit: 50})
	var leaks atomic.Int64
	var wg sync.WaitGroup
	for i := range 64 {
		p := principals[i%len(principals)]
		ctx := runCtx(t, cat, p)
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := desc.Invoke(ctx, raw)
			if err != nil {
				t.Errorf("Invoke: %v", err)
				return
			}
			out, ok := res.Value.(ToolSearchOut)
			if !ok {
				t.Errorf("result type %T", res.Value)
				return
			}
			for _, r := range out.Tools {
				if strings.HasPrefix(r.Name, "mailsrv") && !ownsResult(p, r.Name) {
					leaks.Add(1)
				}
			}
			if out.Count != 2 {
				t.Errorf("principal %s found %d own tools, want 2", p.source, out.Count)
			}
		}()
	}
	wg.Wait()
	if leaks.Load() != 0 {
		t.Fatalf("%d cross-principal results under concurrency", leaks.Load())
	}
}

// TestSkillCapability_DerivesFromRunView is the SECURITY regression for the
// skill_* capability envelope and the skill_create_draft warning: both use
// the run's view (deferred tools included), never the process catalog, so a
// foreign tool can neither widen skill visibility nor be probed for
// presence through a draft warning.
func TestSkillCapability_DerivesFromRunView(t *testing.T) {
	principals := isolationPrincipals()
	var invoked sync.Map
	cat := sharedServerCatalog(t, principals, 3, &invoked)
	p, other := principals[0], principals[1]
	own := string(p.source) + "_send_message"
	foreign := string(other.source) + "_send_message"
	ctx := runCtx(t, cat, p)
	rc := RegistryContext{Catalog: cat}

	capability, err := runCapability(ctx, rc)
	if err != nil {
		t.Fatalf("runCapability: %v", err)
	}
	allowed := map[string]bool{}
	for _, n := range capability.AllowedTools {
		allowed[n] = true
	}
	if !allowed[own] {
		t.Fatalf("own deferred tool %q missing from capability %v", own, capability.AllowedTools)
	}
	for n := range allowed {
		if strings.HasPrefix(n, "mailsrv") && !strings.HasPrefix(n, string(p.source)+"_") {
			t.Fatalf("capability admitted another principal's tool %q", n)
		}
	}

	result := SkillCreateDraftResult{}
	result.RequiredTools = []string{own, foreign}
	allowedNames, err := runAllowedTools(ctx, rc.GrantedScopes)
	if err != nil {
		t.Fatalf("runAllowedTools: %v", err)
	}
	appendUnavailableToolWarnings(&result, allowedNames)
	var warnedOwn, warnedForeign bool
	for _, w := range result.Warnings {
		warnedOwn = warnedOwn || strings.Contains(w, own)
		warnedForeign = warnedForeign || strings.Contains(w, foreign)
	}
	if warnedOwn || !warnedForeign {
		t.Fatalf("warnings = %v; a foreign tool must read as absent, the own tool as present", result.Warnings)
	}

	// No seated view (or an explicit nil seat masking an outer one): fail closed.
	idCtx, err := identity.WithRun(context.Background(), p.id, "run-noview")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]context.Context{
		"absent": idCtx,
		"masked": tools.WithRunToolView(ctx, nil),
	} {
		if _, err := runCapability(c, rc); !errors.Is(err, ErrRunToolViewRequired) {
			t.Fatalf("%s: runCapability err = %v, want ErrRunToolViewRequired", name, err)
		}
		if _, err := runAllowedTools(c, rc.GrantedScopes); !errors.Is(err, ErrRunToolViewRequired) {
			t.Fatalf("%s: draft allowed-tools err = %v, want ErrRunToolViewRequired", name, err)
		}
		if _, err := toolSearchWithScopes(c, cat, ToolSearchArgs{Query: "mailbox"}, nil); !errors.Is(err, ErrRunToolViewRequired) {
			t.Fatalf("%s: tool_search err = %v, want ErrRunToolViewRequired", name, err)
		}
	}
}

// TestToolSearch_LaggingIndexIsToppedUpFromRunView: an index that misses a
// visible tool (failed or pending write) does not hide it — the in-memory
// pass over the run's own view fills the page after the ranked hits.
func TestToolSearch_LaggingIndexIsToppedUpFromRunView(t *testing.T) {
	ranked := tools.Tool{Name: "mail_send", Description: "mailbox send"}
	missing := tools.Tool{Name: "mail_read", Description: "mailbox read"}
	cat := tools.NewCatalog(tools.WithSearchCache(fixedSearchCache{results: []tools.Tool{ranked}}))
	for _, tl := range []tools.Tool{ranked, missing} {
		if err := cat.Register(tools.ToolDescriptor{Tool: tl, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := toolSearch(declarativeTestCtx(t), cat, ToolSearchArgs{Query: "mailbox"})
	if err != nil {
		t.Fatalf("tool_search: %v", err)
	}
	if out.Count != 2 || out.Tools[0].Name != "mail_send" || out.Tools[1].Name != "mail_read" {
		t.Fatalf("tool_search = %+v, want ranked hit then the index-missing tool", out.Tools)
	}
}

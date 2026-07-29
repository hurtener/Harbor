package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestBuildIdentityMeta_FlatKeyUnchanged pins that a key with no `.` behaves
// exactly as it did before annotation paths shipped — a top-level `_meta` key
// with a string value.
func TestBuildIdentityMeta_FlatKeyUnchanged(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	meta, err := buildIdentityMeta(ctx, map[string]string{"deployment": "prod-eu", "fleet": "west"})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if meta["deployment"] != "prod-eu" || meta["fleet"] != "west" {
		t.Fatalf("flat annotations changed shape: %+v", meta)
	}
}

// TestBuildIdentityMeta_DottedKeyNests is the phase's headline behaviour: a
// dotted annotation key writes into a NESTED `_meta` map instead of becoming a
// literal flat key.
func TestBuildIdentityMeta_DottedKeyNests(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	meta, err := buildIdentityMeta(ctx, map[string]string{
		"vendor.account_id": "acct-42",
		"vendor.tag":        "blue",
		"a.b.c":             "deep",
	})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if _, flat := meta["vendor.account_id"]; flat {
		t.Fatalf("dotted annotation still landed as a literal flat key: %+v", meta)
	}
	vendor, ok := meta["vendor"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.vendor = %#v, want map[string]any", meta["vendor"])
	}
	if vendor["account_id"] != "acct-42" || vendor["tag"] != "blue" {
		t.Fatalf("nested siblings wrong: %#v", vendor)
	}
	a, ok := meta["a"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.a = %#v, want map[string]any", meta["a"])
	}
	b, ok := a["b"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.a.b = %#v, want map[string]any", a["b"])
	}
	if b["c"] != "deep" {
		t.Fatalf("_meta.a.b.c = %#v, want \"deep\"", b["c"])
	}
}

// TestBuildIdentityMeta_NestingMatchesInjectionShape asserts the acceptance
// criterion literally: an annotation path and the SAME path written by the
// credential-injection mechanism produce byte-identical `_meta` shapes. Two
// mechanisms, one helper, one meaning.
func TestBuildIdentityMeta_NestingMatchesInjectionShape(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})

	viaAnnotation, err := buildIdentityMeta(ctx, map[string]string{"vendor.api_key": "SECRET"})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}

	viaInjection, err := buildIdentityMeta(ctx, nil)
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if err := injectMeta(viaInjection, []string{"vendor", "api_key"}, "SECRET"); err != nil {
		t.Fatalf("injectMeta: %v", err)
	}

	a, err := json.Marshal(viaAnnotation)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(viaInjection)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("annotation nesting and injection nesting disagree:\n annotation=%s\n injection =%s", a, b)
	}
}

// TestBuildIdentityMeta_IntermediateNodesAreMapStringAny is the map-type
// identity guard.
//
// injectMeta type-asserts `cur[seg].(map[string]any)`. `mcpsdk.Meta` is a NAMED
// type over `map[string]any`, and a Go type assertion to `map[string]any` FAILS
// on a dynamic type of `mcpsdk.Meta`. If the annotation nesting built
// `mcpsdk.Meta` intermediates, the assertion would miss, the create branch
// would fire, and the node would be SILENTLY REPLACED — wiping every sibling
// annotation in that namespace. This test asserts the concrete dynamic type of
// every intermediate node, mirroring the assertion in injectMeta itself.
//
// Mutation check: change the nesting to construct `mcpsdk.Meta{}` intermediates
// and this test fails on the type assertion below (and the sibling-survival
// assertion after it).
func TestBuildIdentityMeta_IntermediateNodesAreMapStringAny(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	meta, err := buildIdentityMeta(ctx, map[string]string{
		"vendor.account_id": "acct-42",
		"vendor.nested.tag": "blue",
	})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}

	// Every intermediate node, at every depth, is a plain map[string]any —
	// asserted the same way injectMeta asserts it.
	vendor, ok := meta["vendor"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.vendor has dynamic type %T, want map[string]any — an mcpsdk.Meta intermediate makes injectMeta's assertion miss and silently replace the node", meta["vendor"])
	}
	if _, isMeta := meta["vendor"].(mcpsdk.Meta); isMeta {
		t.Fatalf("_meta.vendor is an mcpsdk.Meta — the named type breaks injectMeta's map[string]any assertion")
	}
	nested, ok := vendor["nested"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.vendor.nested has dynamic type %T, want map[string]any", vendor["nested"])
	}
	if nested["tag"] != "blue" {
		t.Fatalf("_meta.vendor.nested.tag = %#v", nested["tag"])
	}

	// The credential write walks INTO the annotation's node rather than
	// replacing it: both leaves survive.
	if err := injectMeta(meta, []string{"vendor", "api_key"}, "SECRET"); err != nil {
		t.Fatalf("injectMeta into an annotation-created node: %v", err)
	}
	vendor, ok = meta["vendor"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.vendor replaced by the injection write: %T", meta["vendor"])
	}
	if vendor["account_id"] != "acct-42" {
		t.Fatalf("the injection write wiped the annotation sibling: %#v", vendor)
	}
	if vendor["api_key"] != "SECRET" {
		t.Fatalf("the injected credential is missing: %#v", vendor)
	}
}

// TestBuildIdentityMeta_ReservedPerSegmentSkipped extends the shipped
// merge-time re-check to the per-segment arm without weakening the whole-key
// arm. `tenant.foo` must not create a `tenant` node; `io.modelcontextprotocol/x`
// must still never appear.
func TestBuildIdentityMeta_ReservedPerSegmentSkipped(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	ctx = tools.WithInvokingAgent(ctx, "real-agent")
	meta, err := buildIdentityMeta(ctx, map[string]string{
		"tenant.foo":                        "EVIL",
		"vendor.session":                    "EVIL",
		"x.agent_id":                        "EVIL",
		"io.modelcontextprotocol/something": "EVIL",
		"vendor.tag":                        "fine",
	})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if meta["tenant"] != "t1" {
		t.Fatalf("a reserved FIRST segment created/overwrote the tenant node: %#v", meta["tenant"])
	}
	if meta["agent_id"] != "real-agent" {
		t.Fatalf("agent_id shadowed: %#v", meta["agent_id"])
	}
	if _, present := meta["x"]; present {
		t.Fatalf("an annotation with a reserved LAST segment was merged: %#v", meta["x"])
	}
	if _, present := meta["io.modelcontextprotocol/something"]; present {
		t.Fatalf("spec-prefixed annotation leaked into _meta")
	}
	vendor, ok := meta["vendor"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.vendor = %#v, want map[string]any", meta["vendor"])
	}
	if _, present := vendor["session"]; present {
		t.Fatalf("an annotation with a reserved MIDDLE/LAST segment was merged: %#v", vendor)
	}
	if vendor["tag"] != "fine" {
		t.Fatalf("the non-reserved sibling was dropped: %#v", vendor)
	}
}

// TestBuildIdentityMeta_IdentityStampsWinOverNestedAnnotation attempts exactly
// what the acceptance criterion names: shadow identity via a nested path.
func TestBuildIdentityMeta_IdentityStampsWinOverNestedAnnotation(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	ctx = tools.WithInvokingAgent(ctx, "real-agent")
	meta, err := buildIdentityMeta(ctx, map[string]string{
		"tenant":       "EVIL-flat",
		"tenant.inner": "EVIL-nested",
		"agent_id":     "EVIL-flat",
	})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if meta["tenant"] != "t1" || meta["user"] != "u1" || meta["session"] != "s1" {
		t.Fatalf("identity triple shadowed: %+v", meta)
	}
	if meta["agent_id"] != "real-agent" {
		t.Fatalf("agent provenance shadowed: %+v", meta)
	}
}

// TestBuildIdentityMeta_LegacyCollisionFailsLoud pins the merge-time refusal.
// A revision persisted before the path rules shipped can carry a colliding
// annotation pair — nothing rejected one then. The merge must fail with a typed
// ErrMetaPathCollision rather than pick a winner (which, under Go's randomised
// map iteration, would not even be a STABLE winner).
func TestBuildIdentityMeta_LegacyCollisionFailsLoud(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	cases := []map[string]string{
		{"vendor": "flat", "vendor.id": "nested"},
		{"a.b": "x", "a.b.c": "y"},
	}
	for _, annotations := range cases {
		_, err := buildIdentityMeta(ctx, annotations)
		if err == nil {
			t.Fatalf("buildIdentityMeta accepted a colliding annotation pair %v", annotations)
		}
		if !errors.Is(err, ErrMetaPathCollision) {
			t.Fatalf("want ErrMetaPathCollision for %v, got %v", annotations, err)
		}
	}
}

// TestInjectMeta_NonMapIntermediateFailsLoud pins the last-resort defence
// inside the shared helper: a scalar sitting where a path needs a node is a
// collision and is refused, never overwritten. Overwriting it is the silent
// degradation (§13) that discarded an operator's annotation when a credential
// wrote into the same namespace.
func TestInjectMeta_NonMapIntermediateFailsLoud(t *testing.T) {
	meta := mcpsdk.Meta{"vendor": "flat-scalar"}
	err := injectMeta(meta, []string{"vendor", "api_key"}, "SECRET")
	if err == nil {
		t.Fatal("injectMeta silently overwrote a non-map intermediate")
	}
	if meta["vendor"] != "flat-scalar" {
		t.Fatalf("injectMeta mutated the colliding node before failing: %#v", meta["vendor"])
	}

	// The mirror case: a leaf write onto an existing nested map.
	meta2 := mcpsdk.Meta{}
	if err := injectMeta(meta2, []string{"vendor", "nested", "k"}, "v"); err != nil {
		t.Fatalf("injectMeta: %v", err)
	}
	if err := injectMeta(meta2, []string{"vendor", "nested"}, "SCALAR"); err == nil {
		t.Fatal("injectMeta silently replaced a populated node with a scalar leaf")
	}
}

// TestBuildIdentityMeta_Deterministic asserts the merged `_meta` is
// byte-identical across N repetitions despite Go's randomised map iteration at
// the annotation merge.
//
// Determinism is a CONSEQUENCE of refusing colliding paths, not of a sort:
// distinct non-prefixing paths write disjoint leaves, so visit order cannot
// change the result. Mutation check: make injectMeta replace an existing
// intermediate unconditionally (`next := map[string]any{}; cur[seg] = next`) and
// this test fails — `vendor.tag` and `vendor.region` then clobber each other in
// whichever order the runtime happens to visit them.
func TestBuildIdentityMeta_Deterministic(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	annotations := map[string]string{
		"vendor.tag":    "blue",
		"vendor.region": "eu",
		"fleet":         "west",
	}
	var want string
	for i := range 1000 {
		meta, err := buildIdentityMeta(ctx, annotations)
		if err != nil {
			t.Fatalf("buildIdentityMeta: %v", err)
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			want = string(raw)
			// Sanity: both siblings actually landed, so a merge that dropped
			// one on every iteration could not pass by being consistently
			// wrong.
			vendor, ok := meta["vendor"].(map[string]any)
			if !ok || vendor["tag"] != "blue" || vendor["region"] != "eu" {
				t.Fatalf("both namespace siblings must survive the merge: %#v", meta["vendor"])
			}
			continue
		}
		if string(raw) != want {
			t.Fatalf("merge is order-dependent at iteration %d:\n want=%s\n got =%s", i, want, raw)
		}
	}
}

// TestBuildIdentityMeta_ConcurrentReuse is the D-025 concurrent-reuse gate,
// pointed at the two hazards that are actually REACHABLE here.
//
// (The obvious "the source annotation map is unmodified" assertion is inert:
// Config.MetaAnnotations is a map[string]string and cannot hold nested maps,
// and buildIdentityMeta allocates a fresh meta per call, so it would pass
// against any implementation.)
//
// What this asserts under -race, against ONE shared *Provider:
//
//	(a) determinism — every call's marshalled `_meta` for a given identity is
//	    byte-identical, which is where randomised map iteration actually bites;
//	(b) distinct object graphs — mutating one call's nested node is invisible to
//	    every other call (injectMeta allocates intermediates, so this IS
//	    reachable);
//	(c) no identity bleed — each call's stamps match ITS OWN ctx triple across N
//	    concurrent DISTINCT identities.
func TestBuildIdentityMeta_ConcurrentReuse(t *testing.T) {
	const n = 128

	// One shared compiled artifact; every goroutine reads its annotations.
	shared := &Provider{cfg: Config{
		Name: "shared",
		MetaAnnotations: map[string]string{
			"vendor.tag":    "blue",
			"vendor.region": "eu",
			"fleet":         "west",
		},
	}}

	baseline := runtime.NumGoroutine()

	type result struct {
		tenant string
		meta   mcpsdk.Meta
		json   string
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t%d", i),
				UserID:    fmt.Sprintf("u%d", i),
				SessionID: fmt.Sprintf("s%d", i),
			}
			ctx, err := identity.With(t.Context(), id)
			if err != nil {
				t.Errorf("identity.With: %v", err)
				return
			}
			<-start
			meta, err := buildIdentityMeta(ctx, shared.cfg.MetaAnnotations)
			if err != nil {
				t.Errorf("buildIdentityMeta: %v", err)
				return
			}
			raw, err := json.Marshal(meta)
			if err != nil {
				t.Errorf("marshal: %v", err)
				return
			}
			results[i] = result{tenant: id.TenantID, meta: meta, json: string(raw)}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		// (c) no identity bleed — each call carries its OWN triple.
		if r.meta["tenant"] != fmt.Sprintf("t%d", i) ||
			r.meta["user"] != fmt.Sprintf("u%d", i) ||
			r.meta["session"] != fmt.Sprintf("s%d", i) {
			t.Fatalf("identity bleed at %d: %+v", i, r.meta)
		}
		// (a) determinism — the annotation half is identical for every call
		// once the (deliberately distinct) triple is removed.
		vendor, ok := r.meta["vendor"].(map[string]any)
		if !ok {
			t.Fatalf("call %d: _meta.vendor = %#v, want map[string]any", i, r.meta["vendor"])
		}
		if vendor["tag"] != "blue" || vendor["region"] != "eu" || r.meta["fleet"] != "west" {
			t.Fatalf("call %d: annotations drifted under concurrency: %s", i, r.json)
		}
	}

	// (b) distinct object graphs — mutating one call's nested node must be
	// invisible to every other call. A shared intermediate would leak here.
	mutated, _ := results[0].meta["vendor"].(map[string]any)
	mutated["tag"] = "MUTATED"
	for i := 1; i < n; i++ {
		vendor, _ := results[i].meta["vendor"].(map[string]any)
		if vendor["tag"] != "blue" {
			t.Fatalf("call %d shares its nested `_meta` node with call 0 (D-025 no-shared-structure): %#v", i, vendor)
		}
	}

	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, got)
	}
}

// TestAttachDoor_MetaAnnotationPaths drives the ATTACH door (resolveOAuthBinding,
// the shared boot + runtime-set path) — the fourth door an earlier draft of this
// work missed entirely.
func TestAttachDoor_MetaAnnotationPaths(t *testing.T) {
	// The cases below bind no provider — the annotation gate runs BEFORE the
	// binding resolution, which is the point: an invalid annotation path fails
	// the attach regardless of what else the connection declares.
	deep := strings.Repeat("a.", config.MaxMCPMetaKeyDepth) + "leaf"

	cases := []struct {
		name     string
		server   config.MCPServerConfig
		wantText string
	}{
		{
			"reserved FIRST segment refused (newly)",
			config.MCPServerConfig{Name: "x", MetaAnnotations: map[string]string{"tenant.foo": "y"}},
			"reserved",
		},
		{
			"reserved LAST segment refused",
			config.MCPServerConfig{Name: "x", MetaAnnotations: map[string]string{"vendor.agent_id": "y"}},
			"reserved",
		},
		{
			"over-deep annotation path refused",
			config.MCPServerConfig{Name: "x", MetaAnnotations: map[string]string{deep: "y"}},
			"exceeding the cap",
		},
		{
			"annotation/annotation collision refused",
			config.MCPServerConfig{Name: "x", MetaAnnotations: map[string]string{"vendor": "a", "vendor.id": "b"}},
			"collide",
		},
		{
			"flat annotation colliding with the injection _meta path refused",
			config.MCPServerConfig{
				Name:            "x",
				MetaAnnotations: map[string]string{"vendor": "a"},
				Injection: &config.MCPCredentialInjectionConfig{
					Provider: "broker", Form: config.MCPInjectionFormMeta, MetaKey: "vendor.api_key",
				},
			},
			"collide",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveOAuthBinding(tc.server, TransportStreamableHTTP, mapProviderResolver(nil))
			if err == nil {
				t.Fatalf("attach door accepted %s", tc.name)
			}
			if !errors.Is(err, ErrOAuthBinding) {
				t.Fatalf("want ErrOAuthBinding, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err=%q missing text %q", err.Error(), tc.wantText)
			}
		})
	}

	t.Run("a nested annotation sharing the injection namespace is admitted", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name:            "x",
			MetaAnnotations: map[string]string{"vendor.account_id": "acct-42", "deployment": "prod"},
			Injection: &config.MCPCredentialInjectionConfig{
				Provider: "broker", Form: config.MCPInjectionFormMeta, MetaKey: "vendor.api_key",
			},
		}, TransportStreamableHTTP, mapProviderResolver(nil))
		if err != nil {
			t.Fatalf("attach door refused the arrangement this phase exists to enable: %v", err)
		}
	})
}

// TestConfigValidate_InjectionMetaKeyDepthCap closes the wire-only asymmetry in
// the DRIVER's own Config.validate(): the cap now comes from the one hoisted
// constant every door consults.
func TestConfigValidate_InjectionMetaKeyDepthCap(t *testing.T) {
	deep := make([]string, 0, config.MaxMCPMetaKeyDepth+1)
	for range config.MaxMCPMetaKeyDepth {
		deep = append(deep, "x")
	}
	deep = append(deep, "token")

	c := Config{
		Name: "x", URL: "https://x.example.test", TransportMode: TransportStreamableHTTP,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
		Injection: &CredentialInjection{
			Provider: &stubOAuthProvider{token: "t"},
			Form:     InjectionFormMeta,
			MetaKey:  deep,
		},
	}
	err := c.validate()
	if err == nil {
		t.Fatal("Config.validate accepted an over-deep injection MetaKey")
	}
	if !strings.Contains(err.Error(), "exceeding the cap") {
		t.Errorf("err=%q missing the depth-cap text", err.Error())
	}
}

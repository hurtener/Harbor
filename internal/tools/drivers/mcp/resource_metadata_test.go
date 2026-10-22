package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

func appResourceMeta(value any) map[string]any {
	return map[string]any{"ui": map[string]any{"csp": map[string]any{"resourceDomains": value, "connectDomains": []any{"https://ignored.example.com"}}, "private": "do not project"}, "secret": "do not project"}
}

func TestAppResourceMetadata_ClosedBoundsAndOrigins(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want []string
		bad  bool
	}{
		{"absent", nil, nil, false},
		{"unrelated", map[string]any{"secret": strings.Repeat("x", 9000)}, nil, false},
		{"other_directives", map[string]any{"ui": map[string]any{"csp": map[string]any{"connectDomains": []any{"https://ignored.example.com"}}}}, nil, false},
		{"standard", appResourceMeta([]any{"https://assets.example.com", "https://media.example.com"}), []string{"https://assets.example.com", "https://media.example.com"}, false},
		{"empty", appResourceMeta([]any{}), []string{}, false},
		{"duplicate", appResourceMeta([]string{"https://assets.example.com", "https://assets.example.com"}), []string{"https://assets.example.com"}, false},
		{"ui_scalar", map[string]any{"ui": true}, nil, true},
		{"csp_scalar", map[string]any{"ui": map[string]any{"csp": false}}, nil, true},
		{"null", appResourceMeta(nil), nil, true},
		{"scalar", appResourceMeta("https://assets.example.com"), nil, true},
		{"nonstring", appResourceMeta([]any{1}), nil, true},
		{"count", appResourceMeta(make([]any, 33)), nil, true},
	}
	for _, origin := range []string{"http://assets.example.com", "https://*.example.com", "https://assets.example.com/", "https://assets.example.com/a", "https://assets.example.com?q=x", "https://assets.example.com#x", "https://user:secret@assets.example.com", "https://localhost", "https://127.0.0.1", "https://127.1", "https://0x7f000001", "https://2130706433", "https://[::1]", "https://10.1.2.3", "https://a.local", "https://a.internal", "https://a.test", "https://a.invalid", "https://a.home.arpa", "https://ASSETS.example.com", "https://assets.example.com:443", "https://assets.example.com:0443", "https://assets.example.com:08443", "https://assets.example.com:99999", strings.Repeat("x", 2049)} {
		cases = append(cases, struct {
			name string
			meta map[string]any
			want []string
			bad  bool
		}{"origin_" + origin, appResourceMeta([]string{origin}), nil, true})
	}
	boundary := make([]string, 32)
	for i := range boundary {
		boundary[i] = fmt.Sprintf("https://assets%d.example.com", i)
	}
	cases = append(cases, struct {
		name string
		meta map[string]any
		want []string
		bad  bool
	}{"32", appResourceMeta(boundary), boundary, false})
	long := make([]string, 32)
	for i := range long {
		long[i] = "https://" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 57) + ".com"
	}
	cases = append(cases, struct {
		name string
		meta map[string]any
		want []string
		bad  bool
	}{"encoded_total", appResourceMeta(long), nil, true})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resourceDomains(tc.meta)
			if tc.bad {
				if !errors.Is(err, ErrResourceMetadata) {
					t.Fatalf("want typed refusal, got %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("domains %v error %v", got, err)
			}
		})
	}
	for raw, want := range map[string]string{
		"https://API.example.com:443/private/mcp?token=secret#fragment": "https://api.example.com",
		"https://api.example.com:0443/mcp":                              "https://api.example.com",
		"https://api.example.com:08443/mcp":                             "https://api.example.com:8443",
		"https://api.example.com:8443/mcp":                              "https://api.example.com:8443",
		"https://user:secret@api.example.com/mcp":                       "", "http://api.example.com/mcp": "", "https://10.0.0.1/mcp": "", "https://api.internal/mcp": "", "node /private/path": "",
	} {
		if got := PublicHTTPSOrigin(raw); got != want {
			t.Fatalf("origin projection %q got %q", raw, got)
		}
	}
}

// Official MCP Go SDK ResourceContents._meta is the AppsSDK standard location;
// outer result metadata, HTML and adjacent content may not lend declarations.
func TestProvider_AppResourceMetadata_ActualSDK(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*mcpsdk.ReadResourceResult)
		bad, want bool
	}{
		{"standard", func(r *mcpsdk.ReadResourceResult) {
			r.Contents[0].Meta = appResourceMeta([]any{"https://assets.example.com"})
		}, false, true},
		{"outer_only", func(r *mcpsdk.ReadResourceResult) { r.Meta = appResourceMeta([]any{"https://outer.example.com"}) }, false, false},
		{"missing", func(*mcpsdk.ReadResourceResult) {}, false, false},
		{"malformed", func(r *mcpsdk.ReadResourceResult) { r.Contents[0].Meta = appResourceMeta([]any{false}) }, true, false},
		{"other_resource", func(r *mcpsdk.ReadResourceResult) {
			r.Contents[0].URI = "ui://adjacent"
			r.Contents[0].Meta = appResourceMeta([]any{"https://adjacent.example.com"})
		}, true, false},
		{"ambiguous", func(r *mcpsdk.ReadResourceResult) { r.Contents = append(r.Contents, r.Contents[0]) }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newAppsProviderResult(t, func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				r := &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, Text: `<img src="https://untrusted.example.com/a">`, MIMEType: ResourceMIMEType}}}
				tc.mutate(r)
				return r, nil
			})
			got, err := p.ReadResourceWithMetadata(appsIdentityCtx(t), "ui://app/main.html")
			if tc.bad {
				if !errors.Is(err, ErrResourceMetadata) {
					t.Fatalf("expected metadata refusal: %v", err)
				}
				return
			}
			if err != nil || (len(got.ResourceDomains) == 1) != tc.want {
				t.Fatalf("read %+v error %v", got, err)
			}
			if got.ProviderOrigin != "" {
				t.Fatal("provider may not self-assert origin")
			}
		})
	}
}

type replacingResourceProvider struct {
	*stubProvider
	after   func()
	domains []string
}

func (p *replacingResourceProvider) ReadResourceWithMetadata(context.Context, string) (ResourceRead, error) {
	if p.after != nil {
		p.after()
	}
	return ResourceRead{Content: []byte("old"), ResourceDomains: p.domains, ProviderOrigin: "https://self.example.com"}, nil
}

func TestRegistry_AppResourceMetadata_ValidatesEmbedderProjection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains []string
		want    []string
		bad     bool
	}{
		{name: "wildcard", domains: []string{"https://*.example.com"}, bad: true},
		{name: "private_host", domains: []string{"https://localhost"}, bad: true},
		{name: "valid_deduped", domains: []string{"https://assets.example.com", "https://assets.example.com"}, want: []string{"https://assets.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := appsIdentityCtx(t)
			reg := NewRegistry()
			provider := &replacingResourceProvider{stubProvider: &stubProvider{id: "metadata"}, domains: tc.domains}
			if err := reg.Register(ctx, ServerRegistration{Provider: provider, Transport: string(TransportStdio)}); err != nil {
				t.Fatal(err)
			}
			read, err := reg.ReadResourceWithMetadata(ctx, "metadata", "ui://workspace/main")
			if tc.bad {
				if !errors.Is(err, ErrResourceMetadata) {
					t.Fatalf("invalid embedder CSP accepted: %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(read.ResourceDomains, tc.want) || read.ProviderOrigin != "" {
				t.Fatalf("embedder projection %+v: %v", read, err)
			}
		})
	}
}

func TestRegistry_AppResourceMetadata_CurrentIdentityAndOrigin(t *testing.T) {
	ctx := appsIdentityCtx(t)
	owner := auth.Owner{Tenant: defaultIdentity().TenantID, Agent: "agent", User: defaultIdentity().UserID}
	reg := NewRegistry()
	p := &stubProvider{id: tools.ToolSourceID(PhysicalServerName("app", owner)), resourceBody: []byte("document")}
	if err := reg.Register(ctx, ServerRegistration{Provider: p, Owner: owner, LogicalName: "app", Transport: string(TransportStreamableHTTP), URLOrCommand: "https://api.example.com/private/mcp?key=secret"}); err != nil {
		t.Fatal(err)
	}
	got, err := reg.ReadResourceWithMetadata(ctx, string(p.id), "ui://app/main")
	if err != nil || got.ProviderOrigin != "https://api.example.com" || got.ResourceDomains != nil {
		t.Fatalf("legacy provider projection %+v %v", got, err)
	}
	foreign, _ := identity.With(context.Background(), identity.Identity{TenantID: "other", UserID: "other", SessionID: "other"})
	if _, err := reg.ReadResourceWithMetadata(foreign, string(p.id), "ui://app/main"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("cross identity read %v", err)
	}
	if _, err := reg.ReadResourceWithMetadata(context.Background(), string(p.id), "ui://app/main"); !errors.Is(err, ErrRegistryIdentityMissing) {
		t.Fatalf("missing identity %v", err)
	}
	if err := reg.CheckResourceRead(ctx, ResourceRead{}); err == nil {
		t.Fatal("forged read accepted")
	}
	replaced := &replacingResourceProvider{stubProvider: &stubProvider{id: "changing"}}
	replaced.after = func() {
		reg.mu.Lock()
		reg.servers["changing"] = &serverEntry{provider: &stubProvider{id: "changing"}}
		reg.mu.Unlock()
	}
	if err := reg.Register(ctx, ServerRegistration{Provider: replaced, Transport: string(TransportStreamableHTTP), URLOrCommand: "https://old.example.com/mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ReadResourceWithMetadata(ctx, "changing", "ui://app/main"); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("replacement during read %v", err)
	}
}

func TestProvider_AppResourceMetadata_ConcurrentReuse(t *testing.T) {
	p := newAppsProviderResult(t, func(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
		// The actual SDK roundtrip carries distinct identities on each read.
		session, ok := req.Params.Meta["session"].(string)
		if !ok {
			return nil, errors.New("missing per-request session")
		}
		return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, Text: session, Meta: appResourceMeta([]any{"https://assets.example.com"})}}}, nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := range 128 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := defaultIdentity()
			id.SessionID = fmt.Sprintf("session-%d", i)
			ctx, _ := identity.With(context.Background(), id)
			got, err := p.ReadResourceWithMetadata(ctx, "ui://app/main.html")
			if err != nil || string(got.Content) != id.SessionID || len(got.ResourceDomains) != 1 {
				errs <- fmt.Errorf("request %d got %q: %v", i, got.Content, err)
				return
			}
			got.ResourceDomains[0] = "detached"
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	got, err := p.ReadResourceWithMetadata(appsIdentityCtx(t), "ui://app/main.html")
	if err != nil || got.ResourceDomains[0] != "https://assets.example.com" {
		t.Fatalf("mutation leaked: %+v %v", got, err)
	}
}

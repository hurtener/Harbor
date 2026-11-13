package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// TestDefaultLoading_StampsToolFormDescriptors pins the deployment-wide
// MCP loading default: unset keeps every tool-form descriptor "always",
// "deferred" defers them, and resource/prompt wrappers stay deferred
// under both.
func TestDefaultLoading_StampsToolFormDescriptors(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode tools.LoadingMode
		want tools.LoadingMode
	}{
		{"unset", "", tools.LoadingAlways},
		{"always", tools.LoadingAlways, tools.LoadingAlways},
		{"deferred", tools.LoadingDeferred, tools.LoadingDeferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockServer()
			p, err := New(Config{
				Name: "mock", URL: "http://example.invalid", TransportMode: TransportAuto,
				Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), DefaultLoading: tc.mode,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			cleanup := pairProvider(t, m, p)
			t.Cleanup(func() {
				_ = p.Close(context.Background())
				cleanup()
			})
			descs, err := p.Discover(mustIdentity(t))
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			var toolForms, wrappers int
			for _, d := range descs {
				if d.Tool.Form == tools.ToolFormTool {
					toolForms++
					if d.Tool.Loading != tc.want {
						t.Errorf("tool %s loading=%q, want %q", d.Tool.Name, d.Tool.Loading, tc.want)
					}
					continue
				}
				wrappers++
				if d.Tool.Loading != tools.LoadingDeferred {
					t.Errorf("wrapper %s loading=%q, want deferred", d.Tool.Name, d.Tool.Loading)
				}
			}
			if toolForms == 0 || wrappers == 0 {
				t.Fatalf("fixture must expose tools and wrappers: tools=%d wrappers=%d", toolForms, wrappers)
			}
		})
	}
}

func TestDefaultLoading_RejectsUnknownMode(t *testing.T) {
	_, err := New(Config{Name: "mock", URL: "http://example.invalid", Bus: newTestBus(t), DefaultIdentity: defaultIdentity(), DefaultLoading: "lazy"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New with unknown DefaultLoading: err=%v, want ErrInvalidConfig", err)
	}
}

package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestInvocationBoundaryPreservesReplacedOuterInvoke(t *testing.T) {
	for _, simulate := range []bool{false, true} {
		t.Run(map[bool]string{false: "veto", true: "simulation"}[simulate], func(t *testing.T) {
			calls, hooks := 0, 0
			veto := errors.New("outer authorization veto")
			d := tools.ToolDescriptor{Tool: tools.Tool{Name: "guarded"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				calls++
				return tools.ToolResult{Value: "real"}, nil
			}}
			d = tools.WrapInvocationGate(d, func(inner tools.Invocation) tools.Invocation { return inner })
			// This is the supported existing descriptor-copy pattern, also used by the
			// public harbortest simulator. It must never disappear during decoration.
			d.Invoke = func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				if simulate {
					return tools.ToolResult{Value: "synthetic"}, nil
				}
				return tools.ToolResult{}, veto
			}
			d = tools.DecorateInvocation(d, func(inner tools.Invocation) tools.Invocation {
				return func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
					hooks++
					return inner(ctx, args)
				}
			})
			got, err := d.Invoke(t.Context(), nil)
			if calls != 0 || hooks != 0 {
				t.Fatalf("outer wrapper bypassed: real calls=%d hooks=%d", calls, hooks)
			}
			if simulate {
				if err != nil || got.Value != "synthetic" {
					t.Fatalf("simulation replaced: %+v %v", got, err)
				}
			} else if !errors.Is(err, veto) {
				t.Fatalf("veto lost: %v", err)
			}
		})
	}
}

func TestInvocationBoundaryRejectsRenamedBinding(t *testing.T) {
	called := false
	d := tools.WithInvocationBoundary(tools.ToolDescriptor{Tool: tools.Tool{Name: "original"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		called = true
		return tools.ToolResult{}, nil
	}})
	d.Tool.Name = "different"
	d = tools.DecorateInvocation(d, func(inner tools.Invocation) tools.Invocation { return inner })
	_, err := d.Invoke(t.Context(), nil)
	if !errors.Is(err, tools.ErrInvocationCleanupFailed) || called {
		t.Fatalf("changed descriptor crossed native boundary: called=%v err=%v", called, err)
	}
}

package routers

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// UnionRouter dispatches based on payload tag — a string discriminator
// the Tag function extracts from env.Payload. The result keys into
// Branches; Default catches the unknown-tag case.
//
// Used for sum-type-shaped payloads. Example:
//
//	router := &UnionRouter{
//	    Tag: func(p any) string {
//	        switch p.(type) {
//	        case planner.CallTool:    return "call_tool"
//	        case planner.Finish:      return "finish"
//	        }
//	        return ""
//	    },
//	    Branches: map[string]engine.NodeRef{
//	        "call_tool": toolDispatcher.Ref(),
//	        "finish":    finalizer.Ref(),
//	    },
//	    Default: nil, // unknown tag → ErrRouteNotFound
//	}
type UnionRouter struct {
	Tag      func(any) string
	Branches map[string]engine.NodeRef
	Default  *engine.NodeRef
}

// AsNode wraps the router as an engine.Node. The wrapped NodeFunc
// extracts the tag, looks up the branch, writes the chosen target
// into Meta[MetaKeyRoutePolicy], and returns the envelope unchanged.
func (r *UnionRouter) AsNode(name string) engine.Node {
	return engine.Node{
		Name: name,
		Func: func(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			target, err := r.Select(env)
			if err != nil {
				return messages.Envelope{}, err
			}
			out := withRoutePolicy(env, target)
			return out, nil
		},
	}
}

// Select extracts the payload tag and returns the matching branch.
// A nil Tag function or a nil receiver returns ErrRouteNotFound. An
// empty / unknown tag falls back to Default; nil Default returns
// ErrRouteNotFound with the offending tag in the wrapped message.
func (r *UnionRouter) Select(env messages.Envelope) (engine.NodeRef, error) {
	if r == nil || r.Tag == nil {
		return engine.NodeRef{}, fmt.Errorf("%w: nil UnionRouter or Tag function", ErrRouteNotFound)
	}
	tag := r.Tag(env.Payload)
	if tag != "" {
		if target, ok := r.Branches[tag]; ok {
			return target, nil
		}
	}
	if r.Default != nil {
		return *r.Default, nil
	}
	return engine.NodeRef{}, fmt.Errorf("%w: union router tag=%q has no branch", ErrRouteNotFound, tag)
}

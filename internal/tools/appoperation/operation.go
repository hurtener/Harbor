// Package appoperation carries exact App operation coordinates through admitted
// browser and trusted planner requests. It grants no authority: the broker must
// check its current policy and the verified caller coordinates.
package appoperation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// ErrInvalid is a closed, content-free refusal of an operation binding.
var ErrInvalid = errors.New("invalid app operation binding")

// Binding is the broker request extension. Agent tool bindings carry bounded
// canonical outgoing arguments; identity and destination remain authenticated separately.
type Binding struct {
	Version         string          `json:"version"`
	Reference       string          `json:"reference"`
	Kind            string          `json:"kind"`
	AgentID         string          `json:"agent_id"`
	ServerID        string          `json:"server_id"`
	ResourceURI     string          `json:"resource_uri"`
	Generation      string          `json:"generation,omitempty"`
	Tool            string          `json:"tool,omitempty"`
	ArgumentsSHA256 string          `json:"arguments_sha256,omitempty"`
	Arguments       json.RawMessage `json:"arguments,omitempty"`
}

type contextKey struct{}
type agentInvocationKey struct{}

// WithAgentInvocation marks the trusted planner dispatch boundary with its
// reach-admitted effective agent. This is not a Protocol or model input.
func WithAgentInvocation(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, agentInvocationKey{}, agent)
}

// AgentInvocationFrom returns the agent stamped by trusted planner dispatch.
func AgentInvocationFrom(ctx context.Context) (string, bool) {
	agent, ok := ctx.Value(agentInvocationKey{}).(string)
	return agent, ok && agent != ""
}

// WithAgentCall binds one trusted planner invocation to its final outgoing input.
// The random reference is a call coordinate, never authority or a browser selector.
func WithAgentCall(ctx context.Context, agent, server, tool string, args json.RawMessage) (context.Context, error) {
	canonical, digest, err := CanonicalArguments(args)
	if err != nil {
		return nil, err
	}
	ref, err := newReference()
	if err != nil {
		return nil, err
	}
	b := Binding{Version: "app-operation-v1", Reference: ref, Kind: "agent_tool",
		AgentID: agent, ServerID: server, Tool: tool, Arguments: canonical, ArgumentsSHA256: digest}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, b), nil
}

// WithAgentRead binds one trusted planner invocation to an exact App resource.
func WithAgentRead(ctx context.Context, agent, server, resource string) (context.Context, error) {
	ref, err := newReference()
	if err != nil {
		return nil, err
	}
	b := Binding{Version: "app-operation-v1", Reference: ref, Kind: "agent_resource",
		AgentID: agent, ServerID: server, ResourceURI: resource}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, b), nil
}

func newReference() (string, error) {
	var ref [32]byte
	if _, err := rand.Read(ref[:]); err != nil {
		return "", ErrInvalid
	}
	return hex.EncodeToString(ref[:]), nil
}

// WithRead binds a selector to the resource read admitted by the Protocol.
func WithRead(ctx context.Context, ref, agent, server, resource string) (context.Context, error) {
	b := Binding{Version: "app-operation-v1", Reference: ref, Kind: "resource",
		AgentID: agent, ServerID: server, ResourceURI: resource}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, b), nil
}

// WithCall binds a selector to a fresh-render-admitted exact tool invocation.
func WithCall(ctx context.Context, ref, agent, server, resource, generation, tool string, args json.RawMessage) (context.Context, error) {
	digest, err := ArgumentsDigest(args)
	if err != nil {
		return nil, err
	}
	b := Binding{Version: "app-operation-v1", Reference: ref, Kind: "tool",
		AgentID: agent, ServerID: server, ResourceURI: resource,
		Generation: generation, Tool: tool, ArgumentsSHA256: digest}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, b), nil
}

// From returns a value copy; callers cannot mutate another request's binding.
func From(ctx context.Context) (Binding, bool) {
	b, ok := ctx.Value(contextKey{}).(Binding)
	b.Arguments = append(json.RawMessage(nil), b.Arguments...)
	return b, ok
}

func (b Binding) validate() error {
	if b.Version != "app-operation-v1" || len(b.Reference) != 64 ||
		strings.Trim(b.Reference, "0123456789abcdef") != "" ||
		!bounded(b.AgentID, 256) || !bounded(b.ServerID, 256) ||
		(b.Kind != "agent_tool" && (!bounded(b.ResourceURI, 2048) || !strings.HasPrefix(b.ResourceURI, "ui://"))) {
		return ErrInvalid
	}
	switch b.Kind {
	case "resource", "agent_resource":
		if b.Tool != "" || b.ArgumentsSHA256 != "" || b.Generation != "" || len(b.Arguments) != 0 {
			return ErrInvalid
		}
	case "tool":
		if !bounded(b.Tool, 256) || !bounded(b.Generation, 512) || len(b.ArgumentsSHA256) != 64 || strings.Trim(b.ArgumentsSHA256, "0123456789abcdef") != "" || len(b.Arguments) != 0 {
			return ErrInvalid
		}
	case "agent_tool":
		if b.ResourceURI != "" || b.Generation != "" || !bounded(b.Tool, 256) || len(b.Arguments) == 0 || len(b.Arguments) > 8192 || len(b.ArgumentsSHA256) != 64 {
			return ErrInvalid
		}
		_, digest, err := CanonicalArguments(b.Arguments)
		if err != nil || digest != b.ArgumentsSHA256 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func bounded(s string, limit int) bool {
	if s == "" || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Digest identifies the complete binding acknowledged by the broker.
func (b Binding) Digest() (string, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// CheckResource refuses reuse for another source or resource before credential
// resolution or network dispatch.
func (b Binding) CheckResource(server, resource string) error {
	if (b.Kind != "resource" && b.Kind != "agent_resource") || b.ServerID != server || b.ResourceURI != resource {
		return ErrInvalid
	}
	return b.validate()
}

// CheckCall compares the actual outgoing MCP arguments, after any transformation.
func (b Binding) CheckCall(server, tool string, args map[string]any) error {
	if (b.Kind != "tool" && b.Kind != "agent_tool") || b.ServerID != server || b.Tool != tool {
		return ErrInvalid
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ErrInvalid
	}
	digest, err := ArgumentsDigest(raw)
	if err != nil || digest != b.ArgumentsSHA256 {
		return ErrInvalid
	}
	return b.validate()
}

// ArgumentsDigest hashes a bounded, duplicate-free JSON object after key sorting
// and string normalization. Number spelling is preserved (including integers
// beyond float64 precision). Absent arguments mean the empty object.
func ArgumentsDigest(raw json.RawMessage) (string, error) {
	_, digest, err := CanonicalArguments(raw)
	return digest, err
}

// CanonicalArguments returns key-sorted, duplicate-free object JSON with exact
// number spelling and its SHA-256 digest, using the app-operation-v1 codec.
func CanonicalArguments(raw json.RawMessage) (json.RawMessage, string, error) {
	value, err := DecodeArguments(raw)
	if err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", ErrInvalid
	}
	sum := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(sum[:]), nil
}

// DecodeArguments validates closed object input and preserves exact JSON numbers.
func DecodeArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(raw) > 1<<20 || !utf8.Valid(raw) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := decodeValue(d, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	if _, err = d.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}
	return object, nil
}

func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, ErrInvalid
			}
			if _, exists := object[name]; exists {
				return nil, ErrInvalid
			}
			value, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return object, nil
	case json.Delim('['):
		array := []any{}
		for d.More() {
			value, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return array, nil
	default:
		if _, delim := token.(json.Delim); delim {
			return nil, fmt.Errorf("%w: delimiter", ErrInvalid)
		}
		return token, nil
	}
}

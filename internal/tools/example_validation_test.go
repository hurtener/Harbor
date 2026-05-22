package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// noopInvoke is a minimal valid Invoke for descriptor construction.
func noopInvoke(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{}, nil
}

// TestRegister_ExampleArgsSubsetOfSchema_Accepts confirms a tool whose
// example Args keys are all declared in args_schema.properties
// registers cleanly (Phase 83b — D-144).
func TestRegister_ExampleArgsSubsetOfSchema_Accepts(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name: "search",
			ArgsSchema: json.RawMessage(
				`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}`),
			Examples: []tools.ToolExample{
				{Args: map[string]any{"query": "x"}, Tags: []string{"minimal"}},
				{Args: map[string]any{"query": "x", "limit": 5}, Tags: []string{"common"}},
			},
		},
		Invoke: noopInvoke,
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register with valid examples: %v", err)
	}
}

// TestRegister_ExampleArgKeyNotInSchema_RejectsLoudly is the
// fail-loudly acceptance criterion: an example referencing an
// undeclared key fails registration with ErrToolExampleInvalid.
func TestRegister_ExampleArgKeyNotInSchema_RejectsLoudly(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:       "search",
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			Examples: []tools.ToolExample{
				{Args: map[string]any{"querty": "typo"}, Tags: []string{"minimal"}},
			},
		},
		Invoke: noopInvoke,
	}
	err := cat.Register(d)
	if err == nil {
		t.Fatal("Register accepted an example with an undeclared arg key")
	}
	if !errors.Is(err, tools.ErrToolExampleInvalid) {
		t.Errorf("error = %v, want ErrToolExampleInvalid", err)
	}
	// The tool must NOT be registered after a rejected example.
	if _, ok := cat.Resolve("search"); ok {
		t.Error("tool was registered despite invalid example")
	}
}

// TestRegister_ExampleWithoutSchema_Accepts confirms a schema-free tool
// can carry examples — a tool that makes no shape claim cannot have an
// example contradict it.
func TestRegister_ExampleWithoutSchema_Accepts(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name: "freeform",
			Examples: []tools.ToolExample{
				{Args: map[string]any{"anything": "goes"}, Tags: []string{"minimal"}},
			},
		},
		Invoke: noopInvoke,
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register schema-free tool with examples: %v", err)
	}
}

// TestRegister_NoExamples_Accepts confirms the pre-83b bare shape (no
// examples, no schema) still registers unchanged.
func TestRegister_NoExamples_Accepts(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool:   tools.Tool{Name: "ping"},
		Invoke: noopInvoke,
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register bare tool: %v", err)
	}
}

// TestRegister_MalformedSchemaWithExample_DefersToDriver confirms a
// tool whose ArgsSchema is not valid JSON does not fail example
// validation — a malformed schema is the driver's own
// schema-compilation concern; example validation does not double-
// report it (the example simply cannot be checked against a
// non-parseable schema, so registration proceeds).
func TestRegister_MalformedSchemaWithExample_DefersToDriver(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:       "malformed",
			ArgsSchema: json.RawMessage(`{not json`),
			Examples: []tools.ToolExample{
				{Args: map[string]any{"anything": "x"}, Tags: []string{"minimal"}},
			},
		},
		Invoke: noopInvoke,
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("example validation should defer a malformed schema to the driver: %v", err)
	}
}

// TestRegister_EmptyArgsExample_Accepts confirms an example with an
// empty Args map (a no-argument call) is always valid.
func TestRegister_EmptyArgsExample_Accepts(t *testing.T) {
	cat := tools.NewCatalog()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:       "noargs",
			ArgsSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Examples: []tools.ToolExample{
				{Description: "call it", Args: nil, Tags: []string{"minimal"}},
			},
		},
		Invoke: noopInvoke,
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register no-args example: %v", err)
	}
}

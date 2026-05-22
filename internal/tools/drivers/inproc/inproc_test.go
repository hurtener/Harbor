package inproc_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/conformancetest"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

func TestInProc_Conformance(t *testing.T) {
	conformancetest.Run(t, func() tools.ToolCatalog {
		return tools.NewCatalog()
	})
}

// TestRegisterFunc_DerivesSchema_FromInputType exercises the
// reflection-based schema derivation for the common shapes.
func TestRegisterFunc_DerivesSchema_FromInputType(t *testing.T) {
	type Args struct {
		Timestamp time.Time         `json:"timestamp"`
		Metadata  map[string]string `json:"metadata"`
		Nullable  *bool             `json:"nullable,omitempty"`
		Name      string            `json:"name"`
		Tags      []string          `json:"tags"`
		Age       int               `json:"age,omitempty"`
	}
	type Out struct {
		OK bool `json:"ok"`
	}

	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[Args, Out](cat, "complex", func(ctx context.Context, in Args) (Out, error) {
		return Out{OK: true}, nil
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

	d, _ := cat.Resolve("complex")
	var schema map[string]any
	if err := json.Unmarshal(d.Tool.ArgsSchema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("expected object schema, got %v", schema["type"])
	}
	props := schema["properties"].(map[string]any)
	if props["name"].(map[string]any)["type"] != "string" {
		t.Errorf("name: expected string")
	}
	if props["age"].(map[string]any)["type"] != "integer" {
		t.Errorf("age: expected integer")
	}
	if props["tags"].(map[string]any)["type"] != "array" {
		t.Errorf("tags: expected array")
	}
	if props["metadata"].(map[string]any)["type"] != "object" {
		t.Errorf("metadata: expected object")
	}
	if props["timestamp"].(map[string]any)["type"] != "string" {
		t.Errorf("timestamp: expected string")
	}
	if props["timestamp"].(map[string]any)["format"] != "date-time" {
		t.Errorf("timestamp: expected format=date-time")
	}
	required := schema["required"].([]any)
	if len(required) != 4 {
		t.Errorf("expected 4 required fields, got %v", required)
	}
}

// TestRegisterFunc_UnsupportedType_ChannelField surfaces an
// operator-friendly error when the input type contains an unsupported
// shape.
func TestRegisterFunc_UnsupportedType_ChannelField(t *testing.T) {
	type BadArgs struct {
		Ch chan int `json:"ch"`
	}
	type Out struct{}
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[BadArgs, Out](cat, "bad", func(ctx context.Context, in BadArgs) (Out, error) {
		return Out{}, nil
	})
	if err == nil {
		t.Fatalf("expected ErrUnsupportedType, got nil")
	}
	if !errors.Is(err, inproc.ErrUnsupportedType) {
		t.Fatalf("expected ErrUnsupportedType, got: %v", err)
	}
}

// TestRegisterFunc_ValidatesArgs ensures the schema validator
// surfaces a typed ErrToolInvalidArgs when JSON is shape-wrong.
func TestRegisterFunc_ValidatesArgs(t *testing.T) {
	type Args struct {
		Required string `json:"required"`
	}
	type Out struct{}
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[Args, Out](cat, "validated", func(ctx context.Context, in Args) (Out, error) {
		return Out{}, nil
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	d, _ := cat.Resolve("validated")
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})

	_, err = d.Invoke(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected ErrToolInvalidArgs, got nil")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got: %v", err)
	}

	_, err = d.Invoke(ctx, []byte(`{"required": 123}`))
	if err == nil {
		t.Fatalf("expected type-mismatch ErrToolInvalidArgs, got nil")
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("expected ErrToolInvalidArgs, got: %v", err)
	}

	res, err := d.Invoke(ctx, []byte(`{"required":"ok"}`))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if res.Value == nil {
		t.Fatalf("expected non-nil result")
	}
}

// TestRegisterFunc_NilCatalog_Rejects guards the API contract.
func TestRegisterFunc_NilCatalog_Rejects(t *testing.T) {
	type Args struct{}
	type Out struct{}
	err := inproc.RegisterFunc[Args, Out](nil, "x", func(ctx context.Context, in Args) (Out, error) {
		return Out{}, nil
	})
	if err == nil {
		t.Fatalf("expected nil-catalog error, got nil")
	}
	if !strings.Contains(err.Error(), "catalog is nil") {
		t.Fatalf("expected 'catalog is nil', got: %v", err)
	}
}

// TestRegisterFunc_EmptyName_Rejects guards the API contract.
func TestRegisterFunc_EmptyName_Rejects(t *testing.T) {
	type Args struct{}
	type Out struct{}
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[Args, Out](cat, "", func(ctx context.Context, in Args) (Out, error) {
		return Out{}, nil
	})
	if err == nil {
		t.Fatalf("expected empty-name error, got nil")
	}
}

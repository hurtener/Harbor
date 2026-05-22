// weather_test.go — worked example of registering and invoking an
// in-process tool. Run by the `examples` CI job via
// `go test ./examples/...`.
package weather

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

// TestRegister_AndInvoke registers the example tool into a fresh
// catalog, resolves the descriptor, and invokes it — the full
// register → resolve → invoke round-trip a planner performs.
func TestRegister_AndInvoke(t *testing.T) {
	cat := tools.NewCatalog()
	if err := Register(cat); err != nil {
		t.Fatalf("Register: %v", err)
	}

	desc, ok := cat.Resolve(ToolName)
	if !ok {
		t.Fatalf("Resolve(%q): not found in catalog", ToolName)
	}

	args, err := json.Marshal(LookupArgs{City: "Lisbon"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if err := desc.Validate(args); err != nil {
		t.Fatalf("Validate: derived schema rejected valid args: %v", err)
	}

	res, err := desc.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, ok := res.Value.(LookupResult)
	if !ok {
		t.Fatalf("Invoke: result Value = %T, want LookupResult", res.Value)
	}
	if out.City != "Lisbon" {
		t.Errorf("Invoke: City = %q, want %q", out.City, "Lisbon")
	}
}

// TestRegister_RejectsDuplicate proves the fail-loud path: registering
// the same tool name twice surfaces an error (CLAUDE.md §5).
func TestRegister_RejectsDuplicate(t *testing.T) {
	cat := tools.NewCatalog()
	if err := Register(cat); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := Register(cat); err == nil {
		t.Fatal("second Register: expected duplicate-name error, got nil")
	}
}

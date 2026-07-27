package inproc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

type refArgs struct {
	Doc   artifactref.Ref `json:"doc"`
	Label string          `json:"label"`
}

type refOut struct {
	// Size is a MEASUREMENT of the resolved content. The tool
	// deliberately does not return the content itself — that is the
	// direction of travel the routing exists for.
	Size  int    `json:"size"`
	RefID string `json:"ref_id"`
	Label string `json:"label"`
}

// refToolName is the catalog name the reference-taking test tool
// registers under.
const refToolName = "ref.tool"

// registerRefTool registers a tool that reads its artifact-reference
// parameter's bytes and reports only measurements.
func registerRefTool(t *testing.T, cat tools.ToolCatalog) {
	t.Helper()
	err := inproc.RegisterFunc[refArgs, refOut](cat, refToolName,
		func(_ context.Context, in refArgs) (refOut, error) {
			body, err := in.Doc.Bytes()
			if err != nil {
				return refOut{}, err
			}
			return refOut{Size: len(body), RefID: in.Doc.ID(), Label: in.Label}, nil
		})
	if err != nil {
		t.Fatalf("RegisterFunc: %v", err)
	}
}

func tableResolver(table map[string][]byte) artifactref.Resolver {
	return artifactref.ResolverFunc(func(_ context.Context, id string) ([]byte, error) {
		b, ok := table[id]
		if !ok {
			return nil, fmt.Errorf("no artifact %q", id)
		}
		return b, nil
	})
}

// TestRegisterFunc_ArtifactRefParamDerivesAsAPlainString — the model
// reads a string, not the carrier struct. If this regressed, the model
// would be told to author an object and every call would fail schema
// validation.
func TestRegisterFunc_ArtifactRefParamDerivesAsAPlainString(t *testing.T) {
	cat := tools.NewCatalog()
	registerRefTool(t, cat)

	desc, ok := cat.Resolve(refToolName)
	if !ok {
		t.Fatal("tool did not resolve")
	}
	var doc map[string]any
	if err := json.Unmarshal(desc.Tool.ArgsSchema, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props, _ := doc["properties"].(map[string]any)
	field, _ := props["doc"].(map[string]any)
	if field == nil {
		t.Fatalf("schema has no `doc` property: %s", desc.Tool.ArgsSchema)
	}
	if field["type"] != "string" {
		t.Fatalf("doc type = %v, want string (schema: %s)", field["type"], desc.Tool.ArgsSchema)
	}
	if desc, _ := field["description"].(string); !strings.Contains(desc, "artifact reference id") {
		t.Fatalf("doc description does not tell the model to supply an id: %q", desc)
	}
	// The schema validator must accept the string form the model writes.
	if err := desc.Validate(json.RawMessage(`{"doc":"tool_abc123","label":"x"}`)); err != nil {
		t.Fatalf("string-shaped reference failed validation: %v", err)
	}
}

func TestInvoke_ResolvesTheArtifactRefAtDispatch(t *testing.T) {
	cat := tools.NewCatalog()
	registerRefTool(t, cat)
	desc, _ := cat.Resolve(refToolName)

	body := []byte(strings.Repeat("x", 4096))
	ctx := artifactref.WithResolver(context.Background(),
		tableResolver(map[string][]byte{"tool_abc123": body}))

	res, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"tool_abc123","label":"L"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out, ok := res.Value.(refOut)
	if !ok {
		t.Fatalf("result value = %T, want refOut", res.Value)
	}
	if out.Size != len(body) {
		t.Fatalf("Size = %d, want %d", out.Size, len(body))
	}
	if out.RefID != "tool_abc123" {
		t.Fatalf("RefID = %q, want tool_abc123", out.RefID)
	}
}

// TestInvoke_TheArgumentJSONIsNotRewritten — the substitution is the
// runtime's and it ends at the tool boundary. The args the runtime
// records, renders and replays keep carrying the id the model authored.
func TestInvoke_TheArgumentJSONIsNotRewritten(t *testing.T) {
	cat := tools.NewCatalog()
	registerRefTool(t, cat)
	desc, _ := cat.Resolve(refToolName)

	const marker = "RESOLVED-CONTENT-MARKER"
	ctx := artifactref.WithResolver(context.Background(),
		tableResolver(map[string][]byte{"tool_abc123": []byte(marker)}))

	args := json.RawMessage(`{"doc":"tool_abc123","label":"L"}`)
	before := string(args)
	res, err := desc.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(args) != before {
		t.Fatalf("the argument JSON was rewritten in place: %s", args)
	}
	// And the RESULT the runtime turns into an observation carries no
	// resolved content either.
	encoded, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(encoded), marker) {
		t.Fatalf("the tool result leaked the resolved value: %s", encoded)
	}
}

func TestInvoke_FailsLoudlyWithoutASeatedResolver(t *testing.T) {
	cat := tools.NewCatalog()
	registerRefTool(t, cat)
	desc, _ := cat.Resolve(refToolName)

	_, err := desc.Invoke(context.Background(), json.RawMessage(`{"doc":"tool_abc123","label":"L"}`))
	if err == nil {
		t.Fatal("an unresolvable reference invoked successfully; want a loud failure")
	}
	if !errors.Is(err, artifactref.ErrNoResolver) {
		t.Fatalf("err = %v, want it to wrap ErrNoResolver", err)
	}
	if !errors.Is(err, tools.ErrToolInvalidArgs) {
		t.Fatalf("err = %v, want it classified as invalid args", err)
	}
}

func TestInvoke_FailsLoudlyWhenTheReferenceDoesNotResolve(t *testing.T) {
	cat := tools.NewCatalog()
	registerRefTool(t, cat)
	desc, _ := cat.Resolve(refToolName)

	ctx := artifactref.WithResolver(context.Background(), tableResolver(nil))
	_, err := desc.Invoke(ctx, json.RawMessage(`{"doc":"tool_missing","label":"L"}`))
	if err == nil {
		t.Fatal("a missing reference invoked successfully; want a loud failure")
	}
	if !strings.Contains(err.Error(), "tool_missing") {
		t.Fatalf("err = %v, want it to name the reference id", err)
	}
}

// TestInvoke_AToolWithoutAReferenceParamNeedsNoResolver — the routing is
// opt-in through the declared field type, so every existing tool keeps
// working with nothing seated.
func TestInvoke_AToolWithoutAReferenceParamNeedsNoResolver(t *testing.T) {
	type plainArgs struct {
		City string `json:"city"`
	}
	type plainOut struct {
		Echo string `json:"echo"`
	}
	cat := tools.NewCatalog()
	if err := inproc.RegisterFunc[plainArgs, plainOut](cat, "plain.tool",
		func(_ context.Context, in plainArgs) (plainOut, error) {
			return plainOut{Echo: in.City}, nil
		}); err != nil {
		t.Fatalf("RegisterFunc: %v", err)
	}
	desc, _ := cat.Resolve("plain.tool")
	if _, err := desc.Invoke(context.Background(), json.RawMessage(`{"city":"Lisbon"}`)); err != nil {
		t.Fatalf("a reference-free tool failed without a resolver: %v", err)
	}
}

// TestInvoke_ConcurrentReuse_NoContextBleedAcrossRuns is the D-025
// concurrent-reuse gate for the routing seam. ONE registered descriptor
// (the compiled artifact) is invoked by N goroutines, each carrying its
// OWN ctx-seated resolver over its OWN artifact table. A resolver read
// from the descriptor rather than from the call's ctx, or a Ref bound on
// shared state rather than on the per-invocation argument value, shows
// up here as a run reading another run's bytes.
func TestInvoke_ConcurrentReuse_NoContextBleedAcrossRuns(t *testing.T) {
	const n = 128

	cat := tools.NewCatalog()
	registerRefTool(t, cat)
	desc, _ := cat.Resolve(refToolName)

	var wg sync.WaitGroup
	errs := make([]error, n)
	sizes := make([]int, n)
	ids := make([]string, n)

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("tool_run%03d", i)
			// Each run's content has a distinct, run-derived length, so a
			// bleed is visible as a size mismatch rather than needing a
			// byte compare.
			body := []byte(strings.Repeat("y", i+1))
			ctx := artifactref.WithResolver(context.Background(),
				tableResolver(map[string][]byte{id: body}))
			args := json.RawMessage(fmt.Sprintf(`{"doc":%q,"label":"run%03d"}`, id, i))
			res, err := desc.Invoke(ctx, args)
			if err != nil {
				errs[i] = err
				return
			}
			out, ok := res.Value.(refOut)
			if !ok {
				errs[i] = fmt.Errorf("result value = %T", res.Value)
				return
			}
			sizes[i] = out.Size
			ids[i] = out.RefID
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		if sizes[i] != i+1 {
			t.Fatalf("run %d resolved %d bytes, want %d — a run read another run's artifact", i, sizes[i], i+1)
		}
		if want := fmt.Sprintf("tool_run%03d", i); ids[i] != want {
			t.Fatalf("run %d resolved ref %q, want %q", i, ids[i], want)
		}
	}
}

// TestInvoke_ConcurrentReuse_CancellingOneRunDoesNotAffectAnother — a
// per-call cancellation must not reach a sibling invocation of the same
// shared descriptor.
func TestInvoke_ConcurrentReuse_CancellingOneRunDoesNotAffectAnother(t *testing.T) {
	cat := tools.NewCatalog()
	registerRefTool(t, cat)
	desc, _ := cat.Resolve(refToolName)

	table := map[string][]byte{"tool_live": []byte("alive")}
	liveCtx := artifactref.WithResolver(context.Background(), tableResolver(table))

	cancelledCtx, cancel := context.WithCancel(
		artifactref.WithResolver(context.Background(), tableResolver(table)))
	cancel()

	if _, err := desc.Invoke(cancelledCtx, json.RawMessage(`{"doc":"tool_live","label":"c"}`)); err == nil {
		t.Fatal("a cancelled invocation succeeded")
	}
	if _, err := desc.Invoke(liveCtx, json.RawMessage(`{"doc":"tool_live","label":"l"}`)); err != nil {
		t.Fatalf("cancelling one run affected another: %v", err)
	}
}

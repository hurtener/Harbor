package builtin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
)

// Exercise the registered descriptor, not only artifactList: schema validation
// rejects malformed model inputs before the handler can normalize anything.
func TestArtifactList_CatalogValidationAndDispatch(t *testing.T) {
	store := artifactFetchTestStore(t)
	cat := tools.NewCatalog()
	if err := RegisterWith(RegistryContext{Catalog: cat, ArtifactStore: store}, []string{"artifact_list"}); err != nil {
		t.Fatal(err)
	}
	desc, ok := cat.Resolve("artifact_list")
	if !ok || desc.Validate == nil {
		t.Fatal("artifact_list descriptor or validator missing")
	}
	ctx := artifactFetchTestCtx(t, "t", "u", "s", "r")
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range 55 {
		putToolResult(t, store, "s", "web_search", fmt.Sprintf("result-%02d.txt", i), "text/plain", fmt.Sprint(i), base.Add(time.Duration(i)*time.Minute))
	}
	putToolResult(t, store, "other-session", "web_search", "foreign.txt", "text/plain", "foreign", base)

	for _, tc := range []struct {
		name string
		args string
		want int
	}{
		{"omitted", `{}`, 20},
		{"empty_strings", `{"source":"","tool":"","filename":"","cursor":""}`, 20},
		{"blank_strings", `{"source":" \t","tool":"\n","filename":" ","cursor":"\t "}`, 20},
		{"zero", `{"limit":0}`, 20},
		{"negative", `{"limit":-1}`, 20},
		{"one", `{"limit":1}`, 1},
		{"ceiling", `{"limit":50}`, 50},
		{"clamped", `{"limit":1000000}`, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := json.RawMessage(tc.args)
			if err := desc.Validate(args); err != nil {
				t.Fatalf("catalog validation rejected supported args: %v", err)
			}
			result, err := desc.Invoke(ctx, args)
			if err != nil {
				t.Fatalf("dispatch failed: %v", err)
			}
			out, ok := result.Value.(ArtifactListOut)
			if !ok || out.Error != "" || out.Total != 55 || len(out.Artifacts) != tc.want || out.NextCursor == "" {
				t.Fatalf("dispatch result = %+v, want %d entries of 55", result.Value, tc.want)
			}
			continued, err := json.Marshal(ArtifactListArgs{Cursor: out.NextCursor, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if err := desc.Validate(continued); err != nil {
				t.Fatalf("continuation rejected: %v", err)
			}
			second, err := desc.Invoke(ctx, continued)
			if err != nil {
				t.Fatal(err)
			}
			rest := second.Value.(ArtifactListOut)
			if rest.Error != "" || len(rest.Artifacts) != min(50, 55-tc.want) {
				t.Fatalf("continuation = %+v", rest)
			}
			seen := make(map[string]bool, len(out.Artifacts))
			for _, entry := range out.Artifacts {
				seen[entry.Ref] = true
			}
			for _, entry := range rest.Artifacts {
				if seen[entry.Ref] || entry.Filename == "foreign.txt" {
					t.Fatalf("continuation leaked or repeated entry: %+v", entry)
				}
			}
		})
	}

	for _, example := range desc.Tool.Examples {
		args, err := json.Marshal(example.Args)
		if err != nil {
			t.Fatal(err)
		}
		if err := desc.Validate(args); err != nil {
			t.Fatalf("advertised example rejected: %v", err)
		}
		if _, err := desc.Invoke(ctx, args); err != nil {
			t.Fatalf("advertised example cannot dispatch: %v", err)
		}
	}

	for _, tc := range []struct {
		args  string
		field string
	}{
		{`{"source":null}`, "source"},
		{`{"source":[]}`, "source"},
		{`{"tool":42}`, "tool"},
		{`{"filename":false}`, "filename"},
		{`{"cursor":1}`, "cursor"},
		{`{"limit":"20"}`, "limit"},
		{`{"limit":1.5}`, "limit"},
		{`{"page":1}`, "page"},
	} {
		t.Run("invalid_"+tc.field+"_"+tc.args, func(t *testing.T) {
			args := json.RawMessage(tc.args)
			if err := desc.Validate(args); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("validation must name %s: %v", tc.field, err)
			}
			if _, err := desc.Invoke(ctx, args); !errors.Is(err, tools.ErrToolInvalidArgs) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("dispatch must reject %s as invalid args: %v", tc.field, err)
			}
		})
	}

	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"source":"all"}`, `source must be "user_upload", "tool", or "other"`},
		{`{"cursor":"1"}`, "cursor must be omitted or copied exactly"},
	} {
		if err := desc.Validate(json.RawMessage(tc.args)); err != nil {
			t.Fatalf("semantic input rejected by structural validator: %v", err)
		}
		result, err := desc.Invoke(ctx, json.RawMessage(tc.args))
		if err != nil {
			t.Fatal(err)
		}
		out := result.Value.(ArtifactListOut)
		if !strings.Contains(out.Error, tc.want) || len(out.Artifacts) != 0 {
			t.Fatalf("semantic error must guide field repair: %+v", out)
		}
	}
}

func TestArtifactList_CatalogEmptyListingsReturnArray(t *testing.T) {
	store := artifactFetchTestStore(t)
	cat := tools.NewCatalog()
	if err := RegisterWith(RegistryContext{Catalog: cat, ArtifactStore: store}, []string{"artifact_list"}); err != nil {
		t.Fatal(err)
	}
	desc, ok := cat.Resolve("artifact_list")
	if !ok || desc.Validate == nil {
		t.Fatal("artifact_list descriptor or validator missing")
	}
	putToolResult(t, store, "s", "web_search", "report.txt", "text/plain", "result", time.Now())
	for _, tc := range []struct {
		name    string
		session string
		args    string
	}{
		{"empty_session", "empty-session", `{}`},
		{"source_no_match", "s", `{"source":"user_upload"}`},
		{"tool_no_match", "s", `{"tool":"knowledge_read"}`},
		{"filename_no_match", "s", `{"filename":"absent.pdf"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := json.RawMessage(tc.args)
			if err := desc.Validate(args); err != nil {
				t.Fatalf("catalog rejected supported args: %v", err)
			}
			ctx := artifactFetchTestCtx(t, "t", "u", tc.session, "r")
			result, err := desc.Invoke(ctx, args)
			if err != nil {
				t.Fatalf("empty listing rejected at dispatch: %v", err)
			}
			out, ok := result.Value.(ArtifactListOut)
			if !ok || out.Error != "" || out.Total != 0 || out.Artifacts == nil || len(out.Artifacts) != 0 || out.NextCursor != "" {
				t.Fatalf("empty listing must contain []: %+v", result.Value)
			}
		})
	}
}

func TestArtifactList_CatalogConcurrentBlankInputsStayIsolated(t *testing.T) {
	store := artifactFetchTestStore(t)
	cat := tools.NewCatalog()
	if err := RegisterWith(RegistryContext{Catalog: cat, ArtifactStore: store}, []string{"artifact_list"}); err != nil {
		t.Fatal(err)
	}
	desc, ok := cat.Resolve("artifact_list")
	if !ok {
		t.Fatal("artifact_list descriptor missing")
	}
	args := json.RawMessage(`{"source":" ","tool":" ","cursor":" ","limit":100}`)
	var wg sync.WaitGroup
	for i := range 128 {
		session := fmt.Sprint("s-", i)
		putToolResult(t, store, session, "web_search", session+".txt", "text/plain", session, time.Now())
		ctx := artifactFetchTestCtx(t, "t", "u", session, "r")
		wg.Go(func() {
			if err := desc.Validate(args); err != nil {
				t.Errorf("session %s validation: %v", session, err)
				return
			}
			result, err := desc.Invoke(ctx, args)
			if err != nil {
				t.Errorf("session %s dispatch: %v", session, err)
				return
			}
			out := result.Value.(ArtifactListOut)
			if out.Error != "" || out.Total != 1 || len(out.Artifacts) != 1 || out.Artifacts[0].Filename != session+".txt" {
				t.Errorf("session %s listing crossed scope: %+v", session, out)
			}
		})
	}
	wg.Wait()
}

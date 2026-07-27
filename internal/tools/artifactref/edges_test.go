package artifactref_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

func TestWithResolver_NilResolverIsANoOp(t *testing.T) {
	base := context.Background()
	if got := artifactref.WithResolver(base, nil); got != base {
		t.Fatal("seating a nil resolver produced a new context")
	}
	if _, ok := artifactref.ResolverFrom(base); ok {
		t.Fatal("ResolverFrom found a resolver on a bare context")
	}
}

func TestSubstitute_ANilPointerFieldIsSkipped(t *testing.T) {
	type args struct {
		Doc *artifactref.Ref `json:"doc,omitempty"`
	}
	var a args
	if err := json.Unmarshal([]byte(`{}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// No resolver: a nil pointer must not need one.
	if err := artifactref.Substitute(context.Background(), &a); err != nil {
		t.Fatalf("Substitute over a nil pointer field: %v", err)
	}
}

// TestSubstitute_IsIdempotent — the policy shell may re-run the decode +
// bind for a retried attempt; re-binding an already-bound reference must
// not call the resolver a second time or change the value.
func TestSubstitute_IsIdempotent(t *testing.T) {
	calls := 0
	ctx := artifactref.WithResolver(context.Background(),
		artifactref.ResolverFunc(func(_ context.Context, _ string) ([]byte, error) {
			calls++
			return []byte("once"), nil
		}))
	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"tool_abc123","max_words":1}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for range 3 {
		if err := artifactref.Substitute(ctx, &a); err != nil {
			t.Fatalf("Substitute: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1", calls)
	}
	if got, _ := a.Doc.Bytes(); string(got) != "once" {
		t.Fatalf("Bytes = %q, want once", got)
	}
}

// TestSubstitute_FailsLoudlyOnACyclicArgumentGraph — a pathological
// argument shape must fail rather than recurse without end. (The schema
// deriver rejects cyclic TYPES at registration, so this can only arrive
// through a hand-built value; the walk still bounds itself.)
func TestSubstitute_FailsLoudlyOnACyclicArgumentGraph(t *testing.T) {
	type node struct {
		Ref  artifactref.Ref
		Next *node
	}
	head := &node{Ref: artifactref.NewRef("x")}
	cur := head
	for range 40 {
		cur.Next = &node{Ref: artifactref.NewRef("x")}
		cur = cur.Next
	}
	ctx := artifactref.WithResolver(context.Background(),
		artifactref.ResolverFunc(func(_ context.Context, _ string) ([]byte, error) {
			return []byte("v"), nil
		}))
	err := artifactref.Substitute(ctx, head)
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("err = %v, want a depth-bound failure", err)
	}
}

// TestSubstitute_AMapValueIsWrittenBack — map elements are not
// addressable, so a bind that forgot the write-back would silently leave
// the reference unresolved.
func TestSubstitute_AMapValueIsWrittenBack(t *testing.T) {
	type args struct {
		Docs map[string]artifactref.Ref `json:"docs"`
	}
	ctx := artifactref.WithResolver(context.Background(),
		artifactref.ResolverFunc(func(_ context.Context, id string) ([]byte, error) {
			return []byte("body-" + id), nil
		}))
	var a args
	if err := json.Unmarshal([]byte(`{"docs":{"first":"id1","second":"id2"}}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := artifactref.Substitute(ctx, &a); err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	for key, wantID := range map[string]string{"first": "id1", "second": "id2"} {
		got, err := a.Docs[key].Bytes()
		if err != nil {
			t.Fatalf("docs[%s]: %v", key, err)
		}
		if string(got) != "body-"+wantID {
			t.Fatalf("docs[%s] = %q, want body-%s", key, got, wantID)
		}
	}
}

// TestSubstitute_ARefInsideAnInterfaceIsNotReachable — a JSON decode
// cannot produce a Ref inside an `any`, and reflection cannot write
// through one. The walk and TypeContainsRef must AGREE on that, or the
// walk would try to bind something it cannot.
func TestSubstitute_ARefInsideAnInterfaceIsNotReachable(t *testing.T) {
	type args struct {
		Anything any `json:"anything"`
	}
	a := args{Anything: artifactref.NewRef("id1")}
	ctx := artifactref.WithResolver(context.Background(),
		artifactref.ResolverFunc(func(_ context.Context, _ string) ([]byte, error) {
			t.Fatal("the resolver was called for a reference inside an interface")
			return nil, nil
		}))
	if err := artifactref.Substitute(ctx, &a); err != nil {
		t.Fatalf("Substitute: %v", err)
	}
}

func TestViolation_StringNamesTheFileAndLine(t *testing.T) {
	withLine := artifactref.Violation{File: "a/b.go", Line: 12, Kind: "k", Detail: "d"}
	if got := withLine.String(); got != "a/b.go:12: k: d" {
		t.Fatalf("String = %q", got)
	}
	listLevel := artifactref.Violation{File: "a/b.go", Kind: "k", Detail: "d"}
	if got := listLevel.String(); got != "a/b.go: k: d" {
		t.Fatalf("String = %q", got)
	}
}

// TestScan_BlankAndDotImportsCannotCall — neither form can call the
// writer under a qualifier, so neither is a substitution site.
func TestScan_BlankAndDotImportsCannotCall(t *testing.T) {
	for name, spec := range map[string]string{
		"blank": `_ "github.com/hurtener/Harbor/internal/tools/artifactref"`,
		"dot":   `. "github.com/hurtener/Harbor/internal/tools/artifactref"`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := "package worker\n\nimport (\n\t" + spec + "\n)\n"
			if err := os.WriteFile(filepath.Join(dir, "imp.go"), []byte(src), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			violations, files, err := artifactref.ScanSubstitutionSites(dir, nil)
			if err != nil {
				t.Fatalf("ScanSubstitutionSites: %v", err)
			}
			if files != 1 {
				t.Fatalf("read %d files, want 1", files)
			}
			if len(violations) != 0 {
				t.Fatalf("violations = %v", violations)
			}
		})
	}
}

// TestScan_ViolationsAreOrderedDeterministically — a failure message
// that reorders between runs is a failure message nobody trusts.
func TestScan_ViolationsAreOrderedDeterministically(t *testing.T) {
	dir := t.TempDir()
	src := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	for _, name := range []string{"c.go", "a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	var first []string
	for range 3 {
		violations, _, err := artifactref.ScanSubstitutionSites(dir, nil)
		if err != nil {
			t.Fatalf("ScanSubstitutionSites: %v", err)
		}
		got := make([]string, 0, len(violations))
		for _, v := range violations {
			got = append(got, v.File)
		}
		if first == nil {
			first = got
			if strings.Join(got, ",") != "a.go,b.go,c.go" {
				t.Fatalf("order = %v, want a.go,b.go,c.go", got)
			}
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("order changed between runs: %v vs %v", first, got)
		}
	}
}

func TestScan_AParseErrorIsSurfaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package !!!"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, _, err := artifactref.ScanSubstitutionSites(dir, nil); err == nil {
		t.Fatal("a file the scan cannot parse was skipped silently")
	}
}

func TestRef_MarshalRoundTripsThroughTheID(t *testing.T) {
	r := artifactref.NewRef("tool_abc123")
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"tool_abc123"` {
		t.Fatalf("marshal = %s, want a bare id string", b)
	}
	var back artifactref.Ref
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ID() != r.ID() {
		t.Fatalf("round trip = %q, want %q", back.ID(), r.ID())
	}
	if back.Resolved() {
		t.Fatal("a round-tripped reference reports itself resolved")
	}
	if _, err := back.Bytes(); !errors.Is(err, artifactref.ErrUnresolved) {
		t.Fatalf("Bytes err = %v, want ErrUnresolved", err)
	}
}

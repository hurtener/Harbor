package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestConfigDoc_AllFieldsDocumented walks the `Config` struct,
// collects every yaml leaf path (the same set `walkLeaves` reaches
// from the env-override + WithOverrides paths), and asserts every
// path has a corresponding `### <path>` heading in `docs/CONFIG.md`.
// Fails the build when a new config field lands without an entry —
// the §4.4 mirror-pattern read one layer over to documentation.
//
// Phase 83n / D-153. The test is deliberately permissive about
// FORMAT — any line that starts with `### <path>` (optionally
// followed by trailing text) satisfies the assertion. Authors stay
// free to format the body however they like as long as the heading
// exists.
func TestConfigDoc_AllFieldsDocumented(t *testing.T) {
	t.Parallel()

	docPath := configDocPath(t)
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v (the file must exist for the doc-drift gate to run)", docPath, err)
	}

	// Collect every leaf yaml path on Config.
	paths := collectConfigLeafPaths()

	// Collect every `### path` heading in CONFIG.md.
	documented := collectDocHeadings(string(docBytes))

	var missing []string
	for _, p := range paths {
		if _, ok := documented[p]; !ok {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("docs/CONFIG.md is missing `### <path>` headings for these yaml leaves (add one section per missing path, then re-run):\n  - %s",
			strings.Join(missing, "\n  - "))
	}
}

// configDocPath resolves the repository-root-relative path to
// docs/CONFIG.md from the package's source file location. Robust
// against the test being run from a worktree or a CI cache.
func configDocPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) returned !ok — cannot resolve docs/CONFIG.md path")
	}
	// internal/config/doc_drift_test.go → docs/CONFIG.md
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "docs", "CONFIG.md")
}

// collectConfigLeafPaths walks Config{} and returns every yaml path
// for a leaf (non-struct) field, sorted lexicographically. Slices /
// maps / pointers are leaves — their per-element doc lives one
// heading down at the struct's discretion (the test is permissive
// about that), but the slice / map itself MUST appear.
func collectConfigLeafPaths() []string {
	out := []string{}
	collectLeavesInto(reflect.ValueOf(Config{}), nil, &out)
	sort.Strings(out)
	return out
}

// collectLeavesInto is a local mirror of `walkLeaves`'s shape — the
// test does NOT use `walkLeaves` so a refactor of the loader
// machinery cannot accidentally narrow the doc-drift gate.
func collectLeavesInto(v reflect.Value, prefix []string, out *[]string) {
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		// Build a child path without aliasing `prefix`.
		path := make([]string, 0, len(prefix)+1)
		path = append(path, prefix...)
		path = append(path, name)
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct {
			// time.Duration is an int64 alias — treat it as a leaf.
			if isDurationKind(fv) {
				*out = append(*out, strings.Join(path, "."))
				continue
			}
			collectLeavesInto(fv, path, out)
			continue
		}
		*out = append(*out, strings.Join(path, "."))
	}
}

// isDurationKind returns true when v's type is time.Duration.
// Resolves via the type's String() representation so the test does
// not need a `time` import for one comparison.
func isDurationKind(v reflect.Value) bool {
	return v.Type().String() == "time.Duration"
}

// collectDocHeadings parses `docs/CONFIG.md` and returns the set of
// paths exposed via `### <path>` headings (trailing text after the
// path is allowed).
func collectDocHeadings(doc string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "### "))
		if rest == "" {
			continue
		}
		// The path is the first whitespace-delimited token. This lets
		// a heading like `### tools.built_in (V1.1: clock.now, text.echo)`
		// still match the path `tools.built_in`.
		path := strings.Fields(rest)[0]
		out[path] = struct{}{}
	}
	return out
}

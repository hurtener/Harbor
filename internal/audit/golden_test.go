package audit_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
)

// TestGoldenFiles drives the V1 rule pipeline against every
// testdata/golden/<name>.json fixture and compares the redacted
// result to <name>.expected.json. Failures dump the diff so a
// contributor can refresh fixtures with HARBOR_UPDATE_GOLDEN=1.
//
// To regenerate after a deliberate rule change:
//
//	HARBOR_UPDATE_GOLDEN=1 go test ./internal/audit/...
//
// (only set this when you've reviewed the diff and it's the
// intended new behavior.)
func TestGoldenFiles(t *testing.T) {
	driver := patterns.New()
	dir := "testdata/golden"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".expected.json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		t.Run(stem, func(t *testing.T) {
			input := filepath.Join(dir, name)
			expected := filepath.Join(dir, stem+".expected.json")

			rawIn, err := os.ReadFile(input)
			if err != nil {
				t.Fatalf("read %s: %v", input, err)
			}
			var payload any
			if err := json.Unmarshal(rawIn, &payload); err != nil {
				t.Fatalf("unmarshal %s: %v", input, err)
			}
			got, err := driver.Redact(context.Background(), payload)
			if err != nil {
				t.Fatalf("Redact(%s): %v", stem, err)
			}
			gotBytes, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}

			if os.Getenv("HARBOR_UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(expected, append(gotBytes, '\n'), 0o644); err != nil {
					t.Fatalf("update golden %s: %v", expected, err)
				}
				t.Logf("updated golden %s", expected)
				return
			}

			rawExp, err := os.ReadFile(expected)
			if err != nil {
				t.Fatalf("read expected %s: %v\n(set HARBOR_UPDATE_GOLDEN=1 to seed)", expected, err)
			}
			var want any
			if err := json.Unmarshal(rawExp, &want); err != nil {
				t.Fatalf("unmarshal expected %s: %v", expected, err)
			}
			wantBytes, err := json.MarshalIndent(want, "", "  ")
			if err != nil {
				t.Fatalf("marshal want: %v", err)
			}

			if string(gotBytes) != string(wantBytes) {
				t.Errorf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s",
					stem, gotBytes, wantBytes)
			}
		})
	}
}

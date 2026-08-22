package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runLegacyRepairCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestEventsRepairLegacyHeads_DefaultInspectDoesNotBootRuntime(t *testing.T) {
	stdout, stderr, err := runLegacyRepairCLI(t, "--json", "events", "repair-legacy-heads", "--driver", "inmem")
	if err != nil {
		t.Fatalf("inspect error: %v; stderr=%s", err, stderr)
	}
	if stderr != "" {
		t.Fatalf("inspect stderr = %q", stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("inspect output is not JSON: %v; output=%s", err, stdout)
	}
	if report["mode"] != "inspect" || report["heads_scanned"] != float64(0) {
		t.Fatalf("inspect report = %v", report)
	}
}

func TestEventsRepairLegacyHeads_ApplyRefusesBeforeOpeningStore(t *testing.T) {
	stdout, stderr, err := runLegacyRepairCLI(t, "events", "repair-legacy-heads", "--mode", "apply", "--driver", "postgres", "--dsn", "postgres://u:p@example:6432/harbor")
	if err == nil {
		t.Fatal("apply unexpectedly succeeded without acknowledgement")
	}
	if stdout != "" {
		t.Fatalf("apply wrote stdout before refusal: %q", stdout)
	}
	if !strings.Contains(stderr, "--freeze-ack") {
		t.Fatalf("ack refusal missing from stderr: %s", stderr)
	}
}

func TestEventsRepairLegacyHeads_ApplyRejectsPooledDSNAfterAcknowledgement(t *testing.T) {
	_, stderr, err := runLegacyRepairCLI(t, "events", "repair-legacy-heads", "--mode", "apply", "--freeze-ack", "--driver", "postgres", "--dsn", "postgres://u:p@example:6432/harbor")
	if err == nil {
		t.Fatal("apply unexpectedly accepted PgBouncer DSN")
	}
	if !strings.Contains(stderr, "5432") || !strings.Contains(stderr, "6432") {
		t.Fatalf("pooled DSN diagnostic missing: %s", stderr)
	}
}

func TestEventsRepairLegacyHeads_SQLiteInspect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repair.db")
	stdout, stderr, err := runLegacyRepairCLI(t, "--json", "events", "repair-legacy-heads", "--driver", "sqlite", "--dsn", path)
	if err != nil {
		t.Fatalf("sqlite inspect error: %v; stderr=%s", err, stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sqlite store was not opened: %v", err)
	}
	if !strings.Contains(stdout, "\"mode\": \"inspect\"") {
		t.Fatalf("sqlite inspect output = %s", stdout)
	}
}

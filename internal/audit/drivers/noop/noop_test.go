package noop_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/noop"
	"github.com/hurtener/Harbor/internal/config"
)

func TestNoop_PassesThrough(t *testing.T) {
	d := noop.New()
	in := map[string]any{
		"api_key":  "would-normally-be-redacted",
		"password": "hunter2",
	}
	out, err := d.Redact(context.Background(), in)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	if m["api_key"] != "would-normally-be-redacted" {
		t.Errorf("noop redacted api_key: %v", m["api_key"])
	}
	if m["password"] != "hunter2" {
		t.Errorf("noop redacted password: %v", m["password"])
	}
}

func TestNoop_RegisteredViaInit(t *testing.T) {
	r, err := audit.OpenDriver("noop", config.AuditConfig{})
	if err != nil {
		t.Fatalf("OpenDriver(noop): %v", err)
	}
	if r == nil {
		t.Fatal("OpenDriver(noop) returned nil")
	}
}

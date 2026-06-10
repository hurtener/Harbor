package telemetry_test

// Tests for the Slog() bridge (Wave C checkpoint audit) — the
// *slog.Logger adapter the assembly threads into subsystems that
// accept a plain *slog.Logger. The load-bearing guarantees: records
// flow through the mandatory redactor, ctx identity is stamped,
// Error-level records fire the paired runtime.error bus emit, and the
// Logger's log_level filtering applies.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
)

func TestSlogBridge_RoutesThroughRedactor(t *testing.T) {
	l, buf := newLogger(t)
	sl := l.Slog()
	sl.Info("request seen", "trace_note", "saw header Bearer xxx.yyy.zzz come through")
	out := buf.String()
	if strings.Contains(out, "xxx.yyy.zzz") {
		t.Fatalf("bearer credential leaked through the slog bridge: %s", out)
	}
	if !strings.Contains(out, "Bearer ***") {
		t.Errorf("bearer redaction missing: %s", out)
	}
	if !strings.Contains(out, "request seen") {
		t.Errorf("record message missing: %s", out)
	}
}

func TestSlogBridge_StampsCtxIdentity(t *testing.T) {
	l, buf := newLogger(t)
	sl := l.Slog()
	id := identity.Identity{TenantID: "t-slog", UserID: "u-slog", SessionID: "s-slog"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	sl.InfoContext(ctx, "scoped line")
	out := buf.String()
	for _, want := range []string{"t-slog", "u-slog", "s-slog"} {
		if !strings.Contains(out, want) {
			t.Errorf("identity attribute %q missing: %s", want, out)
		}
	}
}

func TestSlogBridge_ErrorFiresBusEmitter(t *testing.T) {
	red := auditpatterns.New()
	buf := &lockedBuf{}
	em := &recordingEmitter{}
	l, err := telemetry.New(validCfg(), red,
		telemetry.WithWriter(buf), telemetry.WithBusEmitter(em))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sl := l.Slog()

	sl.Warn("warn does not emit")
	em.mu.Lock()
	got := em.calls
	em.mu.Unlock()
	if got != 0 {
		t.Fatalf("Warn fired the bus emitter %d times, want 0", got)
	}

	sl.Error("subsystem exploded", "detail", "boom")
	em.mu.Lock()
	got = em.calls
	em.mu.Unlock()
	if got != 1 {
		t.Fatalf("Error fired the bus emitter %d times, want 1 (the paired runtime.error)", got)
	}
	if !strings.Contains(buf.String(), "subsystem exploded") {
		t.Errorf("error record missing from the slog output: %s", buf.String())
	}
}

func TestSlogBridge_HonoursLogLevel(t *testing.T) {
	red := auditpatterns.New()
	buf := &lockedBuf{}
	cfg := validCfg()
	cfg.LogLevel = "warn"
	l, err := telemetry.New(cfg, red, telemetry.WithWriter(buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sl := l.Slog()
	if sl.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("bridge reports Info enabled under log_level=warn")
	}
	sl.Info("must be filtered")
	sl.Warn("must pass")
	out := buf.String()
	if strings.Contains(out, "must be filtered") {
		t.Errorf("info record passed a warn-level Logger: %s", out)
	}
	if !strings.Contains(out, "must pass") {
		t.Errorf("warn record filtered: %s", out)
	}
}

func TestSlogBridge_WithAttrsAndGroupDerive(t *testing.T) {
	l, buf := newLogger(t)
	base := l.Slog()
	child := base.With("component", "sweeper").WithGroup("reap")
	child.Info("pass done", "count", 3)
	out := buf.String()
	if !strings.Contains(out, "sweeper") {
		t.Errorf("bound attr missing: %s", out)
	}
	if !strings.Contains(out, "reap") || !strings.Contains(out, "count") {
		t.Errorf("grouped attr missing: %s", out)
	}
	// The base logger is unchanged (derive semantics, D-025).
	buf2len := len(buf.String())
	base.Info("plain line")
	if strings.Contains(buf.String()[buf2len:], "sweeper") {
		t.Error("derived attrs bled back into the base logger")
	}
}

package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// lockedBuf is a goroutine-safe wrapper around bytes.Buffer used by
// the concurrent-reuse test so the slog handler's writes don't race.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuf) Lines() []string {
	s := b.String()
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func validCfg() config.TelemetryConfig {
	return config.TelemetryConfig{
		LogFormat:   "json",
		LogLevel:    "debug",
		ServiceName: "harbor-test",
	}
}

func newLogger(t *testing.T, opts ...telemetry.Option) (*telemetry.Logger, *lockedBuf) {
	t.Helper()
	red := auditpatterns.New()
	buf := &lockedBuf{}
	all := append([]telemetry.Option{telemetry.WithWriter(buf)}, opts...)
	l, err := telemetry.New(validCfg(), red, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, buf
}

func TestNew_Happy(t *testing.T) {
	l, _ := newLogger(t)
	if l == nil {
		t.Fatal("New returned nil")
	}
}

func TestNew_NilRedactorErrors(t *testing.T) {
	_, err := telemetry.New(validCfg(), nil)
	if err == nil {
		t.Fatal("New with nil redactor returned nil error")
	}
	if !errors.Is(err, telemetry.ErrRedactorMissing) {
		t.Errorf("err=%v, want errors.Is ErrRedactorMissing", err)
	}
	if !errors.Is(err, audit.ErrRedactorMissing) {
		t.Errorf("err=%v, want errors.Is audit.ErrRedactorMissing", err)
	}
}

func TestNew_InvalidLogFormatErrors(t *testing.T) {
	cfg := validCfg()
	cfg.LogFormat = "csv"
	_, err := telemetry.New(cfg, auditpatterns.New())
	if err == nil {
		t.Fatal("New with invalid log_format returned nil error")
	}
	if !errors.Is(err, telemetry.ErrLoggerNotConfigured) {
		t.Errorf("err=%v, want errors.Is ErrLoggerNotConfigured", err)
	}
}

func TestNew_InvalidLogLevelErrors(t *testing.T) {
	cfg := validCfg()
	cfg.LogLevel = "trace"
	_, err := telemetry.New(cfg, auditpatterns.New())
	if err == nil {
		t.Fatal("New with invalid log_level returned nil error")
	}
	if !errors.Is(err, telemetry.ErrLoggerNotConfigured) {
		t.Errorf("err=%v, want errors.Is ErrLoggerNotConfigured", err)
	}
}

func TestHandler_JSONShape(t *testing.T) {
	l, buf := newLogger(t)
	l.Info(context.Background(), "hello world")
	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, line)
	}
	if rec["msg"] != "hello world" {
		t.Errorf("msg=%v, want hello world", rec["msg"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level=%v, want INFO", rec["level"])
	}
}

func TestHandler_TextShape(t *testing.T) {
	cfg := validCfg()
	cfg.LogFormat = "text"
	red := auditpatterns.New()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, red, telemetry.WithWriter(buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info(context.Background(), "hello text")
	if !strings.Contains(buf.String(), `msg="hello text"`) {
		t.Errorf("text output missing expected key=value: %q", buf.String())
	}
}

func TestWithIdentity_StampsTriple(t *testing.T) {
	l, buf := newLogger(t)
	id := identity.Identity{TenantID: "T-1", UserID: "U-1", SessionID: "S-1"}
	derived := l.WithIdentity(id)
	derived.Info(context.Background(), "id-bound")
	rec := decodeOne(t, buf)
	if rec["tenant_id"] != "T-1" || rec["user_id"] != "U-1" || rec["session_id"] != "S-1" {
		t.Errorf("identity attrs missing or wrong: %+v", rec)
	}
}

func TestWithRun_StampsQuadruple(t *testing.T) {
	l, buf := newLogger(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T-2", UserID: "U-2", SessionID: "S-2"},
		RunID:    "R-1",
	}
	derived := l.WithRun(q)
	derived.Info(context.Background(), "run-bound")
	rec := decodeOne(t, buf)
	if rec["run_id"] != "R-1" {
		t.Errorf("run_id missing: %+v", rec)
	}
}

func TestWith_PassesThroughExtraAttrs(t *testing.T) {
	l, buf := newLogger(t)
	derived := l.With(slog.String("tool", "search"), slog.Int64("task_id", 42))
	derived.Info(context.Background(), "extra")
	rec := decodeOne(t, buf)
	if rec["tool"] != "search" {
		t.Errorf("tool missing: %+v", rec)
	}
	if v, ok := rec["task_id"]; !ok || fmt.Sprint(v) != "42" {
		t.Errorf("task_id missing or wrong: %v", rec["task_id"])
	}
}

func TestEmit_AutoStampsIdentityFromCtx(t *testing.T) {
	l, buf := newLogger(t)
	id := identity.Identity{TenantID: "T-ctx", UserID: "U-ctx", SessionID: "S-ctx"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	l.Info(ctx, "ctx-stamped")
	rec := decodeOne(t, buf)
	if rec["tenant_id"] != "T-ctx" {
		t.Errorf("ctx tenant_id missing: %+v", rec)
	}
}

func TestEmit_AutoStampsQuadrupleFromCtx(t *testing.T) {
	l, buf := newLogger(t)
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.WithRun(context.Background(), id, "R-ctx")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	l.Info(ctx, "run-ctx-stamped")
	rec := decodeOne(t, buf)
	if rec["run_id"] != "R-ctx" {
		t.Errorf("run_id missing: %+v", rec)
	}
}

func TestEmit_BoundWinsOverCtx(t *testing.T) {
	l, buf := newLogger(t)
	idCtx := identity.Identity{TenantID: "T-ctx", UserID: "U-ctx", SessionID: "S-ctx"}
	ctx, err := identity.With(context.Background(), idCtx)
	if err != nil {
		t.Fatal(err)
	}
	idBound := identity.Identity{TenantID: "T-bound", UserID: "U-bound", SessionID: "S-bound"}
	l.WithIdentity(idBound).Info(ctx, "bound-wins")
	rec := decodeOne(t, buf)
	if rec["tenant_id"] != "T-bound" {
		t.Errorf("bound tenant_id should win, got %v", rec["tenant_id"])
	}
}

func TestEmit_AbsentCtxIdentityElided(t *testing.T) {
	l, buf := newLogger(t)
	l.Info(context.Background(), "no-id")
	rec := decodeOne(t, buf)
	if _, ok := rec["tenant_id"]; ok {
		t.Errorf("tenant_id should be elided when absent, got %v", rec["tenant_id"])
	}
}

func TestEmit_LevelsRouteCorrectly(t *testing.T) {
	l, buf := newLogger(t)
	l.Debug(context.Background(), "d")
	l.Info(context.Background(), "i")
	l.Warn(context.Background(), "w")
	l.Error(context.Background(), "e")
	lines := buf.Lines()
	if len(lines) < 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), buf.String())
	}
	want := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	for i, lvl := range want {
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		if rec["level"] != lvl {
			t.Errorf("line %d level=%v, want %s", i, rec["level"], lvl)
		}
	}
}

func TestRedact_HappyPath_AttrValueRedacted(t *testing.T) {
	l, buf := newLogger(t)
	l.Info(context.Background(), "ok",
		slog.String("api_key", "real-secret-do-not-leak"))
	out := buf.String()
	if strings.Contains(out, "real-secret-do-not-leak") {
		t.Fatalf("api_key value leaked: %s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("redaction placeholder missing: %s", out)
	}
}

func TestRedact_HappyPath_BearerInValueRedacted(t *testing.T) {
	l, buf := newLogger(t)
	l.Info(context.Background(), "request",
		slog.String("trace_note", "saw header Bearer xxx.yyy.zzz come through"))
	out := buf.String()
	if strings.Contains(out, "xxx.yyy.zzz") {
		t.Fatalf("bearer credential leaked: %s", out)
	}
	if !strings.Contains(out, "Bearer ***") {
		t.Errorf("bearer redaction missing: %s", out)
	}
}

// boomRedactor is a redactor that always errors — used to test the
// fail-loudly contract.
type boomRedactor struct{}

func (boomRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, fmt.Errorf("%w: forced", audit.ErrRedactionFailed)
}

func TestRedact_FailureEmitsSentinel_NoLeakage(t *testing.T) {
	cfg := validCfg()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, boomRedactor{}, telemetry.WithWriter(buf))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info(context.Background(), "DO-NOT-LEAK-THIS-MSG",
		slog.String("api_key", "DO-NOT-LEAK-THIS-VALUE"))
	out := buf.String()
	if strings.Contains(out, "DO-NOT-LEAK-THIS-MSG") {
		t.Errorf("original msg leaked: %s", out)
	}
	if strings.Contains(out, "DO-NOT-LEAK-THIS-VALUE") {
		t.Errorf("original attr value leaked: %s", out)
	}
	if !strings.Contains(out, "[redacted: log emission blocked by redactor error]") {
		t.Errorf("sentinel missing: %s", out)
	}
}

// recordingEmitter captures bus emit calls for assertions.
type recordingEmitter struct {
	mu       sync.Mutex
	calls    int
	lastMsg  string
	lastAttr []slog.Attr
}

func (e *recordingEmitter) EmitRuntimeError(_ context.Context, msg string, attrs []slog.Attr) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.lastMsg = msg
	e.lastAttr = attrs
}

func TestError_FiresBusEmitter_WithRedactedPayload(t *testing.T) {
	em := &recordingEmitter{}
	cfg := validCfg()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, auditpatterns.New(),
		telemetry.WithWriter(buf), telemetry.WithBusEmitter(em))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Error(context.Background(), "boom",
		slog.String("api_key", "leak-me-not"))
	em.mu.Lock()
	defer em.mu.Unlock()
	if em.calls != 1 {
		t.Fatalf("emitter calls=%d, want 1", em.calls)
	}
	if em.lastMsg != "boom" {
		t.Errorf("emitter msg=%q, want boom", em.lastMsg)
	}
	for _, a := range em.lastAttr {
		if a.Key == "api_key" {
			if a.Value.Any() == "leak-me-not" {
				t.Errorf("emitter received raw secret in api_key")
			}
		}
	}
}

func TestError_NoBusEmitterCall_ForNonErrorLevels(t *testing.T) {
	em := &recordingEmitter{}
	cfg := validCfg()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, auditpatterns.New(),
		telemetry.WithWriter(buf), telemetry.WithBusEmitter(em))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Debug(context.Background(), "d")
	l.Info(context.Background(), "i")
	l.Warn(context.Background(), "w")
	em.mu.Lock()
	defer em.mu.Unlock()
	if em.calls != 0 {
		t.Errorf("emitter fired for non-error levels: calls=%d", em.calls)
	}
}

// recursiveEmitter calls back into Logger.Error to test the recursion
// guard. Without the guard this would loop forever; the guard makes
// the second call a no-op for the bus emitter (slog still writes).
type recursiveEmitter struct {
	mu     sync.Mutex
	logger *telemetry.Logger
	calls  atomic.Int64
}

func (e *recursiveEmitter) EmitRuntimeError(ctx context.Context, msg string, _ []slog.Attr) {
	e.calls.Add(1)
	e.mu.Lock()
	logger := e.logger
	e.mu.Unlock()
	if logger == nil {
		return
	}
	// This re-entrant call MUST NOT trigger another emitter invocation.
	logger.Error(ctx, "recursive: "+msg)
}

func TestError_BusEmitterRecursionGuard(t *testing.T) {
	em := &recursiveEmitter{}
	cfg := validCfg()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, auditpatterns.New(),
		telemetry.WithWriter(buf), telemetry.WithBusEmitter(em))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	em.mu.Lock()
	em.logger = l
	em.mu.Unlock()
	// Use a short timeout so a runaway loop fails the test.
	done := make(chan struct{})
	go func() {
		l.Error(context.Background(), "outer")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Error did not return within 2s — recursion guard broken (calls so far: %d)", em.calls.Load())
	}
	if em.calls.Load() != 1 {
		t.Errorf("emitter calls=%d, want 1 (recursion guard failed)", em.calls.Load())
	}
}

// panicEmitter panics on EmitRuntimeError; the Logger must recover.
type panicEmitter struct{}

func (panicEmitter) EmitRuntimeError(_ context.Context, _ string, _ []slog.Attr) {
	panic("kaboom")
}

func TestError_BusEmitterPanic_RecoveredAndLogged(t *testing.T) {
	cfg := validCfg()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, auditpatterns.New(),
		telemetry.WithWriter(buf), telemetry.WithBusEmitter(panicEmitter{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Logger.Error propagated emitter panic: %v", r)
		}
	}()
	l.Error(context.Background(), "outer")
	if !strings.Contains(buf.String(), "bus emitter panicked") {
		t.Errorf("expected panic-warn record, got: %s", buf.String())
	}
}

func TestEmit_GroupAttrResolved(t *testing.T) {
	l, buf := newLogger(t)
	l.Info(context.Background(), "grouped",
		slog.Group("auth",
			slog.String("api_key", "leak"),
			slog.String("user_agent", "harbor/0.0.1"),
		),
	)
	out := buf.String()
	if strings.Contains(out, `"api_key":"leak"`) {
		t.Errorf("api_key inside group leaked: %s", out)
	}
}

// lazySecret is a LogValuer whose Resolve materializes a secret-like
// string. Without resolveAttrs running before redaction, the secret
// would skip the redactor and leak.
type lazySecret string

func (s lazySecret) LogValue() slog.Value { return slog.StringValue(string(s)) }

func TestEmit_LogValuerResolvedBeforeRedaction(t *testing.T) {
	l, buf := newLogger(t)
	l.Info(context.Background(), "lazy",
		slog.Any("api_key", lazySecret("leak-from-valuer")))
	if strings.Contains(buf.String(), "leak-from-valuer") {
		t.Errorf("LogValuer materialized secret leaked: %s", buf.String())
	}
}

// Concurrent-reuse contract test (D-025): N≥100 goroutines deriving
// per-goroutine *Loggers from a shared base, emitting under -race,
// asserting no cross-talk in identity attributes and no goroutine
// leak.
func TestLogger_ConcurrentReuse_ReuseContract(t *testing.T) {
	const goroutines = 128
	l, buf := newLogger(t)
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i%17),
				UserID:    fmt.Sprintf("u-%d", i%41),
				SessionID: fmt.Sprintf("s-%d", i),
			}
			derived := l.WithIdentity(id)
			for j := 0; j < 8; j++ {
				derived.Info(context.Background(), "msg",
					slog.Int("iter", j),
					slog.String("session_marker", id.SessionID))
			}
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
	// Verify per-line identity coherence: each emitted record's
	// session_id must match its session_marker (proving no
	// cross-goroutine attr bleed).
	lines := buf.Lines()
	if len(lines) != goroutines*8 {
		t.Fatalf("expected %d lines, got %d", goroutines*8, len(lines))
	}
	mismatches := 0
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("malformed line: %v", err)
		}
		if rec["session_id"] != rec["session_marker"] {
			mismatches++
		}
	}
	if mismatches > 0 {
		t.Errorf("%d records observed cross-talk between bound and per-call identity", mismatches)
	}
}

// Integration test: end-to-end pipeline with the real audit patterns
// driver and a real recordingEmitter — pins the cross-package
// contract Phase 03 + Phase 04 + Phase 05+ all rely on.
func TestIntegration_E2E_AuditDriver_BusEmitter(t *testing.T) {
	em := &recordingEmitter{}
	cfg := validCfg()
	buf := &lockedBuf{}
	l, err := telemetry.New(cfg, auditpatterns.New(),
		telemetry.WithWriter(buf), telemetry.WithBusEmitter(em))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	l.Error(ctx, "downstream call failed",
		slog.String("api_key", "must-be-redacted"),
		slog.String("note", "audit log: Bearer abc.def.ghi"))

	rec := decodeOne(t, buf)
	if rec["tenant_id"] != "T" {
		t.Errorf("tenant_id missing in slog: %+v", rec)
	}
	if v, _ := rec["api_key"].(string); v != "***" {
		t.Errorf("api_key not redacted: %v", rec["api_key"])
	}
	if note, _ := rec["note"].(string); !strings.Contains(note, "Bearer ***") {
		t.Errorf("bearer not redacted in note: %v", note)
	}

	em.mu.Lock()
	defer em.mu.Unlock()
	if em.calls != 1 {
		t.Fatalf("bus emitter calls=%d, want 1", em.calls)
	}
	for _, a := range em.lastAttr {
		if a.Key == "api_key" && fmt.Sprint(a.Value.Any()) == "must-be-redacted" {
			t.Errorf("bus emitter received raw secret")
		}
	}
}

func decodeOne(t *testing.T, b *lockedBuf) map[string]any {
	t.Helper()
	line := strings.TrimSpace(strings.SplitN(b.String(), "\n", 2)[0])
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, line)
	}
	return m
}

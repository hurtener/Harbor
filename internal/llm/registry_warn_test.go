package llm_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

// captureHandler is a minimal slog.Handler that records Warn-level
// records so the unseated-wrapper warning contract is pinned by test.
type captureHandler struct {
	mu      sync.Mutex
	records []map[string]string
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, attrs)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) missingImports() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.records))
	for _, r := range h.records {
		if p, ok := r["missing_blank_import"]; ok {
			out = append(out, p)
		}
	}
	return out
}

// TestOpen_WarnsOnUnseatedWrapperHooks pins the §13 no-silent-
// degradation contract on llm.Open: when a Disable* flag is false
// (production behaviour requested) but the corresponding wrapper hook
// was never seated by its blank import, Open emits one warning per
// missing wrapper naming the blank-import path. The llm package's own
// test binary imports none of the wrapper packages, so all four hooks
// are unseated here by construction.
//
// Not parallel: it swaps the process-default slog logger.
func TestOpen_WarnsOnUnseatedWrapperHooks(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	deps, cleanup := makeDeps(t)
	defer cleanup()

	if _, err := llm.Open(context.Background(), makeSnapshot("m", 1000), deps); err != nil {
		t.Fatalf("Open: %v", err)
	}

	want := map[string]bool{
		"github.com/hurtener/Harbor/internal/llm/corrections": false,
		"github.com/hurtener/Harbor/internal/llm/output":      false,
		"github.com/hurtener/Harbor/internal/llm/retry":       false,
		"github.com/hurtener/Harbor/internal/governance":      false,
	}
	got := h.missingImports()
	for _, p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected missing_blank_import warning: %q", p)
			continue
		}
		want[p] = true
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("no unseated-wrapper warning emitted for %q", p)
		}
	}
}

// TestOpen_NoWarningWhenWrappersDisabled — explicitly disabling every
// wrapper layer is the legitimate test-isolation path; it must NOT
// warn (the operator asked for the bare client).
func TestOpen_NoWarningWhenWrappersDisabled(t *testing.T) {
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	deps, cleanup := makeDeps(t)
	defer cleanup()

	snap := makeSnapshot("m", 1000)
	snap.DisableCorrections = true
	snap.DisableDowngrade = true
	snap.DisableRetry = true
	snap.DisableGovernance = true
	if _, err := llm.Open(context.Background(), snap, deps); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := h.missingImports(); len(got) != 0 {
		t.Errorf("warnings emitted despite all Disable* flags set: %v", got)
	}
}

// preview_bound_test.go — phase 213 (D-358). `RedactAndCapPreview` is
// the one place `HeavyPreviewThreshold` is read, and the package shipped
// no test that reached it at all: the heavy branch was uncovered while
// the constant silently aliased the LLM-context heavy-output threshold.
//
// These tests are the PIN's mutation witness. Re-aliasing
// `search.HeavyPreviewThreshold` to `config.DefaultHeavyOutputThresholdBytes`
// (now 128 KiB) makes the 32768-byte and 65536-byte cases fall through
// to the inline branch and this file FAILS — it does not skip.
//
// The redactor is a real `audit/drivers/patterns` driver, not a fake
// (CLAUDE.md §17.3 read down to the unit layer: the boundary under test
// is redaction → classification).

package search_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/search"
)

// TestRedactAndCapPreview_HeavyBound_SelectsRefNotTruncation pins both
// sides of the bound plus the 64 KiB case that sits INSIDE the raised
// LLM-context offload band — the case that proves the search bound did
// not follow the raise.
//
// There is deliberately no truncation assertion on the heavy side: the
// threshold performs SELECTION (an empty preview plus a ref), and
// PreviewMaxRunes caps every emitted preview independently afterwards.
func TestRedactAndCapPreview_HeavyBound_SelectsRefNotTruncation(t *testing.T) {
	t.Parallel()
	redactor := auditpatterns.New()

	for _, tc := range []struct {
		name      string
		size      int
		wantHeavy bool
	}{
		{"one byte under the bound rides inline", search.HeavyPreviewThreshold - 1, false},
		{"exactly at the bound ships a ref", search.HeavyPreviewThreshold, true},
		{"64 KiB — inside the raised offload band — still ships a ref", 64 * 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, heavy, err := search.RedactAndCapPreview(
				context.Background(), redactor, strings.Repeat("a", tc.size))
			if err != nil {
				t.Fatalf("RedactAndCapPreview: %v", err)
			}
			if heavy != tc.wantHeavy {
				t.Fatalf("heavy = %v, want %v (size=%d, bound=%d)",
					heavy, tc.wantHeavy, tc.size, search.HeavyPreviewThreshold)
			}
			if tc.wantHeavy {
				if out != "" {
					t.Errorf("heavy row shipped %d preview bytes, want an EMPTY preview plus a ref", len(out))
				}
				return
			}
			// Inline side: capped at PreviewMaxRunes + the ellipsis.
			if runes := []rune(out); len(runes) != search.PreviewMaxRunes+1 {
				t.Errorf("inline preview is %d runes, want %d + ellipsis",
					len(runes), search.PreviewMaxRunes)
			}
			if !strings.HasSuffix(out, "…") {
				t.Error("capped inline preview lost its ellipsis")
			}
		})
	}
}

// TestRedactAndCapPreview_PinnedBelowLLMContextThreshold is the pin
// stated as an assertion rather than left implicit in the sizes above:
// the search bound is 32 KiB and is NOT the LLM-context heavy-output
// threshold. Re-aliasing them fails here first, with the reason.
func TestRedactAndCapPreview_PinnedBelowLLMContextThreshold(t *testing.T) {
	t.Parallel()
	if search.HeavyPreviewThreshold != 32*1024 {
		t.Fatalf("HeavyPreviewThreshold = %d, want 32768 — the preview bound is pinned; "+
			"it classifies a source record, it does not budget a context window",
			search.HeavyPreviewThreshold)
	}
}

// TestRedactAndCapPreview_ShortPreview_RidesInlineUncapped covers the
// ordinary path: under both bounds, nothing is altered.
func TestRedactAndCapPreview_ShortPreview_RidesInlineUncapped(t *testing.T) {
	t.Parallel()
	out, heavy, err := search.RedactAndCapPreview(
		context.Background(), auditpatterns.New(), "a modest preview")
	if err != nil {
		t.Fatalf("RedactAndCapPreview: %v", err)
	}
	if heavy {
		t.Error("a 16-byte preview was classified heavy")
	}
	if out != "a modest preview" {
		t.Errorf("preview = %q, want it unaltered", out)
	}
}

// TestRedactAndCapPreview_EmptyPreview_IsNeitherHeavyNorAnError pins
// the short-circuit ahead of the redactor call.
func TestRedactAndCapPreview_EmptyPreview_IsNeitherHeavyNorAnError(t *testing.T) {
	t.Parallel()
	out, heavy, err := search.RedactAndCapPreview(context.Background(), auditpatterns.New(), "")
	if err != nil || heavy || out != "" {
		t.Errorf("empty preview → (%q, %v, %v), want (\"\", false, nil)", out, heavy, err)
	}
}

// nonStringRedactor returns a structured marker instead of a string —
// the shape an audit driver produces when it replaces a whole value.
type nonStringRedactor struct{}

func (nonStringRedactor) Redact(_ context.Context, _ any) (any, error) {
	return map[string]any{"redacted": true}, nil
}

// erroringRedactor fails loudly; the caller must not emit a row.
type erroringRedactor struct{ err error }

func (r erroringRedactor) Redact(_ context.Context, _ any) (any, error) { return nil, r.err }

// TestRedactAndCapPreview_NonStringRedaction_ShipsReflessEmptyRow pins
// the branch a heavy-vs-inline change could quietly reroute: a driver
// that returns a structured marker yields an empty, NON-heavy preview
// (a refless row), never the marker's Go rendering.
func TestRedactAndCapPreview_NonStringRedaction_ShipsReflessEmptyRow(t *testing.T) {
	t.Parallel()
	out, heavy, err := search.RedactAndCapPreview(
		context.Background(), nonStringRedactor{}, strings.Repeat("b", 64*1024))
	if err != nil {
		t.Fatalf("RedactAndCapPreview: %v", err)
	}
	if heavy {
		t.Error("a non-string redaction must not be classified heavy — the row is refless")
	}
	if out != "" {
		t.Errorf("preview = %q, want empty", out)
	}
}

// TestRedactAndCapPreview_NilRedactor_FailsLoudly — identity of the
// failure matters: no silent degradation to an empty preview
// (CLAUDE.md §13).
func TestRedactAndCapPreview_NilRedactor_FailsLoudly(t *testing.T) {
	t.Parallel()
	if _, _, err := search.RedactAndCapPreview(context.Background(), nil, "x"); !errors.Is(err, search.ErrRedactionFailed) {
		t.Errorf("nil redactor → %v, want ErrRedactionFailed", err)
	}
}

// TestRedactAndCapPreview_RedactorError_Propagates — the caller MUST
// NOT emit a row, so the error is wrapped, not swallowed.
func TestRedactAndCapPreview_RedactorError_Propagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("driver exploded")
	_, _, err := search.RedactAndCapPreview(
		context.Background(), erroringRedactor{err: sentinel}, "x")
	if !errors.Is(err, search.ErrRedactionFailed) || !errors.Is(err, sentinel) {
		t.Errorf("redactor error → %v, want both ErrRedactionFailed and the driver error", err)
	}
}

// compile-time assertion: the test doubles satisfy the real interface.
var (
	_ audit.Redactor = nonStringRedactor{}
	_ audit.Redactor = erroringRedactor{}
)

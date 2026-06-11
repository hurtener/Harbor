// Phase 84b (D-189) — disposition-resolution tests for the shared
// run-loop helper: the thin-caller contract (precedence lives in
// internal/planner; this helper only threads hints/policy, fetches
// bytes for inline, logs the winning layer, and emits the
// `task.input_disposition.resolved` event).
package runctx_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/tools"
)

// dispCatalogView is a minimal planner.ToolCatalogView fixture.
type dispCatalogView struct{ listed []tools.Tool }

func (c dispCatalogView) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.listed {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (c dispCatalogView) List() []tools.Tool { return c.listed }

// TestResolveInputArtifacts_HintWinsAndEventEmitted — the
// per-attachment caller hint (top precedence layer) routes a PDF to a
// forced tool, the view carries the resolved disposition, and the
// helper emits one `task.input_disposition.resolved` event per
// attachment with the winning layer.
func TestResolveInputArtifacts_HintWinsAndEventEmitted(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	ref, err := store.PutBytes(ctx, runctxScope(), []byte("%PDF-1.4"), artifacts.PutOpts{
		MimeType: "application/pdf", Filename: "doc.pdf",
	})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	var emitted []events.Event
	catalog := dispCatalogView{listed: []tools.Tool{{Name: "pdf.extract"}}}
	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ, []string{ref.ID}, slog.Default(),
		runctx.InputArtifactOptions{
			Hints:   runctx.DispositionHints(map[string]string{ref.ID: "tool:pdf.extract"}),
			Policy:  planner.DispositionPolicy{Default: planner.DispositionRef},
			Catalog: catalog,
			Emit:    func(ev events.Event) { emitted = append(emitted, ev) },
		})
	if len(got) != 1 {
		t.Fatalf("resolved %d views, want 1", len(got))
	}
	if got[0].Disposition != planner.DispositionTool("pdf.extract") {
		t.Fatalf("Disposition = %q, want tool:pdf.extract", got[0].Disposition)
	}
	if got[0].Bytes != nil {
		t.Error("tool-disposed artifact must stay ref-only (no inline bytes)")
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted %d events, want 1", len(emitted))
	}
	ev := emitted[0]
	if ev.Type != runctx.EventTypeInputDispositionResolved {
		t.Fatalf("event type = %q", ev.Type)
	}
	payload, ok := ev.Payload.(runctx.InputDispositionResolvedPayload)
	if !ok {
		t.Fatalf("payload type = %T", ev.Payload)
	}
	if payload.Disposition != "tool:pdf.extract" || payload.Layer != string(planner.DispositionLayerCallerHint) {
		t.Fatalf("payload = %+v, want disposition=tool:pdf.extract layer=caller_hint", payload)
	}
	if payload.Degraded {
		t.Fatalf("unexpected degradation on a known tool: %+v", payload)
	}
	if payload.Identity != runctxTestQ || ev.Identity != runctxTestQ {
		t.Fatal("identity quadruple did not propagate onto the event")
	}
}

// TestResolveInputArtifacts_PolicyLayerAndInlineByteFetch — the
// per-agent policy map (middle layer) flips a non-default branch:
// an `image/*: ref` policy suppresses the default inline byte fetch,
// while an explicit inline policy on text degrades loudly.
func TestResolveInputArtifacts_PolicyLayerAndInlineByteFetch(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	imgRef, err := store.PutBytes(ctx, runctxScope(), []byte{0x89, 0x50}, artifacts.PutOpts{MimeType: "image/png"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	policy := planner.DispositionPolicy{
		ByMIME: map[string]planner.AttachmentDisposition{"image/*": planner.DispositionRef},
	}
	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ, []string{imgRef.ID}, slog.Default(),
		runctx.InputArtifactOptions{Policy: policy})
	if len(got) != 1 {
		t.Fatalf("resolved %d views, want 1", len(got))
	}
	if got[0].Disposition != planner.DispositionRef {
		t.Fatalf("Disposition = %q, want ref (agent policy layer)", got[0].Disposition)
	}
	if got[0].Bytes != nil {
		t.Error("ref-disposed image must not fetch bytes")
	}
}

// TestResolveInputArtifacts_DegradationsLoggedAndEmitted — an unknown
// `tool:<name>` hint and a pre-84c provider_native policy both degrade
// to ref with a Warn log AND a degradation-carrying event (CLAUDE.md
// §13 — never a silent drop).
func TestResolveInputArtifacts_DegradationsLoggedAndEmitted(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	pdfRef, err := store.PutBytes(ctx, runctxScope(), []byte("%PDF"), artifacts.PutOpts{MimeType: "application/pdf"})
	if err != nil {
		t.Fatalf("PutBytes pdf: %v", err)
	}
	imgRef, err := store.PutBytes(ctx, runctxScope(), []byte{0x89}, artifacts.PutOpts{MimeType: "image/png"})
	if err != nil {
		t.Fatalf("PutBytes img: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	var emitted []events.Event
	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ, []string{pdfRef.ID, imgRef.ID}, logger,
		runctx.InputArtifactOptions{
			Hints:   runctx.DispositionHints(map[string]string{pdfRef.ID: "tool:no.such.tool"}),
			Policy:  planner.DispositionPolicy{ByMIME: map[string]planner.AttachmentDisposition{"image/*": planner.DispositionProviderNative}},
			Catalog: dispCatalogView{},
			Emit:    func(ev events.Event) { emitted = append(emitted, ev) },
		})
	if len(got) != 2 {
		t.Fatalf("resolved %d views, want 2", len(got))
	}
	for i, view := range got {
		if view.Disposition != planner.DispositionRef {
			t.Fatalf("views[%d].Disposition = %q, want ref (degraded)", i, view.Disposition)
		}
	}
	logText := buf.String()
	if !strings.Contains(logText, "attachment disposition degraded") {
		t.Errorf("expected Warn for degradation; log: %s", logText)
	}
	if !strings.Contains(logText, "unknown_tool") || !strings.Contains(logText, "provider_native_unavailable") {
		t.Errorf("expected both degradation reasons in the log; log: %s", logText)
	}
	if len(emitted) != 2 {
		t.Fatalf("emitted %d events, want 2", len(emitted))
	}
	p0 := emitted[0].Payload.(runctx.InputDispositionResolvedPayload)
	if !p0.Degraded || p0.DegradationReason != string(planner.DegradationUnknownTool) || p0.Tool != "no.such.tool" {
		t.Fatalf("pdf payload = %+v, want unknown_tool degradation", p0)
	}
	if p0.DegradedFrom != "tool:no.such.tool" {
		t.Fatalf("pdf payload DegradedFrom = %q", p0.DegradedFrom)
	}
	p1 := emitted[1].Payload.(runctx.InputDispositionResolvedPayload)
	if !p1.Degraded || p1.DegradationReason != string(planner.DegradationProviderNativeUnavailable) {
		t.Fatalf("image payload = %+v, want provider_native_unavailable degradation", p1)
	}
}

// TestResolveInputArtifacts_RuntimeDefaultLayerOnZeroOptions — zero
// options reproduce the pre-84b behaviour and report the
// runtime_default layer on the emitted event.
func TestResolveInputArtifacts_RuntimeDefaultLayerOnZeroOptions(t *testing.T) {
	store := newRunctxArtifactStore(t)
	ctx := context.Background()
	imgRef, err := store.PutBytes(ctx, runctxScope(), []byte{0x89, 0x50}, artifacts.PutOpts{MimeType: "image/png"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	var emitted []events.Event
	got := runctx.ResolveInputArtifacts(ctx, store, runctxTestQ, []string{imgRef.ID}, slog.Default(),
		runctx.InputArtifactOptions{Emit: func(ev events.Event) { emitted = append(emitted, ev) }})
	if len(got) != 1 || got[0].Disposition != planner.DispositionInline || got[0].Bytes == nil {
		t.Fatalf("zero-options image view drifted from the inline default: %#v", got)
	}
	if len(emitted) != 1 {
		t.Fatalf("emitted %d events, want 1", len(emitted))
	}
	payload := emitted[0].Payload.(runctx.InputDispositionResolvedPayload)
	if payload.Layer != string(planner.DispositionLayerRuntimeDefault) || payload.Disposition != "inline" {
		t.Fatalf("payload = %+v, want runtime_default/inline", payload)
	}
}

// TestDispositionHints covers the string→typed conversion.
func TestDispositionHints(t *testing.T) {
	if got := runctx.DispositionHints(nil); got != nil {
		t.Fatalf("nil in: got %v", got)
	}
	got := runctx.DispositionHints(map[string]string{"a": "ref", "b": "tool:x"})
	if got["a"] != planner.DispositionRef || got["b"] != planner.DispositionTool("x") {
		t.Fatalf("converted map drifted: %v", got)
	}
}

package planner_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// parityCatalog advertises one MIME-handling tool so the Fetch.Tool
// population path is part of the golden surface.
func parityCatalog() planner.ToolCatalogView {
	return stubCatalogView{listed: []tools.Tool{
		{Name: "csv.summarise", HandlesMIME: []string{"text/csv"}},
		{Name: "pdf.extract", HandlesMIME: []string{"application/pdf"}},
	}}
}

// parityViews is the fixed input set the default-parity golden test
// pins: every pre-84b dispatcher branch (image-with-bytes,
// image-missing-bytes, pdf, audio, catalog-matched CSV, unknown MIME).
func parityViews() []planner.InputArtifactView {
	return []planner.InputArtifactView{
		{ID: "art_img", MIME: "image/png", SizeBytes: 8, Filename: "pixel.png", Bytes: []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}},
		{ID: "art_img_nobytes", MIME: "image/jpeg", SizeBytes: 4096, Filename: "lost.jpg"},
		{ID: "art_pdf", MIME: "application/pdf", SizeBytes: 2048, Filename: "doc.pdf"},
		{ID: "art_audio", MIME: "audio/wav", SizeBytes: 512, Filename: "clip.wav"},
		{ID: "art_csv", MIME: "text/csv", SizeBytes: 64, Filename: "rows.csv"},
		{ID: "art_bin", MIME: "application/x-custom", SizeBytes: 32},
	}
}

// goldenParityContent is the EXPECTED materialization of parityViews —
// built explicitly from the llm part types, independent of the
// materializer implementation, pinning today's (pre-84b) behaviour
// byte-for-byte: image inline as DataURL, PDF as FilePart stub, audio
// as AudioPart stub, everything else (including a bytes-less image) as
// an ArtifactStub text block, with Fetch.Tool populated from the
// catalog's HandlesMIME match.
func goldenParityContent(t *testing.T, goal string) llm.Content {
	t.Helper()
	stubText := func(stub *llm.ArtifactStub, filename string) string {
		raw, err := stub.MarshalJSON()
		if err != nil {
			t.Fatalf("stub MarshalJSON: %v", err)
		}
		header := "Attachment"
		if filename != "" {
			header += " — " + filename
		}
		return header + ":\n" + string(raw)
	}
	imgBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	parts := []llm.ContentPart{
		{Type: llm.PartText, Text: goal},
		{Type: llm.PartImage, Image: &llm.ImagePart{
			DataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes),
			MIME:    "image/png",
		}},
		{Type: llm.PartText, Text: stubText(&llm.ArtifactStub{
			Ref: "art_img_nobytes", MIME: "image/jpeg", SizeBytes: 4096,
			Summary: "User-uploaded image/jpeg input (4096 bytes) [lost.jpg]",
		}, "lost.jpg")},
		{Type: llm.PartFile, File: &llm.FilePart{
			Artifact: &llm.ArtifactStub{
				Ref: "art_pdf", MIME: "application/pdf", SizeBytes: 2048,
				Summary: "User-uploaded application/pdf input (2048 bytes) [doc.pdf]",
				Fetch:   &llm.StubFetch{Tool: "pdf.extract", ID: "art_pdf"},
			},
			MIME:     "application/pdf",
			Filename: "doc.pdf",
		}},
		{Type: llm.PartAudio, Audio: &llm.AudioPart{
			Artifact: &llm.ArtifactStub{
				Ref: "art_audio", MIME: "audio/wav", SizeBytes: 512,
				Summary: "User-uploaded audio/wav input (512 bytes) [clip.wav]",
			},
			MIME: "audio/wav",
		}},
		{Type: llm.PartText, Text: stubText(&llm.ArtifactStub{
			Ref: "art_csv", MIME: "text/csv", SizeBytes: 64,
			Summary: "User-uploaded text/csv input (64 bytes) [rows.csv]",
			Fetch:   &llm.StubFetch{Tool: "csv.summarise", ID: "art_csv"},
		}, "rows.csv")},
		{Type: llm.PartText, Text: stubText(&llm.ArtifactStub{
			Ref: "art_bin", MIME: "application/x-custom", SizeBytes: 32,
			Summary: "User-uploaded application/x-custom input (32 bytes)",
		}, "")},
	}
	return llm.Content{Parts: parts}
}

// assertContentEqual compares two llm.Content values byte-for-byte via
// their JSON encodings (the representation that reaches the driver
// translation layer).
func assertContentEqual(t *testing.T, got, want llm.Content) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("default-parity drift:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestMaterializeInputContent_DefaultParityGolden is the Phase 84b #1
// guard: with NO disposition declared anywhere (zero Disposition on
// every view — the pre-84b call shape), every MIME materializes
// exactly as it did before the policy seam landed. Byte-for-byte.
func TestMaterializeInputContent_DefaultParityGolden(t *testing.T) {
	t.Parallel()
	got := planner.MaterializeInputContent("summarise my uploads", parityViews(), parityCatalog())
	assertContentEqual(t, got, goldenParityContent(t, "summarise my uploads"))
}

// TestMaterializeInputContent_RuntimeDefaultResolutionParity pins the
// second half of the parity criterion: views whose Disposition was
// resolved through the EMPTY policy chain (ResolveDisposition +
// EffectiveDisposition with no hint, no policy) materialize
// byte-for-byte identically to the zero-Disposition legacy path.
func TestMaterializeInputContent_RuntimeDefaultResolutionParity(t *testing.T) {
	t.Parallel()
	catalog := parityCatalog()
	views := parityViews()
	for i := range views {
		resolved, layer := planner.ResolveDisposition("", planner.DispositionPolicy{}, views[i].MIME)
		if layer != planner.DispositionLayerRuntimeDefault {
			t.Fatalf("views[%d] layer = %q, want runtime_default", i, layer)
		}
		effective, degr := planner.EffectiveDisposition(resolved, views[i].MIME, catalog)
		if degr != nil {
			t.Fatalf("views[%d] unexpected degradation: %+v", i, degr)
		}
		views[i].Disposition = effective
	}
	got := planner.MaterializeInputContent("summarise my uploads", views, catalog)
	assertContentEqual(t, got, goldenParityContent(t, "summarise my uploads"))
}

// TestResolveDisposition_Precedence exercises the three layers across
// all four enum values.
func TestResolveDisposition_Precedence(t *testing.T) {
	t.Parallel()
	policy := planner.DispositionPolicy{
		ByMIME: map[string]planner.AttachmentDisposition{
			"application/pdf": planner.DispositionTool("pdf.extract"),
			"image/*":         planner.DispositionProviderNative,
		},
		Default: planner.DispositionRef,
	}
	cases := []struct {
		name      string
		hint      planner.AttachmentDisposition
		policy    planner.DispositionPolicy
		mime      string
		wantD     planner.AttachmentDisposition
		wantLayer planner.DispositionLayer
	}{
		{"hint ref wins over policy", planner.DispositionRef, policy, "application/pdf", planner.DispositionRef, planner.DispositionLayerCallerHint},
		{"hint inline wins", planner.DispositionInline, policy, "application/pdf", planner.DispositionInline, planner.DispositionLayerCallerHint},
		{"hint provider_native wins", planner.DispositionProviderNative, policy, "text/csv", planner.DispositionProviderNative, planner.DispositionLayerCallerHint},
		{"hint tool wins", planner.DispositionTool("doc.index"), policy, "application/pdf", planner.DispositionTool("doc.index"), planner.DispositionLayerCallerHint},
		{"policy exact MIME", "", policy, "application/pdf", planner.DispositionTool("pdf.extract"), planner.DispositionLayerAgentPolicy},
		{"policy family wildcard", "", policy, "image/png", planner.DispositionProviderNative, planner.DispositionLayerAgentPolicy},
		{"policy default", "", policy, "audio/wav", planner.DispositionRef, planner.DispositionLayerAgentPolicy},
		{"runtime default image", "", planner.DispositionPolicy{}, "image/png", planner.DispositionInline, planner.DispositionLayerRuntimeDefault},
		{"runtime default non-image", "", planner.DispositionPolicy{}, "application/pdf", planner.DispositionRef, planner.DispositionLayerRuntimeDefault},
		{"runtime default empty mime", "", planner.DispositionPolicy{}, "", planner.DispositionRef, planner.DispositionLayerRuntimeDefault},
		{"map-only policy without default falls through", "", planner.DispositionPolicy{ByMIME: map[string]planner.AttachmentDisposition{"application/pdf": planner.DispositionRef}}, "image/png", planner.DispositionInline, planner.DispositionLayerRuntimeDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotD, gotLayer := planner.ResolveDisposition(tc.hint, tc.policy, tc.mime)
			if gotD != tc.wantD || gotLayer != tc.wantLayer {
				t.Fatalf("ResolveDisposition(%q, policy, %q) = (%q, %q), want (%q, %q)",
					tc.hint, tc.mime, gotD, gotLayer, tc.wantD, tc.wantLayer)
			}
		})
	}
}

// TestEffectiveDisposition_Degradations exercises every capability
// constraint and asserts the typed degradation facts.
func TestEffectiveDisposition_Degradations(t *testing.T) {
	t.Parallel()
	catalog := parityCatalog()
	cases := []struct {
		name       string
		resolved   planner.AttachmentDisposition
		mime       string
		catalog    planner.ToolCatalogView
		wantD      planner.AttachmentDisposition
		wantReason planner.DispositionDegradationReason // "" = no degradation
		wantTool   string
	}{
		{"ref passes", planner.DispositionRef, "application/pdf", catalog, planner.DispositionRef, "", ""},
		{"zero passes", "", "application/pdf", catalog, "", "", ""},
		{"inline image passes", planner.DispositionInline, "image/png", catalog, planner.DispositionInline, "", ""},
		{"inline non-image degrades", planner.DispositionInline, "application/pdf", catalog, planner.DispositionRef, planner.DegradationInlineUnsupportedMIME, ""},
		{"provider_native degrades pre-84c", planner.DispositionProviderNative, "image/png", catalog, planner.DispositionRef, planner.DegradationProviderNativeUnavailable, ""},
		{"known tool passes", planner.DispositionTool("pdf.extract"), "application/pdf", catalog, planner.DispositionTool("pdf.extract"), "", ""},
		{"unknown tool degrades", planner.DispositionTool("nope.tool"), "application/pdf", catalog, planner.DispositionRef, planner.DegradationUnknownTool, "nope.tool"},
		{"tool with nil catalog kept verbatim", planner.DispositionTool("anything"), "text/csv", nil, planner.DispositionTool("anything"), "", ""},
		{"invalid value degrades", planner.AttachmentDisposition("garbage"), "text/csv", catalog, planner.DispositionRef, planner.DegradationInvalidDisposition, ""},
		{"bare tool prefix degrades as invalid", planner.AttachmentDisposition("tool:"), "text/csv", catalog, planner.DispositionRef, planner.DegradationInvalidDisposition, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotD, degr := planner.EffectiveDisposition(tc.resolved, tc.mime, tc.catalog)
			if gotD != tc.wantD {
				t.Fatalf("effective = %q, want %q", gotD, tc.wantD)
			}
			if tc.wantReason == "" {
				if degr != nil {
					t.Fatalf("unexpected degradation: %+v", degr)
				}
				return
			}
			if degr == nil {
				t.Fatalf("want degradation %q, got nil", tc.wantReason)
			}
			if degr.Reason != tc.wantReason || degr.To != planner.DispositionRef || degr.From != tc.resolved || degr.Tool != tc.wantTool {
				t.Fatalf("degradation = %+v, want reason=%q to=ref from=%q tool=%q", degr, tc.wantReason, tc.resolved, tc.wantTool)
			}
		})
	}
}

// TestParseDisposition covers the carrier-edge grammar.
func TestParseDisposition(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"ref", "inline", "provider_native", "tool:pdf.extract", "tool:a"} {
		d, err := planner.ParseDisposition(ok)
		if err != nil {
			t.Fatalf("ParseDisposition(%q): %v", ok, err)
		}
		if string(d) != ok {
			t.Fatalf("ParseDisposition(%q) = %q", ok, d)
		}
	}
	for _, bad := range []string{"", "REF", "tool:", "native", "tool", "inline "} {
		if _, err := planner.ParseDisposition(bad); !errors.Is(err, planner.ErrInvalidDisposition) {
			t.Fatalf("ParseDisposition(%q): want ErrInvalidDisposition, got %v", bad, err)
		}
	}
}

// TestAttachmentDisposition_ToolName covers the parametrised accessor.
func TestAttachmentDisposition_ToolName(t *testing.T) {
	t.Parallel()
	if name, ok := planner.DispositionTool("pdf.extract").ToolName(); !ok || name != "pdf.extract" {
		t.Fatalf("ToolName = (%q, %v), want (pdf.extract, true)", name, ok)
	}
	for _, d := range []planner.AttachmentDisposition{"", "ref", "inline", "provider_native", "tool:"} {
		if name, ok := d.ToolName(); ok {
			t.Fatalf("ToolName(%q) = (%q, true), want false", d, name)
		}
	}
}

// TestDispositionPolicyFromConfig pins the harbor.yaml block decode:
// the config carrier and the programmatic construction produce the
// same DispositionPolicy value.
func TestDispositionPolicyFromConfig(t *testing.T) {
	t.Parallel()
	got, err := planner.DispositionPolicyFromConfig(config.MultimodalConfig{
		Disposition: map[string]string{
			"application/pdf": "tool:pdf.extract",
			"image/*":         "inline",
			"*":               "ref",
		},
	})
	if err != nil {
		t.Fatalf("DispositionPolicyFromConfig: %v", err)
	}
	if got.Default != planner.DispositionRef {
		t.Fatalf("Default = %q, want ref", got.Default)
	}
	if got.ByMIME["application/pdf"] != planner.DispositionTool("pdf.extract") {
		t.Fatalf("ByMIME[application/pdf] = %q", got.ByMIME["application/pdf"])
	}
	if got.ByMIME["image/*"] != planner.DispositionInline {
		t.Fatalf("ByMIME[image/*] = %q", got.ByMIME["image/*"])
	}
	if _, hasStar := got.ByMIME["*"]; hasStar {
		t.Fatal("the * key must decode onto Default, not stay in ByMIME")
	}

	// Zero block → zero policy.
	zero, err := planner.DispositionPolicyFromConfig(config.MultimodalConfig{})
	if err != nil {
		t.Fatalf("zero decode: %v", err)
	}
	if !zero.IsZero() {
		t.Fatalf("zero block decoded non-zero policy: %+v", zero)
	}

	// Invalid value fails loud (defense-in-depth behind the validator).
	if _, err := planner.DispositionPolicyFromConfig(config.MultimodalConfig{
		Disposition: map[string]string{"text/csv": "bogus"},
	}); !errors.Is(err, planner.ErrInvalidDisposition) {
		t.Fatalf("want ErrInvalidDisposition, got %v", err)
	}
}

// TestMaterializeInputContent_DispositionConsultation covers the
// materializer as the policy consumer: each non-zero disposition
// routes its branch.
func TestMaterializeInputContent_DispositionConsultation(t *testing.T) {
	t.Parallel()
	catalog := parityCatalog()
	pdf := planner.InputArtifactView{ID: "art_pdf", MIME: "application/pdf", SizeBytes: 2048, Filename: "doc.pdf"}
	img := planner.InputArtifactView{ID: "art_img", MIME: "image/png", SizeBytes: 8, Filename: "p.png", Bytes: []byte{1, 2, 3}}

	t.Run("tool forces Fetch.Tool over catalog discovery", func(t *testing.T) {
		t.Parallel()
		v := pdf
		v.Disposition = planner.DispositionTool("doc.index")
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		part := c.Parts[1]
		if part.Type != llm.PartText {
			t.Fatalf("part type = %q, want text stub", part.Type)
		}
		if !strings.Contains(part.Text, `"tool":"doc.index"`) {
			t.Fatalf("stub text missing forced tool: %s", part.Text)
		}
	})

	t.Run("explicit ref on an image emits a stub, not inline", func(t *testing.T) {
		t.Parallel()
		v := img
		v.Disposition = planner.DispositionRef
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		part := c.Parts[1]
		if part.Type != llm.PartText || !strings.Contains(part.Text, `"artifact_ref":"art_img"`) {
			t.Fatalf("explicit ref image: got %+v, want ArtifactStub text part", part)
		}
	})

	t.Run("explicit ref on a pdf keeps the typed FilePart stub", func(t *testing.T) {
		t.Parallel()
		v := pdf
		v.Disposition = planner.DispositionRef
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		if c.Parts[1].Type != llm.PartFile || c.Parts[1].File == nil || c.Parts[1].File.Artifact == nil {
			t.Fatalf("ref pdf part = %+v, want FilePart with Artifact stub", c.Parts[1])
		}
	})

	t.Run("explicit ref on audio keeps the typed AudioPart stub", func(t *testing.T) {
		t.Parallel()
		v := planner.InputArtifactView{ID: "art_a", MIME: "audio/wav", SizeBytes: 9, Disposition: planner.DispositionRef}
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		if c.Parts[1].Type != llm.PartAudio || c.Parts[1].Audio == nil || c.Parts[1].Audio.Artifact == nil {
			t.Fatalf("ref audio part = %+v, want AudioPart with Artifact stub", c.Parts[1])
		}
	})

	t.Run("inline image with bytes inlines", func(t *testing.T) {
		t.Parallel()
		v := img
		v.Disposition = planner.DispositionInline
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		if c.Parts[1].Type != llm.PartImage || c.Parts[1].Image == nil || c.Parts[1].Image.DataURL == "" {
			t.Fatalf("inline image part = %+v, want ImagePart with DataURL", c.Parts[1])
		}
	})

	t.Run("inline image without bytes degrades to stub", func(t *testing.T) {
		t.Parallel()
		v := planner.InputArtifactView{ID: "art_i2", MIME: "image/png", SizeBytes: 99, Disposition: planner.DispositionInline}
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		if c.Parts[1].Type != llm.PartText || !strings.Contains(c.Parts[1].Text, `"artifact_ref":"art_i2"`) {
			t.Fatalf("inline-no-bytes part = %+v, want stub text fallback", c.Parts[1])
		}
	})

	t.Run("defensive provider_native renders as ref", func(t *testing.T) {
		t.Parallel()
		v := pdf
		v.Disposition = planner.DispositionProviderNative
		c := planner.MaterializeInputContent("goal", []planner.InputArtifactView{v}, catalog)
		if c.Parts[1].Type != llm.PartFile {
			t.Fatalf("provider_native pdf part = %+v, want the ref FilePart degradation", c.Parts[1])
		}
	})
}

// TestResolveDisposition_ConcurrentReuse exercises the pure resolver
// from many goroutines against shared inputs under -race (D-025: pure
// functions over shared immutable values must be trivially safe).
func TestResolveDisposition_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	policy := planner.DispositionPolicy{
		ByMIME:  map[string]planner.AttachmentDisposition{"application/pdf": planner.DispositionTool("pdf.extract")},
		Default: planner.DispositionRef,
	}
	catalog := parityCatalog()
	done := make(chan error, 128)
	for i := 0; i < 128; i++ {
		go func(n int) {
			mime := "application/pdf"
			if n%2 == 0 {
				mime = "image/png"
			}
			d, layer := planner.ResolveDisposition("", policy, mime)
			eff, degr := planner.EffectiveDisposition(d, mime, catalog)
			switch mime {
			case "application/pdf":
				if eff != planner.DispositionTool("pdf.extract") || layer != planner.DispositionLayerAgentPolicy || degr != nil {
					done <- fmt.Errorf("pdf resolution drifted: %q %q %+v", eff, layer, degr)
					return
				}
			default:
				if eff != planner.DispositionRef || layer != planner.DispositionLayerAgentPolicy || degr != nil {
					done <- fmt.Errorf("image resolution drifted: %q %q %+v", eff, layer, degr)
					return
				}
			}
			done <- nil
		}(i)
	}
	for i := 0; i < 128; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

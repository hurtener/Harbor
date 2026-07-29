package builtin

// Read-path byte correctness for artifact_fetch.
//
// The tool returns its window through a `Content string` field that is
// JSON-encoded on its way to the model, and `encoding/json` rewrites
// every invalid UTF-8 byte to U+FFFD. So a window that is not valid text
// arrives CORRUPTED, at a different length than `returned_bytes`
// reported. These tests pin the three properties that close it: an
// inadmissible window refuses loudly, an admissible one is byte-exact
// and self-consistent, and a paging loop terminates.
//
// A sibling file to artifact_fetch_test.go rather than an append to it:
// same package and same helpers, but a separate file keeps this phase's
// additions off a file another in-flight phase also edits.

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// seedArtifactMIME is seedArtifact with the stored MIME under the
// test's control. Heavy tool results are stamped `application/json`
// unconditionally, so the admissibility rule cannot be MIME-driven —
// the MIME is what the REFUSAL reports, not what it decides on.
func seedArtifactMIME(t *testing.T, store artifacts.ArtifactStore, payload []byte, mime string) artifacts.ArtifactRef {
	t.Helper()
	ref, err := store.PutBytes(t.Context(),
		artifacts.ArtifactScope{TenantID: "tA", UserID: "uA", SessionID: "sA"},
		payload,
		artifacts.PutOpts{Namespace: "test.fixture", MimeType: mime})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref
}

// mixedWidthFixture is a blob carrying every UTF-8 rune width — 1, 2, 3
// and 4 bytes — so a byte-addressed window lands mid-rune at both ends
// at many offsets. It is the fixture the invariants are asserted over as
// PROPERTIES rather than as a hand-picked table: a table would have
// missed the livelock.
func mixedWidthFixture() []byte {
	return []byte("aé☃𝄞b" + "zé☃𝄞" + "𝄞☃éq")
}

// TestArtifactFetch_AdmissibilityMatrix walks the fixture kinds a read
// can meet: valid text at every rune width, a window split at the head,
// at the tail and at both, real binary magic numbers, one bad byte
// inside otherwise-valid text, an empty artifact and an offset past the
// end.
func TestArtifactFetch_AdmissibilityMatrix(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	// A 4-byte rune (U+1D11E) so head/tail splits are reachable.
	astral := "\U0001D11E"

	for _, tc := range []struct {
		name     string
		blob     []byte
		mime     string
		offset   int
		maxBytes int
		// wantRefused: the window is inadmissible.
		wantRefused bool
		// wantContent is checked only when wantRefused is false.
		wantContent string
		wantOffset  int64
	}{
		{
			name: "pure ASCII passes through untouched",
			blob: []byte("hello world"), mime: "text/plain",
			offset: 0, maxBytes: 5, wantContent: "hello", wantOffset: 0,
		},
		{
			name: "two-byte runes survive a boundary-aligned window",
			blob: []byte("ééééé"), mime: "text/plain",
			offset: 0, maxBytes: 4, wantContent: "éé", wantOffset: 0,
		},
		{
			name: "three-byte runes survive a boundary-aligned window",
			blob: []byte("☃☃☃"), mime: "text/plain",
			offset: 0, maxBytes: 6, wantContent: "☃☃", wantOffset: 0,
		},
		{
			name: "four-byte rune survives a boundary-aligned window",
			blob: []byte(astral + astral), mime: "text/plain",
			offset: 0, maxBytes: 4, wantContent: astral, wantOffset: 0,
		},
		{
			name: "a rune split at the window TAIL is trimmed, not refused",
			blob: []byte("ab" + astral + "cd"), mime: "text/plain",
			// bytes 0..3 = "ab" + the first two bytes of the astral rune.
			offset: 0, maxBytes: 4, wantContent: "ab", wantOffset: 0,
		},
		{
			name: "a rune split at the window HEAD is trimmed and the offset advances",
			blob: []byte("ab" + astral + "cd"), mime: "text/plain",
			// offset 3 lands one byte into the astral rune.
			offset: 3, maxBytes: 8, wantContent: "cd", wantOffset: 6,
		},
		{
			name: "a rune split at BOTH ends is trimmed at both",
			blob: []byte(astral + "xy" + astral), mime: "text/plain",
			// offset 1 is mid-rune; the window ends mid the trailing rune.
			offset: 1, maxBytes: 7, wantContent: "xy", wantOffset: 4,
		},
		{
			name: "a PNG magic number is refused",
			blob: append([]byte("\x89PNG\r\n\x1a\n"), 0x00, 0x00, 0x00, 0x0D), mime: "image/png",
			offset: 0, maxBytes: 12, wantRefused: true,
		},
		{
			name: "a zip local-file header is refused",
			blob: []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00\xff\xfe\xfd\xfc"), mime: "application/zip",
			offset: 0, maxBytes: 14, wantRefused: true,
		},
		{
			name: "a PDF body with a binary stream is refused",
			blob: append([]byte("%PDF-1.7\nstream\n"), 0xDE, 0xAD, 0xBE, 0xEF), mime: "application/pdf",
			offset: 0, maxBytes: 32, wantRefused: true,
		},
		{
			name: "one bad byte inside otherwise-valid text is refused, not dropped",
			blob: []byte("good text \xC3( more good text"), mime: "text/plain",
			offset: 0, maxBytes: 64, wantRefused: true,
		},
		{
			name: "a leading continuation byte at offset ZERO is content, not a split",
			// No preceding byte to blame, so this is malformed content.
			blob: []byte("\xA9\xA9hello"), mime: "text/plain",
			offset: 0, maxBytes: 16, wantRefused: true,
		},
		{
			name: "an incomplete trailing rune at the END of the artifact is content, not a split",
			// Nothing follows the window, so the missing bytes are missing
			// from the artifact — that is malformed content, not windowing.
			blob: []byte("ok\xF0\x9D\x84"), mime: "text/plain",
			offset: 0, maxBytes: 64, wantRefused: true,
		},
		{
			name: "an empty artifact reads as an empty window",
			blob: []byte{}, mime: "text/plain",
			offset: 0, maxBytes: 16, wantContent: "", wantOffset: 0,
		},
		{
			name: "an offset past the end reads as an empty window",
			blob: []byte("short"), mime: "text/plain",
			offset: 99, maxBytes: 16, wantContent: "", wantOffset: 99,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := seedArtifactMIME(t, store, tc.blob, tc.mime)
			out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
				Ref: ref.ID, Offset: tc.offset, MaxBytes: tc.maxBytes,
			})
			if err != nil {
				t.Fatalf("artifactFetch: hard error %v, want a soft outcome", err)
			}
			if tc.wantRefused {
				if out.Error == "" {
					t.Fatalf("an inadmissible window was ADMITTED: content = %q", out.Content)
				}
				// The U+FFFD rewrite must never reach a caller.
				if strings.ContainsRune(out.Content, utf8.RuneError) {
					t.Fatalf("the refusal still delivered replacement characters: %q", out.Content)
				}
				return
			}
			if out.Error != "" {
				t.Fatalf("an admissible window was REFUSED: %s", out.Error)
			}
			if out.Content != tc.wantContent {
				t.Errorf("Content = %q, want %q", out.Content, tc.wantContent)
			}
			if out.Offset != tc.wantOffset {
				t.Errorf("Offset = %d, want %d", out.Offset, tc.wantOffset)
			}
			if out.ReturnedBytes != int64(len(tc.wantContent)) {
				t.Errorf("ReturnedBytes = %d, want %d", out.ReturnedBytes, len(tc.wantContent))
			}
		})
	}
}

// TestArtifactFetch_Refusal_NamesMIMEOffsetAndTheByReferenceRoute pins
// the refusal's SHAPE, not only that it refuses. A wall teaches a model
// nothing; the auto-materialisation path stamps a fetch hint onto every
// over-threshold binary attachment, so a model WILL arrive here and its
// next decision needs a destination.
func TestArtifactFetch_Refusal_NamesMIMEOffsetAndTheByReferenceRoute(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := append([]byte("%PDF-1.7\n"), 0xDE, 0xAD, 0xBE, 0xEF)
	ref := seedArtifactMIME(t, store, blob, "application/pdf")

	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: ref.ID})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if out.Error == "" {
		t.Fatal("a PDF was returned as text")
	}
	// (a) the stored MIME and (b) the failing absolute byte offset.
	if !strings.Contains(out.Error, "application/pdf") {
		t.Errorf("refusal does not name the stored MIME: %q", out.Error)
	}
	// Byte 11, not byte 9: the first two binary bytes (0xDE 0xAD) happen
	// to be a well-formed two-byte encoding of U+07AD, so admissibility
	// fails at the first byte that genuinely cannot decode. That is the
	// point of a CONTENT-driven check — the offset it reports is the one
	// the bytes actually break at, not the one the MIME suggests.
	if !strings.Contains(out.Error, "byte 11") {
		t.Errorf("refusal does not name the failing byte offset 11: %q", out.Error)
	}
	// (c) the by-reference route, so the next decision has a destination.
	if !strings.Contains(out.Error, "artifact-reference parameter") {
		t.Errorf("refusal does not name the by-reference route: %q", out.Error)
	}
	if !strings.Contains(out.Error, ref.ID) {
		t.Errorf("refusal does not name the id to pass on: %q", out.Error)
	}

	// The populated / zero-valued split is PINNED, not left to fall out
	// of a struct literal: a refusal reporting `truncated: true` would
	// invite a model to page into the same wall forever.
	if out.Ref != ref.ID {
		t.Errorf("Ref = %q, want %q — a refusal still identifies what it refused", out.Ref, ref.ID)
	}
	if out.MIME != "application/pdf" {
		t.Errorf("MIME = %q, want application/pdf", out.MIME)
	}
	if out.SizeBytes != int64(len(blob)) || out.TotalSizeBytes != int64(len(blob)) {
		t.Errorf("SizeBytes/TotalSizeBytes = %d/%d, want %d for both", out.SizeBytes, out.TotalSizeBytes, len(blob))
	}
	if out.Content != "" {
		t.Errorf("Content = %q on a refusal, want empty", out.Content)
	}
	if out.Offset != 0 || out.ReturnedBytes != 0 || out.Truncated {
		t.Errorf("windowing fields on a refusal = (offset %d, returned %d, truncated %v), want all zero-valued",
			out.Offset, out.ReturnedBytes, out.Truncated)
	}
}

// TestArtifactFetch_RefusalSurvivesJSONEncoding is the measurement the
// phase rests on: the corruption is invisible in Go and appears only
// after `encoding/json`. This asserts the pre-phase shape is gone by
// encoding the response the way the runtime does.
func TestArtifactFetch_RefusalSurvivesJSONEncoding(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := []byte("\x89PNG\r\n\x1a\n")
	ref := seedArtifactMIME(t, store, blob, "image/png")

	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: ref.ID})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var round ArtifactFetchOut
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if round.Content != "" {
		t.Fatalf("content survived to the model as %q — the U+FFFD rewrite still reaches a caller", round.Content)
	}
	if round.Error == "" {
		t.Fatal("the refusal did not survive JSON encoding")
	}
	// The pre-phase build reported `returned_bytes: 8` for this 8-byte
	// header while DELIVERING 10 bytes — every invalid byte becomes the
	// 3-byte encoding of U+FFFD. Measured, not reasoned: the assertion
	// below records that the arithmetic is unreachable now.
	if round.ReturnedBytes != 0 {
		t.Fatalf("ReturnedBytes = %d on a refusal, want 0", round.ReturnedBytes)
	}
	if n := len([]byte(prePhaseWindow(blob))); n != 10 {
		t.Fatalf("the pre-phase conversion of this fixture is %d bytes, want the recorded 10", n)
	}
}

// prePhaseWindow reproduces the conversion this phase replaces —
// `string(window)` on raw bytes — so the corruption it caused can be
// MEASURED in a test rather than asserted from memory.
func prePhaseWindow(window []byte) string {
	var sb strings.Builder
	for _, r := range string(window) {
		sb.WriteRune(r)
	}
	return sb.String()
}

// TestArtifactFetch_ReassemblyAndProgressInvariants asserts the two
// load-bearing properties over the FULL (offset x max_bytes)
// cross-product of a mixed-rune-width fixture, rather than over a
// hand-picked table:
//
//	reassembly:      Content == blob[Offset : Offset+ReturnedBytes]
//	strict progress: Truncated implies Offset+ReturnedBytes > requested offset
//
// The first is what makes a paged read reassemble without gaps or
// duplicates; the second is what makes the paging loop terminate.
func TestArtifactFetch_ReassemblyAndProgressInvariants(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := mixedWidthFixture()
	total := int64(len(blob))
	ref := seedArtifactMIME(t, store, blob, "text/plain")

	for offset := int64(0); offset <= total+2; offset++ {
		for maxBytes := 1; maxBytes <= len(blob)+1; maxBytes++ {
			out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
				Ref: ref.ID, Offset: int(offset), MaxBytes: maxBytes,
			})
			if err != nil {
				t.Fatalf("offset=%d max=%d: hard error %v", offset, maxBytes, err)
			}
			if out.Error != "" {
				t.Fatalf("offset=%d max=%d: a valid-UTF-8 fixture was refused: %s", offset, maxBytes, out.Error)
			}
			if out.Offset < offset {
				t.Fatalf("offset=%d max=%d: reported Offset %d moved BACKWARDS", offset, maxBytes, out.Offset)
			}
			end := out.Offset + out.ReturnedBytes
			if out.Offset <= total {
				if end > total {
					t.Fatalf("offset=%d max=%d: window [%d,%d) runs past the artifact (%d bytes)",
						offset, maxBytes, out.Offset, end, total)
				}
				// The reassembly invariant.
				if want := string(blob[out.Offset:end]); out.Content != want {
					t.Fatalf("offset=%d max=%d: Content = %q, want blob[%d:%d] = %q",
						offset, maxBytes, out.Content, out.Offset, end, want)
				}
			}
			// `truncated` is self-consistent with the window it reports.
			if wantTrunc := end < total; out.Truncated != wantTrunc {
				t.Fatalf("offset=%d max=%d: Truncated = %v, want %v (window ends at %d of %d)",
					offset, maxBytes, out.Truncated, wantTrunc, end, total)
			}
			// The strict-progress invariant — what kills the livelock.
			if out.Truncated && end <= offset {
				t.Fatalf("offset=%d max=%d: LIVELOCK — truncated read advances to %d, not past %d",
					offset, maxBytes, end, offset)
			}
		}
	}
}

// TestArtifactFetch_PagingTerminatesForEveryMaxBytes walks the artifact
// exactly the way the tool's description tells a model to, for every
// legal max_bytes, and asserts byte-exact reassembly under a HARD
// iteration cap that fails the test rather than hanging it.
//
// The cap is the provable one: a window is floored at utf8.UTFMax (4)
// and the tail trim removes at most utf8.UTFMax-1 (3) bytes, so each
// truncated read advances at least max(1, max_bytes-3) bytes. See the
// phase plan's recorded departure — the plan's `ceil(total/4)+1` is
// arithmetically unreachable at max_bytes=4 over mixed rune widths,
// where a window can legitimately advance a single byte.
func TestArtifactFetch_PagingTerminatesForEveryMaxBytes(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := mixedWidthFixture()
	total := len(blob)
	ref := seedArtifactMIME(t, store, blob, "text/plain")

	for maxBytes := 1; maxBytes <= total+1; maxBytes++ {
		step := maxBytes - (utf8.UTFMax - 1)
		if step < 1 {
			step = 1
		}
		iterCap := (total+step-1)/step + 1

		var assembled []byte
		offset := 0
		for i := 0; ; i++ {
			if i > iterCap {
				t.Fatalf("max_bytes=%d: paging exceeded the provable cap of %d iterations at offset %d",
					maxBytes, iterCap, offset)
			}
			out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
				Ref: ref.ID, Offset: offset, MaxBytes: maxBytes,
			})
			if err != nil {
				t.Fatalf("max_bytes=%d offset=%d: %v", maxBytes, offset, err)
			}
			if out.Error != "" {
				t.Fatalf("max_bytes=%d offset=%d: refused a valid-UTF-8 fixture: %s", maxBytes, offset, out.Error)
			}
			assembled = append(assembled, out.Content...)
			if !out.Truncated {
				break
			}
			next := int(out.Offset + out.ReturnedBytes)
			if next <= offset {
				t.Fatalf("max_bytes=%d: LIVELOCK — next offset %d does not advance past %d", maxBytes, next, offset)
			}
			offset = next
		}
		if !bytes.Equal(assembled, blob) {
			t.Fatalf("max_bytes=%d: paged read assembled %q, want %q", maxBytes, assembled, blob)
		}
	}
}

// TestArtifactFetch_PagingBound_IsTheProvableOne is the measurement
// behind this phase's recorded departure from its plan.
//
// The plan states a termination bound of ceil(total/4)+1 iterations. The
// window floor guarantees a window of at least 4 bytes, but NOT that 4
// bytes of PROGRESS are made: the tail trim removes up to 3, so an
// artifact alternating a 1-byte rune with a 4-byte one advances 1 byte
// on every other call. This fixture measures exactly that — 100 bytes at
// max_bytes=4 takes 40 iterations where the plan's bound allows 26 — so
// the shipped guard is the provable bound max(1, max_bytes-3) instead.
// The property that matters (termination, and byte-exact reassembly) is
// unaffected; only the constant is.
func TestArtifactFetch_PagingBound_IsTheProvableOne(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	// The adversarial shape: one ASCII byte then one 4-byte rune, so a
	// 4-byte window alternates between returning 1 byte and 4.
	blob := []byte(strings.Repeat("a\U0001D11E", 20))
	total := len(blob)
	ref := seedArtifactMIME(t, store, blob, "text/plain")

	const maxBytes = utf8.UTFMax
	provableCap := total/(maxBytes-(utf8.UTFMax-1)) + 1

	var assembled []byte
	offset, iters := 0, 0
	for {
		iters++
		if iters > provableCap {
			t.Fatalf("paging exceeded the provable cap of %d iterations at offset %d", provableCap, offset)
		}
		out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
			Ref: ref.ID, Offset: offset, MaxBytes: maxBytes,
		})
		if err != nil || out.Error != "" {
			t.Fatalf("offset=%d: err=%v soft=%q", offset, err, out.Error)
		}
		assembled = append(assembled, out.Content...)
		if !out.Truncated {
			break
		}
		next := int(out.Offset + out.ReturnedBytes)
		if next <= offset {
			t.Fatalf("LIVELOCK — next offset %d does not advance past %d", next, offset)
		}
		offset = next
	}
	if !bytes.Equal(assembled, blob) {
		t.Fatalf("paged read assembled %q, want %q", assembled, blob)
	}
	t.Logf("total=%d max_bytes=%d took %d iterations (the plan's ceil(total/4)+1 = %d)",
		total, maxBytes, iters, (total+3)/4+1)
}

// TestArtifactFetch_ASCIIWindowsAreByteIdenticalToThePrePhaseBuild pins
// that the rune discipline is INERT on content that was already
// admissible. The pre-phase build's window was `blob[offset:offset+n]`
// clipped to the blob; for ASCII that is what still comes back, at every
// offset and every bound at or above the floor.
func TestArtifactFetch_ASCIIWindowsAreByteIdenticalToThePrePhaseBuild(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := []byte("The quick brown fox jumps over the lazy dog. 0123456789")
	total := len(blob)
	ref := seedArtifactMIME(t, store, blob, "text/plain")

	for offset := 0; offset <= total; offset++ {
		for maxBytes := utf8.UTFMax; maxBytes <= total+1; maxBytes++ {
			out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
				Ref: ref.ID, Offset: offset, MaxBytes: maxBytes,
			})
			if err != nil || out.Error != "" {
				t.Fatalf("offset=%d max=%d: err=%v soft=%q", offset, maxBytes, err, out.Error)
			}
			end := offset + maxBytes
			if end > total {
				end = total
			}
			if want := string(blob[offset:end]); out.Content != want {
				t.Fatalf("offset=%d max=%d: Content = %q, want the pre-phase window %q",
					offset, maxBytes, out.Content, want)
			}
			if out.Offset != int64(offset) {
				t.Fatalf("offset=%d max=%d: Offset = %d, want the requested offset (nothing was trimmed)",
					offset, maxBytes, out.Offset)
			}
		}
	}
}

// TestArtifactFetch_MultiByteWindowsAreByteIdenticalWhereTheyWereValid
// is the same no-regression pin for content the pre-phase build already
// returned correctly: a window that happens to land on rune boundaries
// is unchanged, including one carrying 2-, 3- and 4-byte runes.
func TestArtifactFetch_MultiByteWindowsAreByteIdenticalWhereTheyWereValid(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := mixedWidthFixture()
	ref := seedArtifactMIME(t, store, blob, "text/plain")

	// Every offset that begins a rune, read to the end of the artifact:
	// no trimming applies, so the answer is the naive window verbatim.
	for offset := range blob {
		if !utf8.RuneStart(blob[offset]) {
			continue
		}
		out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
			Ref: ref.ID, Offset: offset, MaxBytes: len(blob),
		})
		if err != nil || out.Error != "" {
			t.Fatalf("offset=%d: err=%v soft=%q", offset, err, out.Error)
		}
		if want := string(blob[offset:]); out.Content != want {
			t.Fatalf("offset=%d: Content = %q, want the pre-phase window %q", offset, out.Content, want)
		}
		if out.Truncated {
			t.Fatalf("offset=%d: Truncated = true on a read to the end", offset)
		}
	}
}

// TestFetchBounds_EffectiveMaxFloorsAtUTFMax covers the floor on every
// leg it can enter by: the caller's own bound, the operator-resolved
// DEFAULT, and the operator-resolved CEILING — plus its interaction with
// the existing clamp-down-to-ceiling behaviour, which must still clamp.
func TestFetchBounds_EffectiveMaxFloorsAtUTFMax(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		inDefault, inHard int
		requested         int
		want              int
	}{
		{"a caller bound of 1 is raised to the floor", 64, 512, 1, utf8.UTFMax},
		{"a caller bound of 3 is raised to the floor", 64, 512, 3, utf8.UTFMax},
		{"a caller bound of 4 is served as asked", 64, 512, 4, 4},
		{"a caller bound of 5 is served as asked", 64, 512, 5, 5},
		{"an operator DEFAULT of 2 is raised to the floor", 2, 512, 0, utf8.UTFMax},
		{"an operator CEILING of 3 is raised to the floor", 3, 3, 1024, utf8.UTFMax},
		{"the clamp DOWN to the ceiling still applies above the floor", 64, 128, 1 << 20, 128},
		{"an omitted bound still takes the operator default", 64, 512, 0, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := resolveFetchBounds(tc.inDefault, tc.inHard)
			if got := b.effectiveMax(tc.requested); got != tc.want {
				t.Errorf("effectiveMax(%d) with (default %d, hard %d) = %d, want %d",
					tc.requested, tc.inDefault, tc.inHard, got, tc.want)
			}
		})
	}
}

// TestArtifactFetch_SubRuneBoundServedAtTheFloor is the floor's
// end-to-end consequence and the livelock's direct regression gate: the
// pre-phase build tail-trimmed this window to empty while reporting
// truncated=true, and the documented paging rule then yielded the same
// offset forever.
func TestArtifactFetch_SubRuneBoundServedAtTheFloor(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	blob := []byte("\U0001D11E\U0001D11E\U0001D11E") // three 4-byte runes
	ref := seedArtifactMIME(t, store, blob, "text/plain")

	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
		Ref: ref.ID, MaxBytes: 1,
	})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("a sub-rune bound was refused: %s", out.Error)
	}
	if out.ReturnedBytes == 0 {
		t.Fatal("a sub-rune bound returned an empty window — the pre-phase livelock shape")
	}
	if out.Content != "\U0001D11E" {
		t.Errorf("Content = %q, want one whole rune", out.Content)
	}
	if !out.Truncated {
		t.Error("Truncated = false with two runes remaining")
	}
}

// TestAdmissibleWindow_TrimHelpers covers the two trim decisions
// directly, including the cases where a fragment is CONTENT rather than
// a windowing artefact — the distinction the whole gate rests on.
func TestAdmissibleWindow_TrimHelpers(t *testing.T) {
	t.Parallel()

	t.Run("leadingContinuationBytes", func(t *testing.T) {
		for _, tc := range []struct {
			in   []byte
			want int
		}{
			{[]byte("abc"), 0},
			{[]byte("\x9E"), 1},
			{[]byte("\x9D\x84\x9Ea"), 3},
			// Capped: a fourth continuation byte cannot belong to one
			// rune's tail, so it is left for the validity check to refuse.
			{[]byte("\x80\x80\x80\x80"), 3},
			{[]byte{}, 0},
		} {
			if got := leadingContinuationBytes(tc.in); got != tc.want {
				t.Errorf("leadingContinuationBytes(%x) = %d, want %d", tc.in, got, tc.want)
			}
		}
	})

	t.Run("incompleteRuneSuffix", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   []byte
			want int
		}{
			{"ends on an ASCII boundary", []byte("abc"), 0},
			{"ends on a complete two-byte rune", []byte("aé"), 0},
			{"ends on a complete four-byte rune", []byte("a\U0001D11E"), 0},
			{"one byte of a three-byte rune", []byte("a\xE2"), 1},
			{"two bytes of a three-byte rune", []byte("a\xE2\x98"), 2},
			{"three bytes of a four-byte rune", []byte("a\xF0\x9D\x84"), 3},
			// A continuation byte after ASCII is malformed content, not a
			// split — 0 so the validity check refuses it.
			{"a stray continuation after ASCII is content", []byte("ab\x80"), 0},
			{"an invalid leading byte is content", []byte("ab\xFF"), 0},
			{"empty", []byte{}, 0},
		} {
			if got := incompleteRuneSuffix(tc.in); got != tc.want {
				t.Errorf("%s: incompleteRuneSuffix(%x) = %d, want %d", tc.name, tc.in, got, tc.want)
			}
		}
	})

	t.Run("firstInvalidByteIndex", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   []byte
			want int
		}{
			{"valid ASCII", []byte("hello"), -1},
			{"valid mixed widths", mixedWidthFixture(), -1},
			// A correctly ENCODED U+FFFD is valid text and must not be
			// mistaken for a decode failure.
			{"an encoded U+FFFD is valid", []byte("a�b"), -1},
			{"a bad byte reports its index", []byte("ab\xFFcd"), 2},
			{"a truncated rune reports its index", []byte("ab\xE2\x98"), 2},
		} {
			if got := firstInvalidByteIndex(tc.in); got != tc.want {
				t.Errorf("%s: firstInvalidByteIndex(%x) = %d, want %d", tc.name, tc.in, got, tc.want)
			}
		}
	})
}

// TestArtifactFetch_ConcurrentReuse_N128 is the D-025 gate.
// `artifact_fetch` is a compiled artifact: one shared store, N=128
// concurrent invocations across two tenants, interleaving admissible
// reads, refusals and cross-tenant misses. Asserts no data race (the
// -race detector is the gate), no CONTENT bleed across tenants, no
// OUTCOME bleed (one goroutine's refusal must not be attributed to a
// sibling's success), no cancellation cross-talk, and no goroutine leak.
func TestArtifactFetch_ConcurrentReuse_N128(t *testing.T) {
	store := artifactFetchTestStore(t)
	bounds := defaultTestFetchBounds()

	textA := []byte("tenant-A text é☃𝄞 payload")
	binA := []byte("\x89PNG\r\n\x1a\n\xDE\xAD\xBE\xEF")
	refTextA := seedArtifactMIME(t, store, textA, "text/plain")
	refBinA := seedArtifactMIME(t, store, binA, "image/png")

	textB := []byte("tenant-B text é☃𝄞 payload")
	refTextB, err := store.PutBytes(t.Context(),
		artifacts.ArtifactScope{TenantID: "tB", UserID: "uB", SessionID: "sB"},
		textB, artifacts.PutOpts{Namespace: "test.fixture", MimeType: "text/plain"})
	if err != nil {
		t.Fatalf("PutBytes (tenant B): %v", err)
	}

	baseline := runtime.NumGoroutine()

	const n = 128
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0: // tenant A reads its own text
				ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r"+strconv.Itoa(i))
				out, err := artifactFetch(ctx, store, bounds, ArtifactFetchArgs{Ref: refTextA.ID})
				if err != nil || out.Error != "" {
					errs <- "A text: hard=" + errStr(err) + " soft=" + out.Error
					return
				}
				if out.Content != string(textA) {
					errs <- "A text: content bleed: " + out.Content
				}
			case 1: // tenant A reads its own binary — must refuse
				ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r"+strconv.Itoa(i))
				out, err := artifactFetch(ctx, store, bounds, ArtifactFetchArgs{Ref: refBinA.ID})
				if err != nil {
					errs <- "A bin: hard=" + errStr(err)
					return
				}
				if out.Error == "" {
					errs <- "A bin: an inadmissible window was admitted: " + out.Content
					return
				}
				if out.Content != "" {
					errs <- "A bin: outcome bleed — a refusal carried content: " + out.Content
				}
			case 2: // tenant B reads its own text
				ctx := artifactFetchTestCtx(t, "tB", "uB", "sB", "r"+strconv.Itoa(i))
				out, err := artifactFetch(ctx, store, bounds, ArtifactFetchArgs{Ref: refTextB.ID})
				if err != nil || out.Error != "" {
					errs <- "B text: hard=" + errStr(err) + " soft=" + out.Error
					return
				}
				if out.Content != string(textB) {
					errs <- "B text: content bleed: " + out.Content
				}
			default: // tenant B reaches for tenant A's ref — indistinguishable miss
				ctx := artifactFetchTestCtx(t, "tB", "uB", "sB", "r"+strconv.Itoa(i))
				out, err := artifactFetch(ctx, store, bounds, ArtifactFetchArgs{Ref: refTextA.ID})
				if err != nil {
					errs <- "B cross: hard=" + errStr(err)
					return
				}
				if out.Content != "" {
					errs <- "SECURITY: cross-tenant content bleed: " + out.Content
					return
				}
				if !strings.Contains(out.Error, "not found") {
					errs <- "B cross: want the indistinguishable not-found shape, got: " + out.Error
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}

	// Cancellation cross-talk: one invocation's cancelled ctx must not
	// disturb a sibling running against the same registration and store.
	cancelledCtx, cancel := context.WithCancel(artifactFetchTestCtx(t, "tA", "uA", "sA", "rc"))
	cancel()
	_, _ = artifactFetch(cancelledCtx, store, bounds, ArtifactFetchArgs{Ref: refTextA.ID})
	out, err := artifactFetch(artifactFetchTestCtx(t, "tA", "uA", "sA", "rl"), store, bounds,
		ArtifactFetchArgs{Ref: refTextA.ID})
	if err != nil || out.Error != "" || out.Content != string(textA) {
		t.Fatalf("a sibling invocation was disturbed by a cancelled one: hard=%v soft=%q content=%q",
			err, out.Error, out.Content)
	}

	if grew := runtime.NumGoroutine() - baseline; grew > 4 {
		t.Errorf("goroutine count grew by %d after 128 invocations — the builtin leaks", grew)
	}
}

// errStr renders an error for a channel-collected assertion message
// without a nil dereference.
func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

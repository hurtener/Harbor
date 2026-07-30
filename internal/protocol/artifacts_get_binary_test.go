package protocol_test

import (
	"bytes"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// The trip-wire for the deliberate divergence between this byte read and
// the model-facing artifact-fetch built-in.
//
// The built-in trims its window to whole UTF-8 runes and REFUSES a
// window that is not valid text, because it returns `content` as a
// string that `encoding/json` sanitises on the way to a model. This
// response carries `[]byte`, which is base64 over the wire and therefore
// byte-exact for every MIME. Propagating the tool's rune discipline here
// would short-read an operator's PDF — silently, at a length the
// response misreports.
//
// The risk is that a future contributor finds the two window helpers
// disagreeing and "restores consistency" in the wrong direction. This is
// the test that catches that refactor. Its counterpart argument lives in
// both helpers' godoc.

// binaryFixture is content that is deliberately NOT valid UTF-8 at
// several positions: a PNG magic number, a lone continuation byte, a
// truncated multi-byte rune and a 0xFF that can lead no rune at all.
func binaryFixture() []byte {
	return []byte("\x89PNG\r\n\x1a\n" + "\x80\x80" + "\xF0\x9D\x84" + "\xFF\xFE" + "tail")
}

// TestArtifactsGetHandler_BinaryBytesAreExactAtEveryOffset pins that the
// Protocol byte read returns EXACTLY the requested range for binary
// content, at every offset and every bound — no trimming, no refusal, no
// replacement characters.
func TestArtifactsGetHandler_BinaryBytesAreExactAtEveryOffset(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := binaryFixture()
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{MimeType: "image/png"})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	total := int64(len(payload))
	for offset := int64(0); offset <= total; offset++ {
		for maxBytes := int64(1); maxBytes <= total+1; maxBytes++ {
			got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{
				Scope: scope, ID: ref.ID, Offset: offset, MaxBytes: maxBytes,
			})
			end := offset + maxBytes
			if end > total {
				end = total
			}
			want := payload[offset:end]
			if !bytes.Equal(got.Content, want) {
				t.Fatalf("offset=%d max_bytes=%d: content = %x, want the exact range %x",
					offset, maxBytes, got.Content, want)
			}
			if got.Offset != offset {
				t.Fatalf("offset=%d max_bytes=%d: offset = %d — the byte read must NOT advance past a rune split",
					offset, maxBytes, got.Offset)
			}
			if got.ReturnedBytes != int64(len(want)) {
				t.Fatalf("offset=%d max_bytes=%d: returned_bytes = %d, want %d",
					offset, maxBytes, got.ReturnedBytes, len(want))
			}
		}
	}
}

// TestArtifactsGetHandler_BinaryPagesToAByteExactReassembly is the same
// property read end to end: a caller following the response's own paging
// contract reassembles a binary artifact byte-for-byte, including at a
// sub-rune bound of 1 — the bound the model-facing tool deliberately
// floors at 4 and this one deliberately does not.
func TestArtifactsGetHandler_BinaryPagesToAByteExactReassembly(t *testing.T) {
	t.Parallel()
	s := newBoundedArtifactsSurface(t, newInMemStore(t), 1, 1)
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := binaryFixture()
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{MimeType: "image/png"})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	var assembled []byte
	offset := int64(0)
	for i := 0; ; i++ {
		if i > len(payload)+1 {
			t.Fatal("paging a binary artifact did not terminate")
		}
		got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID, Offset: offset})
		if got.ReturnedBytes != 1 && got.Truncated {
			t.Fatalf("a one-byte bound returned %d bytes — the byte read acquired a rune floor",
				got.ReturnedBytes)
		}
		assembled = append(assembled, got.Content...)
		if !got.Truncated {
			break
		}
		offset += got.ReturnedBytes
	}
	if !bytes.Equal(assembled, payload) {
		t.Fatalf("paged binary read assembled %x, want %x", assembled, payload)
	}
}

package audit

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// ArtifactRef is the canonical reference-shaped form of binary
// content per RFC §6.5 / D-021. Phase 03 ships only the type so the
// redactor can recognise refs and pass them through; the artifact
// store + materializer phases own the resolver. The type lives in
// this package to avoid a circular import — `internal/audit` is
// upstream of `internal/artifacts`.
type ArtifactRef struct {
	Ref       string `json:"artifact_ref" yaml:"artifact_ref"`
	MIME      string `json:"mime,omitempty" yaml:"mime,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty" yaml:"size_bytes,omitempty"`
	Hash      string `json:"hash,omitempty" yaml:"hash,omitempty"`
}

// isArtifactRef reports whether v is an ArtifactRef value (including
// pointer / interface wrapping). Recognised refs are passed through
// unredacted by every rule.
func isArtifactRef(v any) bool {
	if v == nil {
		return false
	}
	switch v.(type) {
	case ArtifactRef, *ArtifactRef:
		return true
	}
	return false
}

// dataURLPattern matches RFC 2397 data URLs:
//
//	data:[<mime>][;base64],<data>
//
// The capture groups are (1) MIME type, (2) ";base64" or empty,
// (3) the encoded payload.
var dataURLPattern = regexp.MustCompile(
	`(?i)data:([a-z0-9.+\-]+/[a-z0-9.+\-]+)?(;base64)?,([A-Za-z0-9+/=._\-]+)`)

// rawBase64ImagePattern is a heuristic for naked base64-encoded
// image content (no data: prefix). The threshold is intentionally
// high (>= 256 base64 chars ~ 192 bytes) so we don't false-positive
// on short ID-like strings. Real inline images are kilobytes.
const rawBase64MinLen = 256

// commonImageHeaderPrefixes are the base64 prefixes of common image /
// audio / video MIME types when the raw bytes (PNG / JPEG / WebP /
// GIF / WAV / MP3 / MP4) are encoded. Detection is conservative —
// missing a format means the redactor falls back to the key-based
// rules; false-positives are NOT acceptable.
var commonImageHeaderPrefixes = map[string]string{
	"iVBORw0KGgo":     "image/png",
	"/9j/":            "image/jpeg",
	"R0lGOD":          "image/gif",
	"UklGR":           "image/webp",
	"AAABAA":          "image/x-icon",
	"SUkqAA":          "image/tiff",
	"TU0AKgA":         "image/tiff",
	"//uQ":            "audio/mp3",
	"//tA":            "audio/mp3",
	"UklGRiQ":         "audio/wav",
	"AAAAIGZ0eXA":     "video/mp4",
	"AAAAGGZ0eXA":     "video/mp4",
}

// multimodalRule rewrites inline base64 / DataURL content in payload
// to a `[redacted: <MIME> of <N> bytes]` placeholder. ArtifactRef
// values pass through unchanged. Implements decisions D-021/D-022.
type multimodalRule struct {
	name string
}

func (r *multimodalRule) Name() string {
	if r.name == "" {
		return "multimodal"
	}
	return r.name
}

func (r *multimodalRule) Apply(_ context.Context, payload any) (any, error) {
	return walkReplaceStrings(payload, replaceInlineMedia, 0)
}

// replaceInlineMedia walks a single string value and rewrites any
// inline media to the redacted-placeholder form.
func replaceInlineMedia(s string) string {
	// 1. data: URLs (most common shape).
	s = dataURLPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := dataURLPattern.FindStringSubmatch(match)
		if len(groups) < 4 {
			return match
		}
		mime := groups[1]
		if mime == "" {
			mime = "application/octet-stream"
		}
		payload := groups[3]
		size := decodedSize(payload, groups[2] == ";base64")
		return fmt.Sprintf("[redacted: %s of %d bytes]", mime, size)
	})
	// 2. Naked base64 over the threshold with a known header prefix.
	if len(s) >= rawBase64MinLen {
		if mime, ok := detectBase64Header(s); ok {
			size := decodedSize(s, true)
			return fmt.Sprintf("[redacted: %s of %d bytes]", mime, size)
		}
	}
	return s
}

func decodedSize(s string, isBase64 bool) int {
	if !isBase64 {
		return len(s)
	}
	// base64 size is roughly 4/3 of binary size; recover the
	// approximate binary length, accounting for "=" padding.
	stripped := strings.TrimRight(s, "=")
	pad := len(s) - len(stripped)
	dec, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(stripped)
	if err != nil {
		// Fall back to length-based estimation rather than failing —
		// this is a redaction summary, not a load-bearing decode.
		return (len(stripped)*3)/4 - pad
	}
	return len(dec)
}

func detectBase64Header(s string) (string, bool) {
	// Inspect the first 16 chars — every recognised prefix fits.
	probe := s
	if len(probe) > 16 {
		probe = probe[:16]
	}
	for prefix, mime := range commonImageHeaderPrefixes {
		if strings.HasPrefix(probe, prefix) {
			return mime, true
		}
	}
	return "", false
}

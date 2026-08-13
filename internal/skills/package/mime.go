package skillpkg

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// mime.go — the canonical support-MIME allowlist for complete skill
// packages, and the bounded content validation that makes MIME a
// claim about BYTES, not a filename-suffix lookup.
//
// The v1.28 allowlist is deliberately narrow and closed: every
// retained type has an honest, bounded content check a validator can
// run over the file's (already decompression-bounded) bytes. There is
// no operator override knob, because the package identity (the
// versioned `PackageHash`) must mean the same thing to every
// resolver: a support file's recorded MIME is the MIME its bytes
// actually satisfy.
//
// The classification flow at every boundary (archive ingest, DTO
// validation with materialized bytes) is:
//
//  1. the canonical path's extension PROPOSES a MIME from the
//     allowlist (an extension with no entry — or no extension at all
//     — is unsupported and rejected);
//  2. the bounded bytes are validated against the proposed MIME
//     (UTF-8 + syntax/content checks for the text and structured
//     types; magic + basic shape for the binary types);
//  3. a proposal the bytes do not satisfy is rejected as a MIME
//     CONTENT MISMATCH — the recorded MIME is never claimed on the
//     strength of a suffix alone.
//
// Supported MIME version constant.
const (
	// MIMEAllowlistVersion names the closed allowlist revision this
	// table implements. Bumping the table (adding or removing a
	// retained type) MUST bump this string: it is part of the
	// semantic contract resolvers rely on.
	MIMEAllowlistVersion = "v1.28"
)

// extensionMIME maps a lowercase file extension (without the dot) to
// its canonical support MIME type. The set is the v1.28 allowlist:
//
//   - text/plain; charset=utf-8 and text/markdown — UTF-8 text with a
//     non-empty content check (the charset claim is verified);
//   - application/json — valid UTF-8 that parses as JSON;
//   - application/xml — valid UTF-8, well-formed XML;
//   - image/svg+xml — well-formed XML whose root element is `svg`;
//   - image/png, image/jpeg, image/gif, image/webp, image/avif,
//     application/pdf — binary signatures with a basic shape check;
//   - font/ttf, font/otf, font/woff, font/woff2 — binary signatures
//     with a minimum header-size check.
//
// Source-code dialects (text/x-go, text/x-python, ...), YAML/TOML/
// CSV/HTML/CSS and other formats with no honest bounded byte-level
// validator are intentionally NOT in the allowlist: claiming them
// would be extension-lookup masquerading as content truth.
var extensionMIME = map[string]string{
	// Documents.
	"md":       "text/markdown",
	"markdown": "text/markdown",
	"txt":      "text/plain; charset=utf-8",
	// Structured data.
	"json": "application/json",
	"xml":  "application/xml",
	"svg":  "image/svg+xml",
	// Images.
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"avif": "image/avif",
	// Documents (binary).
	"pdf": "application/pdf",
	// Fonts.
	"ttf":   "font/ttf",
	"otf":   "font/otf",
	"woff":  "font/woff",
	"woff2": "font/woff2",
}

// MIME sentinel errors. Compare via errors.Is.
var (
	// ErrMimeContentMismatch — the bytes do not satisfy the content
	// check of the MIME the canonical path proposed: an extension /
	// content mismatch, or bytes that are unsupported or ambiguous
	// for the proposed type (binary bytes under a text claim,
	// unparseable JSON/XML under a structured claim, a signature that
	// fails the claimed binary type, non-UTF-8 text, ...).
	ErrMimeContentMismatch = errors.New("skillpkg: support content does not match its declared MIME")
)

// mimeSupported reports whether mime is in the canonical allowlist.
func mimeSupported(mime string) bool {
	if mime == "" {
		return false
	}
	for _, candidate := range extensionMIME {
		if candidate == mime {
			return true
		}
	}
	return false
}

// classifyMIME returns the canonical support MIME PROPOSED by the
// canonical package path's last-segment extension, or "" when the
// file is outside the allowlist. The proposal is only a candidate:
// the bytes must satisfy ValidateMimeContent before the MIME is
// recorded. Extension-less files (including the root SKILL.md, which
// is handled by the root-entry checks rather than the support
// manifest) are unsupported as support files.
func classifyMIME(path string) string {
	last := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		last = path[i+1:]
	}
	dot := strings.LastIndexByte(last, '.')
	if dot < 0 || dot == len(last)-1 {
		return ""
	}
	ext := strings.ToLower(last[dot+1:])
	return extensionMIME[ext]
}

// supportMIME reports whether path classifies to a supported MIME.
func supportMIME(path string) (string, bool) {
	mime := classifyMIME(path)
	if mime == "" {
		return "", false
	}
	return mime, true
}

// ClassifySupportMIME reports the canonical support MIME a canonical
// package path proposes, and whether that proposal is inside the
// v1.28 allowlist. The proposal still must be validated from bytes
// (ValidateMimeContent) before a MIME is recorded — classification is
// the candidate, never the claim.
func ClassifySupportMIME(path string) (string, bool) {
	return supportMIME(path)
}

// ValidateMimeContent validates the bounded bytes of a support file
// against the MIME the package proposes for it. It is the "MIME is
// content truth" gate: every boundary that records a MIME for bytes
// (archive ingest, DTO validation with materialized Data) runs it,
// and a claim the bytes do not satisfy fails loudly with wrapped
// ErrMimeContentMismatch. The checks are bounded (linear in the
// already-bounded input; magic checks read only the header bytes).
func ValidateMimeContent(mime string, data []byte) error {
	switch mime {
	case "text/markdown", "text/plain; charset=utf-8":
		return validateTextContent(mime, data)
	case "application/json":
		if err := validateTextContent(mime, data); err != nil {
			return err
		}
		if !json.Valid(data) {
			return fmt.Errorf("%w: %s does not parse as JSON", ErrMimeContentMismatch, mime)
		}
		return nil
	case "application/xml":
		if err := validateTextContent(mime, data); err != nil {
			return err
		}
		return validateXMLWellFormed(mime, data)
	case "image/svg+xml":
		if err := validateTextContent(mime, data); err != nil {
			return err
		}
		if err := validateXMLWellFormed(mime, data); err != nil {
			return err
		}
		root, err := xmlRootElement(data)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrMimeContentMismatch, mime, err)
		}
		if root != "svg" {
			return fmt.Errorf("%w: %s root element is %q, want %q", ErrMimeContentMismatch, mime, root, "svg")
		}
		return nil
	case "image/png":
		return validateBinaryMagic(mime, data, pngCheck)
	case "image/jpeg":
		return validateBinaryMagic(mime, data, jpegCheck)
	case "image/gif":
		return validateBinaryMagic(mime, data, gifCheck)
	case "image/webp":
		return validateBinaryMagic(mime, data, webpCheck)
	case "image/avif":
		return validateBinaryMagic(mime, data, avifCheck)
	case "application/pdf":
		return validateBinaryMagic(mime, data, pdfCheck)
	case "font/ttf":
		return validateBinaryMagic(mime, data, ttfCheck)
	case "font/otf":
		return validateBinaryMagic(mime, data, otfCheck)
	case "font/woff":
		return validateBinaryMagic(mime, data, woffCheck)
	case "font/woff2":
		return validateBinaryMagic(mime, data, woff2Check)
	default:
		return fmt.Errorf("%w: %q is outside the %s allowlist", ErrMimeContentMismatch, mime, MIMEAllowlistVersion)
	}
}

// validateTextContent enforces the shared text claim: valid UTF-8, no
// NUL bytes, and at least one non-whitespace rune (an empty or
// whitespace-only file carries no content a text claim can be checked
// against — it is ambiguous data).
func validateTextContent(mime string, data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: %s carries non-UTF-8 bytes", ErrMimeContentMismatch, mime)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("%w: %s carries a NUL byte", ErrMimeContentMismatch, mime)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("%w: %s is empty or whitespace-only (ambiguous data)", ErrMimeContentMismatch, mime)
	}
	return nil
}

// validateXMLWellFormed asserts the bytes parse as well-formed XML via
// a bounded token walk: a single root element, balanced nesting,
// valid UTF-8, and no syntax errors. A text-only document (no root
// element) and multiple roots are rejected — a well-formed XML
// document has exactly one root.
func validateXMLWellFormed(mime string, data []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	rootSeen := false
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !rootSeen || depth != 0 {
					return fmt.Errorf("%w: %s is not well-formed XML", ErrMimeContentMismatch, mime)
				}
				return nil
			}
			return fmt.Errorf("%w: %s is not well-formed XML: %v", ErrMimeContentMismatch, mime, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if rootSeen {
					return fmt.Errorf("%w: %s has multiple root elements", ErrMimeContentMismatch, mime)
				}
				rootSeen = true
			}
			depth++
			_ = t
		case xml.EndElement:
			depth--
		}
	}
}

// xmlRootElement returns the local name of the document's root
// element, or an error when the bytes are not well-formed XML.
func xmlRootElement(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if se, ok := tok.(xml.StartElement); ok {
			name := se.Name.Local
			if i := strings.LastIndexByte(name, ':'); i >= 0 {
				name = name[i+1:]
			}
			return name, nil
		}
	}
}

// binaryCheck is one bounded signature + shape verifier.
type binaryCheck func(data []byte) error

func validateBinaryMagic(mime string, data []byte, check binaryCheck) error {
	if err := check(data); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrMimeContentMismatch, mime, err)
	}
	return nil
}

func needBytes(data []byte, n int) error {
	if len(data) < n {
		return fmt.Errorf("truncated: %d bytes, want >= %d", len(data), n)
	}
	return nil
}

func pngCheck(data []byte) error {
	if err := needBytes(data, 24); err != nil {
		return err
	}
	if !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return errors.New("missing PNG signature")
	}
	// Basic shape: the first chunk must be a 13-byte IHDR.
	length := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	if length != 13 || !bytes.Equal(data[12:16], []byte("IHDR")) {
		return errors.New("missing IHDR chunk")
	}
	return nil
}

func jpegCheck(data []byte) error {
	if err := needBytes(data, 4); err != nil {
		return err
	}
	if !bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}) {
		return errors.New("missing JPEG SOI marker")
	}
	if data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9 {
		return errors.New("missing JPEG EOI marker")
	}
	return nil
}

func gifCheck(data []byte) error {
	if err := needBytes(data, 6); err != nil {
		return err
	}
	if !bytes.Equal(data[:6], []byte("GIF87a")) && !bytes.Equal(data[:6], []byte("GIF89a")) {
		return errors.New("missing GIF signature")
	}
	return nil
}

func webpCheck(data []byte) error {
	if err := needBytes(data, 12); err != nil {
		return err
	}
	if !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return errors.New("missing RIFF/WEBP signature")
	}
	return nil
}

func avifCheck(data []byte) error {
	if err := needBytes(data, 12); err != nil {
		return err
	}
	if !bytes.Equal(data[4:8], []byte("ftyp")) {
		return errors.New("missing ftyp box")
	}
	brand := data[8:12]
	if !bytes.Equal(brand, []byte("avif")) && !bytes.Equal(brand, []byte("avis")) {
		return errors.New("missing avif brand")
	}
	return nil
}

func pdfCheck(data []byte) error {
	if err := needBytes(data, 5); err != nil {
		return err
	}
	if !bytes.Equal(data[:5], []byte("%PDF-")) {
		return errors.New("missing PDF signature")
	}
	return nil
}

func ttfCheck(data []byte) error {
	if err := needBytes(data, 4); err != nil {
		return err
	}
	// sfnt versions a TrueType file may carry.
	if !bytes.Equal(data[:4], []byte{0x00, 0x01, 0x00, 0x00}) &&
		!bytes.Equal(data[:4], []byte("true")) &&
		!bytes.Equal(data[:4], []byte("typ1")) {
		return errors.New("missing TrueType sfnt version")
	}
	return nil
}

func otfCheck(data []byte) error {
	if err := needBytes(data, 4); err != nil {
		return err
	}
	if !bytes.Equal(data[:4], []byte("OTTO")) {
		return errors.New("missing OpenType (OTTO) sfnt version")
	}
	return nil
}

func woffCheck(data []byte) error {
	if err := needBytes(data, 44); err != nil {
		return err
	}
	if !bytes.Equal(data[:4], []byte("wOFF")) {
		return errors.New("missing wOFF signature")
	}
	return nil
}

func woff2Check(data []byte) error {
	if err := needBytes(data, 48); err != nil {
		return err
	}
	if !bytes.Equal(data[:4], []byte("wOF2")) {
		return errors.New("missing wOF2 signature")
	}
	return nil
}

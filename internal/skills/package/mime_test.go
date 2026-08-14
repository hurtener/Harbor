package skillpkg_test

import (
	"errors"
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// TestMIMEAllowlist_ClosedAndNarrow pins the v1.28 allowlist: the
// retained set is deliberately small, every retained MIME has a real
// content check, and the previously extension-only dialects are gone.
func TestMIMEAllowlist_ClosedAndNarrow(t *testing.T) {
	if skillpkg.MIMEAllowlistVersion != "v1.28" {
		t.Fatalf("MIMEAllowlistVersion = %q, want v1.28", skillpkg.MIMEAllowlistVersion)
	}
	for _, ext := range []string{"md", "markdown", "txt", "json", "xml", "svg",
		"png", "jpg", "jpeg", "gif", "webp", "avif", "pdf", "ttf", "otf", "woff", "woff2"} {
		mime, ok := skillpkg.ClassifySupportMIME("file." + ext)
		if !ok {
			t.Fatalf("extension %q should classify", ext)
		}
		if !strings.Contains(mime, "/") {
			t.Fatalf("extension %q -> %q is not a MIME", ext, mime)
		}
	}
	// Dialects without an honest bounded content validator must NOT be
	// claimable from a suffix alone.
	for _, ext := range []string{"go", "py", "sh", "yaml", "yml", "toml", "csv", "html", "css",
		"rs", "c", "h", "java", "rb", "php", "jsonl", "ipynb", "sql", "proto", "ico", "bmp", "tiff"} {
		if _, ok := skillpkg.ClassifySupportMIME("file." + ext); ok {
			t.Fatalf("extension %q must not classify under the v1.28 allowlist", ext)
		}
	}
}

// TestValidateMimeContent_Accepted exercises the positive content
// check for every retained type: each canonical MIME accepts bytes
// that genuinely satisfy it.
func TestValidateMimeContent_Accepted(t *testing.T) {
	cases := []struct {
		mime string
		data string
	}{
		{"text/markdown", "# A heading\n\nbody text\n"},
		{"text/plain; charset=utf-8", "plain text\n"},
		{"application/json", `{"a": [1, 2, {"b": true}]}`},
		{"application/xml", "<?xml version=\"1.0\"?>\n<root><item>v</item></root>"},
		{"image/svg+xml", "<?xml version=\"1.0\"?>\n<svg xmlns=\"http://www.w3.org/2000/svg\"><circle/></svg>"},
		{"image/png", string(pngBytes())},
		{"image/jpeg", "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01stuff\xff\xd9"},
		{"image/gif", "GIF89a\x01\x00\x01\x00\x80\x00\x00data"},
		{"image/webp", "RIFF\x00\x00\x00\x00WEBPVP8 "},
		{"image/avif", "\x00\x00\x00\x18ftypavif\x00\x00\x00\x00avisdata"},
		{"application/pdf", "%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF"},
		{"font/ttf", "\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
		{"font/otf", "OTTO\x00\x00\x00\x00\x00\x00\x00\x00"},
		{"font/woff", "wOFF\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
		{"font/woff2", "wOF2\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
	}
	for _, c := range cases {
		t.Run(c.mime, func(t *testing.T) {
			if err := skillpkg.ValidateMimeContent(c.mime, []byte(c.data)); err != nil {
				t.Fatalf("ValidateMimeContent(%s): %v", c.mime, err)
			}
		})
	}
}

// TestValidateMimeContent_Rejected pins the extension / content
// mismatch and ambiguous-data rejections: a suffix alone never claims
// a MIME.
func TestValidateMimeContent_Rejected(t *testing.T) {
	cases := []struct {
		name string
		mime string
		data string
	}{
		// Text claims reject binary / non-UTF-8 / ambiguous data.
		{"markdown binary", "text/markdown", "\xff\xfe\x00binary"},
		{"markdown nul", "text/markdown", "text\x00with nul"},
		{"plain empty", "text/plain; charset=utf-8", ""},
		{"plain whitespace", "text/plain; charset=utf-8", "   \n\t"},
		{"markdown invalid utf8", "text/markdown", "caf\xc3\xa9 \xff\xfe"},
		// Structured claims reject unparseable content.
		{"json not json", "application/json", `{"a": }`},
		{"json binary", "application/json", "\x89PNG\r\n\x1a\n"},
		{"json array ok but string", "application/json", `not json`},
		{"xml unclosed", "application/xml", "<open>never closed"},
		{"xml not xml", "application/xml", "this is not xml"},
		{"svg root not svg", "image/svg+xml", "<html><body>x</body></html>"},
		{"svg not xml", "image/svg+xml", "svg content"},
		// Binary claims reject wrong / truncated signatures.
		{"png json bytes", "image/png", `{"demo": true}`},
		{"png truncated", "image/png", "\x89PNG\r\n\x1a\n"},
		{"png no ihdr", "image/png", "\x89PNG\r\n\x1a\nxxxx"},
		{"jpeg truncated", "image/jpeg", "\xff\xd8\xff"},
		{"jpeg no eoi", "image/jpeg", "\xff\xd8\xff\xe0xxxx"},
		{"gif bad magic", "image/gif", "GIF99a\x01\x00\x01\x00"},
		{"webp bad magic", "image/webp", "RIFF\x00\x00\x00\x00PNG "},
		{"avif no ftyp", "image/avif", "\x00\x00\x00\x18data\x00\x00\x00\x00"},
		{"pdf not pdf", "application/pdf", "MZ\x90\x00"},
		{"ttf wrong sfnt", "font/ttf", "\x00\x02\x00\x00\x00\x00"},
		{"otf not otto", "font/otf", "NOTO\x00\x00\x00\x00"},
		{"woff short", "font/woff", "wOFF"},
		{"woff2 wrong magic", "font/woff2", "wOFF\x00\x00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := skillpkg.ValidateMimeContent(c.mime, []byte(c.data))
			if err == nil {
				t.Fatalf("ValidateMimeContent(%s, %q): expected error", c.mime, c.data)
			}
			if !errors.Is(err, skillpkg.ErrMimeContentMismatch) {
				t.Fatalf("ValidateMimeContent(%s): err=%v, want ErrMimeContentMismatch", c.mime, err)
			}
		})
	}
}

// TestValidateMimeContent_UnknownMime rejects MIMEs outside the
// allowlist — the closed-table property.
func TestValidateMimeContent_UnknownMime(t *testing.T) {
	err := skillpkg.ValidateMimeContent("application/zip", []byte("PK\x03\x04x"))
	if !errors.Is(err, skillpkg.ErrMimeContentMismatch) {
		t.Fatalf("err=%v, want ErrMimeContentMismatch", err)
	}
}

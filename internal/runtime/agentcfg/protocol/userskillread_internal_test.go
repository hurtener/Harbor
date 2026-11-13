package protocol

import (
	"strings"
	"testing"

	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

func TestSupportFileContent_TextBinaryAndTruncation(t *testing.T) {
	text := supportFileContent(skillpkg.SupportFile{Path: "a.md", Mime: "text/markdown", Size: 5}, []byte("hello"))
	if text.Binary || text.Truncated || text.Text != "hello" {
		t.Fatalf("text=%+v", text)
	}
	bin := supportFileContent(skillpkg.SupportFile{Path: "i.png", Mime: "image/png", Size: 3}, []byte{0x89, 'P', 'N'})
	if !bin.Binary || bin.Text != "" {
		t.Fatalf("binary=%+v", bin)
	}
	invalid := supportFileContent(skillpkg.SupportFile{Path: "b.txt", Mime: "text/plain", Size: 2}, []byte{0xff, 0xfe})
	if !invalid.Binary {
		t.Fatalf("invalid utf8=%+v", invalid)
	}
	// A multi-byte rune straddling the bound is never split.
	big := strings.Repeat("a", MaxSupportFileReadBytes-1) + "é" + strings.Repeat("b", 10)
	cut := supportFileContent(skillpkg.SupportFile{Path: "big.json", Mime: "application/json", Size: int64(len(big))}, []byte(big))
	if !cut.Truncated || len(cut.Text) != MaxSupportFileReadBytes-1 || !strings.HasSuffix(cut.Text, "a") {
		t.Fatalf("truncated len=%d truncated=%v", len(cut.Text), cut.Truncated)
	}
}

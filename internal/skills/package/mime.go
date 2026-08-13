package skillpkg

import (
	"strings"
)

// mime.go — the canonical support-MIME allowlist for complete skill
// packages. A package's support files may only carry the MIME types
// in this table; any other type (archives, executables, unknown
// extensions) is rejected as unsupported before a SupportFile entry
// exists. The table is closed and canonical: there is no operator
// override knob, because the package identity (the versioned
// `PackageHash`) must mean the same thing to every resolver.

// extensionMIME maps a lowercase file extension (without the dot) to
// its canonical support MIME type.
var extensionMIME = map[string]string{
	// Documents.
	"md":       "text/markdown",
	"markdown": "text/markdown",
	"txt":      "text/plain; charset=utf-8",
	"rst":      "text/x-rst",
	"pdf":      "application/pdf",
	// Structured data / config.
	"json":    "application/json",
	"jsonl":   "application/x-ndjson",
	"yaml":    "application/yaml",
	"yml":     "application/yaml",
	"toml":    "application/toml",
	"csv":     "text/csv",
	"tsv":     "text/tab-separated-values",
	"xml":     "application/xml",
	"proto":   "text/x-proto",
	"ipynb":   "application/x-ipynb+json",
	"sql":     "text/x-sql",
	"graphql": "text/x-graphql",
	// Source / scripts.
	"go":    "text/x-go",
	"py":    "text/x-python",
	"js":    "text/javascript",
	"mjs":   "text/javascript",
	"ts":    "text/x-typescript",
	"sh":    "text/x-shellscript",
	"bash":  "text/x-shellscript",
	"zsh":   "text/x-shellscript",
	"ps1":   "text/x-powershell",
	"rb":    "text/x-ruby",
	"rs":    "text/x-rust",
	"c":     "text/x-c",
	"h":     "text/x-c",
	"cpp":   "text/x-c++",
	"hpp":   "text/x-c++",
	"java":  "text/x-java",
	"kt":    "text/x-kotlin",
	"swift": "text/x-swift",
	"php":   "text/x-php",
	// Web.
	"html": "text/html",
	"htm":  "text/html",
	"css":  "text/css",
	"svg":  "image/svg+xml",
	// Images.
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"ico":  "image/x-icon",
	"bmp":  "image/bmp",
	"tiff": "image/tiff",
	"avif": "image/avif",
	// Fonts.
	"ttf":   "font/ttf",
	"otf":   "font/otf",
	"woff":  "font/woff",
	"woff2": "font/woff2",
}

// mimeSupport reports whether mime is in the canonical allowlist.
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

// classifyMIME returns the canonical support MIME for a canonical
// package path, or "" when the file is outside the allowlist. The
// classification is extension-based (deterministic, closed): the
// last path segment's extension is looked up in the canonical table.
// Extension-less files (including the root SKILL.md, which is handled
// by the root-entry checks rather than the support manifest) are
// unsupported as support files.
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

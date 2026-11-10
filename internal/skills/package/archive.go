package skillpkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// archive.go — the archive validation primitives for complete skill
// packages. ValidateArchive scans a zip archive into the ordered
// canonical entry list that feeds the support manifest, rejecting
// every hostile shape:
//
//   - traversal / absolute / backslash / empty-segment paths and
//     `..` escapes (the traversal class);
//   - exact and case-folded path collisions (the case class — the
//     canonical path charset is ASCII-only, which closes the Unicode
//     normalization collision class by construction);
//   - paths beyond the skillpkg-URI path budget (MaxPackageURIPathRunes
//     — a path the exact bounded URI constructor could not carry);
//   - symlink / device / FIFO / socket / irregular entries and
//     directory entries carrying bytes (the links/devices class). An
//     empty directory entry is skipped, but only after its name passes
//     the traversal and path checks above; macOS `__MACOSX/` and
//     `.DS_Store` metadata is skipped unread;
//   - (after validation) one top-level folder wrapping the whole
//     package with its SKILL.md is stripped (stripWrapperFolder);
//   - nested archives (zip / gzip / tar / bzip2 / xz / zstd / 7z /
//     rar magic inside an entry's decompressed bytes);
//   - unsupported MIME (extensions outside the canonical allowlist)
//     and MIME content mismatches (an extension proposes a supported
//     MIME whose bounded content check the entry's bytes do not
//     satisfy);
//   - decompression and count / size / ratio violations (entry count,
//     per-entry and total decompressed bytes, compression
//     amplification — the zip-bomb class);
//   - structural corruption (bad zip structure, CRC / size mismatches).
//
// The entry list is returned in archive order; the canonical
// serializer re-orders it by path for identity purposes.

// ArchiveSentinel errors. Compare via errors.Is.
var (
	// ErrArchiveNotZip — the bytes are not a zip archive (bad magic).
	ErrArchiveNotZip = errors.New("skillpkg: archive is not a zip")
	// ErrArchiveCorrupt — the zip is structurally corrupt, or an
	// entry's bytes failed CRC / size verification.
	ErrArchiveCorrupt = errors.New("skillpkg: archive is corrupt")
	// ErrArchivePathInvalid — an entry path is structurally invalid
	// (absolute, backslash, empty segment, out-of-charset, non-ASCII,
	// oversized, `.` segment) or beyond the skillpkg-URI path budget
	// (MaxPackageURIPathRunes — a path the exact bounded URI
	// constructor cannot represent).
	ErrArchivePathInvalid = errors.New("skillpkg: archive path is not canonical")
	// ErrArchiveTraversal — an entry path escapes the package root
	// (`..` segment).
	ErrArchiveTraversal = errors.New("skillpkg: archive path escapes the package root")
	// ErrArchivePathCollision — two entries share an exact or
	// case-folded canonical path.
	ErrArchivePathCollision = errors.New("skillpkg: archive path collision")
	// ErrArchiveNonRegular — an entry is a symlink, device, FIFO,
	// socket, other non-regular file, or a directory entry carrying
	// bytes. An empty directory entry is skipped after its name passes
	// the path checks (it creates nothing).
	ErrArchiveNonRegular = errors.New("skillpkg: archive contains a non-regular entry (link/directory/device)")
	// ErrArchiveNested — an entry's content is itself an archive.
	ErrArchiveNested = errors.New("skillpkg: archive contains a nested archive")
	// ErrArchiveMimeUnsupported — an entry's MIME is outside the
	// canonical support allowlist.
	ErrArchiveMimeUnsupported = errors.New("skillpkg: archive entry MIME unsupported")
	// ErrArchiveMimeContentMismatch — an entry's extension proposes a
	// supported MIME, but the entry's bytes do not satisfy that
	// MIME's content check (extension / content mismatch, or
	// unsupported / ambiguous data). Wraps ErrMimeContentMismatch.
	ErrArchiveMimeContentMismatch = errors.New("skillpkg: archive entry content does not match its MIME")
	// ErrArchiveTooManyEntries — the archive exceeds the entry-count
	// bound.
	ErrArchiveTooManyEntries = errors.New("skillpkg: archive has too many entries")
	// ErrArchiveEntryTooLarge — one entry exceeds the per-entry
	// decompressed byte bound.
	ErrArchiveEntryTooLarge = errors.New("skillpkg: archive entry exceeds the size bound")
	// ErrArchiveTotalTooLarge — the entries' decompressed bytes exceed
	// the total bound.
	ErrArchiveTotalTooLarge = errors.New("skillpkg: archive total exceeds the size bound")
	// ErrArchiveRatioTooHigh — an entry's decompression amplification
	// exceeds the ratio bound (zip-bomb shape).
	ErrArchiveRatioTooHigh = errors.New("skillpkg: archive entry compression ratio too high")
)

// ArchiveLimits bounds the cost of scanning one package archive.
// Zero-valued fields fall back to the canonical defaults; there is no
// knob for relaxing the canonical path / MIME / link rules.
type ArchiveLimits struct {
	// MaxEntries bounds the entry count (default
	// DefaultMaxArchiveEntries).
	MaxEntries int
	// MaxEntryBytes bounds one entry's decompressed size (default
	// DefaultMaxArchiveEntryBytes).
	MaxEntryBytes int64
	// MaxTotalBytes bounds the sum of decompressed sizes (default
	// DefaultMaxArchiveTotalBytes).
	MaxTotalBytes int64
	// MaxCompressionRatio bounds decompression amplification:
	// uncompressed / compressed must not exceed it (default
	// DefaultMaxCompressionRatio).
	MaxCompressionRatio float64
}

// Normalize fills zero-valued limit fields with the canonical
// defaults.
func (l ArchiveLimits) Normalize() ArchiveLimits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxArchiveEntries
	}
	if l.MaxEntryBytes <= 0 {
		l.MaxEntryBytes = DefaultMaxArchiveEntryBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultMaxArchiveTotalBytes
	}
	if l.MaxCompressionRatio <= 0 {
		l.MaxCompressionRatio = DefaultMaxCompressionRatio
	}
	return l
}

// ArchiveEntry is one validated regular-file entry of a package
// archive. `Data` carries the materialized (bounded) bytes; `Path`,
// `Mime`, `Size`, and `Digest` are exactly the fields the ordered
// normalized support manifest records.
type ArchiveEntry struct {
	Path   string
	Mime   string
	Size   int64
	Digest string
	Data   []byte
}

// zipMagicVariants are the local-file-header / end-of-central-directory
// / data-descriptor signatures of a zip archive.
func isZipMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == 'P' && b[1] == 'K' && (b[2] == 3 || b[2] == 5 || b[2] == 7)
}

// looksLikeArchive sniffs decompressed entry bytes for a nested
// archive signature. The covered container formats are the common
// ones an attacker would use to smuggle an archive inside a package.
func looksLikeArchive(data []byte) bool {
	if len(data) >= 4 && isZipMagic(data) {
		return true
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return true // gzip
	}
	if len(data) >= 262 && bytes.Equal(data[257:262], []byte("ustar")) {
		return true // tar
	}
	if len(data) >= 3 && bytes.Equal(data[:3], []byte("BZh")) {
		return true // bzip2
	}
	if len(data) >= 6 && bytes.Equal(data[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}) {
		return true // xz
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return true // zstd
	}
	if len(data) >= 6 && bytes.Equal(data[:6], []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}) {
		return true // 7z
	}
	if len(data) >= 7 && bytes.Equal(data[:7], []byte("Rar!\x1a\x07")) {
		return true // rar
	}
	return false
}

// ValidateArchive scans `b` as a zip archive and returns the ordered
// list of validated regular-file entries, or a wrapped sentinel on
// the first rejection. The scan is fully bounded: decompression per
// entry is capped at MaxEntryBytes+1, the entry count and the claimed
// + actual total bytes are capped, and amplification is checked
// against the header sizes BEFORE any bytes are decompressed.
func ValidateArchive(b []byte, limits ArchiveLimits) ([]ArchiveEntry, error) {
	limits = limits.Normalize()
	if len(b) < 4 || !isZipMagic(b) {
		return nil, ErrArchiveNotZip
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("%w: open: %w", ErrArchiveCorrupt, err)
	}
	if len(zr.File) > limits.MaxEntries {
		return nil, fmt.Errorf("%w: %d entries exceeds %d", ErrArchiveTooManyEntries, len(zr.File), limits.MaxEntries)
	}

	exact := make(map[string]struct{}, len(zr.File))
	folded := make(map[string]struct{}, len(zr.File))
	var out []ArchiveEntry
	var claimedTotal, actualTotal int64

	for _, f := range zr.File {
		// Operating-system metadata a zip tool adds (the macOS
		// `__MACOSX/` resource-fork tree and `.DS_Store` files) is
		// ignored, never read: it is not package content.
		if isArchiveJunk(f.Name) {
			continue
		}
		// A directory entry (`zip -r` and Finder write them) creates
		// nothing and is skipped — but only after its name passes the
		// same canonicalization and traversal checks as a file, and
		// only when it carries no bytes. Every other non-regular entry
		// (symlink, device, FIFO, socket) is REJECTED.
		mode := f.Mode()
		if mode.IsDir() {
			if _, err := canonicalArchivePath(strings.TrimSuffix(f.Name, "/")); err != nil {
				return nil, err
			}
			if f.UncompressedSize64 != 0 {
				return nil, fmt.Errorf("%w: directory %q carries bytes", ErrArchiveNonRegular, f.Name)
			}
			continue
		}
		if !mode.IsRegular() {
			return nil, fmt.Errorf("%w: %q mode=%v", ErrArchiveNonRegular, f.Name, mode)
		}

		path, err := canonicalArchivePath(f.Name)
		if err != nil {
			return nil, err
		}
		// URI representability: every path the archive accepts must be
		// representable by the exact bounded skillpkg URI constructor.
		// canonicalizePath only enforces the raw path bound
		// (MaxPackagePathRunes); the URI carries a narrower path budget
		// (MaxPackageURIPathRunes), so a path beyond it would pass the
		// archive boundary and only fail later at PackageHash — reject
		// it here with the archive path error instead.
		if rl := len([]rune(path)); rl > MaxPackageURIPathRunes {
			return nil, fmt.Errorf("%w: %q is %d runes; the %s:// support URI carries at most %d path runes", ErrArchivePathInvalid, path, rl, URIScheme, MaxPackageURIPathRunes)
		}
		if _, dup := exact[path]; dup {
			return nil, fmt.Errorf("%w: duplicate path %q", ErrArchivePathCollision, path)
		}
		key := foldPath(path)
		if _, dup := folded[key]; dup {
			return nil, fmt.Errorf("%w: case-colliding paths under %q", ErrArchivePathCollision, key)
		}
		exact[path] = struct{}{}
		folded[key] = struct{}{}

		// A zip64 header may declare sizes above int64's range; a
		// wrapping int64 conversion would silently turn a hostile size
		// negative and slip past the per-entry bound below, so the
		// conversion is guarded before it can wrap. (The later
		// exact-size check would still reject the entry, but as a
		// confusing mismatch.)
		if f.UncompressedSize64 > math.MaxInt64 || f.CompressedSize64 > math.MaxInt64 {
			return nil, fmt.Errorf("%w: %q declares a size beyond the archive bound", ErrArchiveCorrupt, path)
		}
		size := int64(f.UncompressedSize64)
		compressed := int64(f.CompressedSize64)
		if size > limits.MaxEntryBytes {
			return nil, fmt.Errorf("%w: %q is %d bytes (bound %d)", ErrArchiveEntryTooLarge, path, size, limits.MaxEntryBytes)
		}
		claimedTotal += size
		if claimedTotal > limits.MaxTotalBytes {
			return nil, fmt.Errorf("%w: claimed total %d exceeds %d", ErrArchiveTotalTooLarge, claimedTotal, limits.MaxTotalBytes)
		}
		if compressed > 0 && size > 0 {
			ratio := float64(size) / float64(compressed)
			if ratio > limits.MaxCompressionRatio {
				return nil, fmt.Errorf("%w: %q amplifies %.1fx (bound %.1fx)", ErrArchiveRatioTooHigh, path, ratio, limits.MaxCompressionRatio)
			}
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: open %q: %w", ErrArchiveCorrupt, path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, limits.MaxEntryBytes+1))
		closeErr := rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: read %q: %w", ErrArchiveCorrupt, path, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%w: read %q: %w", ErrArchiveCorrupt, path, closeErr)
		}
		if int64(len(data)) != size {
			return nil, fmt.Errorf("%w: %q decompressed to %d bytes, header claims %d", ErrArchiveCorrupt, path, len(data), size)
		}
		actualTotal += int64(len(data))
		if actualTotal > limits.MaxTotalBytes {
			return nil, fmt.Errorf("%w: actual total %d exceeds %d", ErrArchiveTotalTooLarge, actualTotal, limits.MaxTotalBytes)
		}

		if looksLikeArchive(data) {
			return nil, fmt.Errorf("%w: %q", ErrArchiveNested, path)
		}

		mime, ok := supportMIME(path)
		if !ok {
			return nil, fmt.Errorf("%w: %q has no supported MIME", ErrArchiveMimeUnsupported, path)
		}
		// MIME is content truth, not an extension lookup: the bytes
		// must satisfy the proposed MIME's bounded content check.
		if err := ValidateMimeContent(mime, data); err != nil {
			return nil, fmt.Errorf("%w: %q: %w", ErrArchiveMimeContentMismatch, path, err)
		}

		sum := sha256.Sum256(data)
		out = append(out, ArchiveEntry{
			Path:   path,
			Mime:   mime,
			Size:   size,
			Digest: hex.EncodeToString(sum[:]),
			Data:   data,
		})
	}
	return stripWrapperFolder(out), nil
}

// canonicalArchivePath canonicalizes one raw entry name, mapping a
// traversal to ErrArchiveTraversal and any other violation to
// ErrArchivePathInvalid.
func canonicalArchivePath(name string) (string, error) {
	path, err := canonicalizePath(name)
	if err != nil {
		var pe *pathErr
		if errors.As(err, &pe) && pe.class == violationTraversal {
			return "", fmt.Errorf("%w: %s", ErrArchiveTraversal, pe.msg)
		}
		return "", fmt.Errorf("%w: %s", ErrArchivePathInvalid, err.Error())
	}
	return path, nil
}

// stripWrapperFolder removes one top-level folder that wraps the whole
// package (the layout Finder's "Compress" and most skill downloads
// produce: `my-skill/SKILL.md`). It applies only when there is no root
// SKILL.md and every entry sits under the same `<dir>/` whose
// `<dir>/SKILL.md` exists; otherwise the entries are returned unchanged
// and the root-SKILL.md rules refuse them as before. Stripping a shared
// prefix keeps paths canonical, unique and case-unique, so the package
// hashes exactly like the same content zipped at the root.
func stripWrapperFolder(entries []ArchiveEntry) []ArchiveEntry {
	if len(entries) == 0 {
		return entries
	}
	dir, _, ok := strings.Cut(entries[0].Path, "/")
	if !ok {
		return entries
	}
	prefix := dir + "/"
	hasSkill := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, prefix) {
			return entries
		}
		if e.Path == prefix+RootSkillFileName {
			hasSkill = true
		}
	}
	if !hasSkill {
		return entries
	}
	out := make([]ArchiveEntry, len(entries))
	for i, e := range entries {
		e.Path = strings.TrimPrefix(e.Path, prefix)
		out[i] = e
	}
	return out
}

// isArchiveJunk reports whether a raw zip entry name is operating-system
// metadata (macOS `__MACOSX/` trees and `.DS_Store` files) that is
// skipped rather than validated as package content.
func isArchiveJunk(name string) bool {
	if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
		return true
	}
	base := name
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		base = name[i+1:]
	}
	return base == ".DS_Store"
}

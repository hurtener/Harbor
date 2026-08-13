package skillpkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
//   - symlink / device / FIFO / socket / irregular entries (the
//     links/devices class; directory entries are skipped as
//     content-free);
//   - nested archives (zip / gzip / tar / bzip2 / xz / zstd / 7z /
//     rar magic inside an entry's decompressed bytes);
//   - unsupported MIME (extensions outside the canonical allowlist);
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
	// oversized, `.` segment).
	ErrArchivePathInvalid = errors.New("skillpkg: archive path is not canonical")
	// ErrArchiveTraversal — an entry path escapes the package root
	// (`..` segment).
	ErrArchiveTraversal = errors.New("skillpkg: archive path escapes the package root")
	// ErrArchivePathCollision — two entries share an exact or
	// case-folded canonical path.
	ErrArchivePathCollision = errors.New("skillpkg: archive path collision")
	// ErrArchiveNonRegular — an entry is a symlink, device, FIFO,
	// socket, or other non-regular file.
	ErrArchiveNonRegular = errors.New("skillpkg: archive contains a non-regular entry (link/device)")
	// ErrArchiveNested — an entry's content is itself an archive.
	ErrArchiveNested = errors.New("skillpkg: archive contains a nested archive")
	// ErrArchiveMimeUnsupported — an entry's MIME is outside the
	// canonical support allowlist.
	ErrArchiveMimeUnsupported = errors.New("skillpkg: archive entry MIME unsupported")
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
		return nil, fmt.Errorf("%w: open: %v", ErrArchiveCorrupt, err)
	}
	if len(zr.File) > limits.MaxEntries {
		return nil, fmt.Errorf("%w: %d entries exceeds %d", ErrArchiveTooManyEntries, len(zr.File), limits.MaxEntries)
	}

	exact := make(map[string]struct{}, len(zr.File))
	folded := make(map[string]struct{}, len(zr.File))
	var out []ArchiveEntry
	var claimedTotal, actualTotal int64

	for _, f := range zr.File {
		// Directory entries carry no content and are skipped, never
		// materialized. The zip convention marks them either by a
		// trailing slash on the name or by the mode's directory bit;
		// both are checked BEFORE path canonicalization because a
		// trailing slash is not a canonical path.
		if strings.HasSuffix(f.Name, "/") || f.Mode().IsDir() {
			continue
		}

		path, err := canonicalizePath(f.Name)
		if err != nil {
			var pe *pathErr
			if errors.As(err, &pe) && pe.class == violationTraversal {
				return nil, fmt.Errorf("%w: %s", ErrArchiveTraversal, pe.msg)
			}
			return nil, fmt.Errorf("%w: %s", ErrArchivePathInvalid, err.Error())
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

		mode := f.Mode()
		if !mode.IsRegular() {
			return nil, fmt.Errorf("%w: %q mode=%v", ErrArchiveNonRegular, path, mode)
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
			return nil, fmt.Errorf("%w: open %q: %v", ErrArchiveCorrupt, path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(rc, limits.MaxEntryBytes+1))
		closeErr := rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrArchiveCorrupt, path, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrArchiveCorrupt, path, closeErr)
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

		sum := sha256.Sum256(data)
		out = append(out, ArchiveEntry{
			Path:   path,
			Mime:   mime,
			Size:   size,
			Digest: hex.EncodeToString(sum[:]),
			Data:   data,
		})
	}
	return out, nil
}

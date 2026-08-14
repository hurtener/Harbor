// fscheck.go — the strict filesystem primitives of the eager boot
// loader.
//
// The loader never trusts a path. Every directory and file is checked
// with Lstat (never Stat, so a symlink at the final component is seen
// as a symlink, not followed), regular-file checks reject
// symlink/hardlink/special entries, and the open path re-verifies the
// file's identity with fstat + os.SameFile so a path swap between
// stat and open fails loudly instead of reading a different file.

package bootpacks

import (
	"fmt"
	"io"
	"os"
)

// checkDirInfo rejects a directory entry that is not a real
// directory: a symlink (ModeSymlink — Lstat sees the link itself, not
// the target) or any non-directory mode.
func checkDirInfo(info os.FileInfo, path string) error {
	m := info.Mode()
	switch {
	case m&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s", ErrSymlink, path)
	case !m.IsDir():
		return fmt.Errorf("%w: %s mode=%v", ErrNotDirectory, path, m)
	default:
		return nil
	}
}

// checkRegularFile rejects a file entry that is not a single-link
// regular file: symlinks, directories, devices, FIFOs, sockets, and
// other specials are all rejected, and the link count must be exactly
// 1. When the platform cannot report a link count the check fails
// closed — the loader never accepts a file whose hardlink status it
// cannot prove.
//
//nolint:misspell // A "hardlinked" file has nlink > 1 even when only one name lives in the include directory.
func checkRegularFile(info os.FileInfo, path string) error {
	m := info.Mode()
	switch {
	case m&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s", ErrSymlink, path)
	case !m.IsRegular():
		return fmt.Errorf("%w: %s mode=%v", ErrSpecialFile, path, m)
	}
	n, ok := linkCount(info)
	if !ok {
		return fmt.Errorf("%w: %s: link count unavailable (cannot prove nlink == 1)", ErrHardlink, path)
	}
	if n != 1 {
		return fmt.Errorf("%w: %s nlink=%d", ErrHardlink, path, n)
	}
	return nil
}

// verifySameFile enforces the path-swap gate: the fstat of the opened
// file must be the same file the Lstat described. A different file
// (the path was swapped between the two calls) fails loudly.
func verifySameFile(before, after os.FileInfo, path string) error {
	if !os.SameFile(before, after) {
		return fmt.Errorf("%w: %s: identity changed between stat and open", ErrPathSwap, path)
	}
	return nil
}

// readFileStrict reads a single-link regular file at path, bounded at
// maxBytes, returning every byte in a fresh slice.
//
// The read is strict end to end:
//
//  1. Lstat + regular/nlink checks (fail fast on the metadata);
//  2. the declared size must fit the byte bound BEFORE opening;
//  3. open, then fstat the opened file and verify it is the same file
//     the Lstat described (path swap rejection), re-checking the
//     regular/nlink invariants on the opened identity;
//  4. read every byte through a limit reader — a file that grew past
//     the bound between stat and read fails loudly.
func readFileStrict(path string, maxBytes int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("bootpacks: stat %s: %w", path, err)
	}
	if err := checkRegularFile(before, path); err != nil {
		return nil, err
	}
	if before.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes (bound %d)", ErrBoundExceeded, path, before.Size(), maxBytes)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bootpacks: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	after, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("bootpacks: fstat %s: %w", path, err)
	}
	if err := verifySameFile(before, after, path); err != nil {
		return nil, err
	}
	if err := checkRegularFile(after, path); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bootpacks: read %s: %w", path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s grew to %d bytes during read (bound %d)", ErrBoundExceeded, path, len(data), maxBytes)
	}
	return data, nil
}

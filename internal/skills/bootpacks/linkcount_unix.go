//go:build unix

package bootpacks

import (
	"os"
	"syscall"
)

// hasSingleLink reports whether info has exactly one hard link and whether the
// platform reported one. On unix the Lstat/fstat identity carries the
// nlink field; any file with nlink > 1 is rejected by the loader.
//
//nolint:misspell // "hardlinked" is the correct term for a file with nlink > 1.
func hasSingleLink(info os.FileInfo) (bool, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return st.Nlink == 1, true
}

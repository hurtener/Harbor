//go:build unix

package bootpacks

import (
	"os"
	"syscall"
)

// linkCount returns the hard-link count of info and whether the
// platform reported one. On unix the Lstat/fstat identity carries the
// nlink field; any file with nlink > 1 is a hardlinked file and is
// rejected by the loader.
func linkCount(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

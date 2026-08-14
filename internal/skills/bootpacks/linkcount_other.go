//go:build !unix

package bootpacks

import "os"

// linkCount fails closed on platforms that cannot report a hard-link
// count: the loader rejects the file rather than accepting one whose
// link status it cannot prove.
func linkCount(os.FileInfo) (uint64, bool) {
	return 0, false
}

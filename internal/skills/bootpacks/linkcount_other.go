//go:build !unix

package bootpacks

import "os"

// hasSingleLink fails closed on platforms that cannot report a hard-link
// count: the loader rejects the file rather than accepting one whose
// link status it cannot prove.
func hasSingleLink(os.FileInfo) (bool, bool) {
	return false, false
}

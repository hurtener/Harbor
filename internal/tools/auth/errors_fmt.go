package auth

import "fmt"

// joinFmt formats a wrapped sentinel with %w plus a contextual detail
// message. Kept in its own file so `wrap` in `auth.go` does not pull
// fmt into the leaf interface file (one-file-one-job convention).
func joinFmt(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

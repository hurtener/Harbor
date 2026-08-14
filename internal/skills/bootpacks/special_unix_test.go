//go:build unix

package bootpacks

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
)

// TestNew_RejectsFIFO pins the special-file gate on unix: a FIFO (or
// any non-regular entry) at the SKILL.md position fails the load loud
// with ErrSpecialFile, never being read.
func TestNew_RejectsFIFO(t *testing.T) {
	root := t.TempDir()
	dir := writePackDir(t, root, "fifo", nil)
	if err := syscall.Mkfifo(filepath.Join(dir, "SKILL.md"), 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	_, err := New(context.Background(), []config.BootAgentPackConfig{
		declaration(t, "acme", "agent", root, "fifo"),
	}, testDeps(t, nil))
	if !errors.Is(err, ErrSpecialFile) {
		t.Fatalf("New: err=%v, want ErrSpecialFile", err)
	}
}

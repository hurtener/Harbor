//go:build darwin

package integration

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY(width, height int) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	//nolint:staticcheck // x/sys exposes these Darwin PTY requests only through the deprecated raw ioctl constant.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCPTYGRANT, 0); errno != 0 {
		_ = master.Close()
		return nil, nil, errno
	}
	//nolint:staticcheck // x/sys exposes these Darwin PTY requests only through the deprecated raw ioctl constant.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCPTYUNLK, 0); errno != 0 {
		_ = master.Close()
		return nil, nil, errno
	}
	name := make([]byte, 128)
	//nolint:staticcheck // x/sys exposes these Darwin PTY requests only through the deprecated raw ioctl constant.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		_ = master.Close()
		return nil, nil, errno
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open PTY slave: %w", err)
	}
	if err = resizePTY(master, width, height); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
func resizePTY(master *os.File, width, height int) error {
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(width), Row: uint16(height)})
}
func ptySize(master *os.File) (int, int, error) {
	size, err := unix.IoctlGetWinsize(int(master.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(size.Col), int(size.Row), nil
}

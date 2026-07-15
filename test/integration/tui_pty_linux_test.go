//go:build linux

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
	unlock := 0
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		_ = master.Close()
		return nil, nil, errno
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
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

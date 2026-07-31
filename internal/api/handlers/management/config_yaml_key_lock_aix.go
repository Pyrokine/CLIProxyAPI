//go:build aix

package management

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockConfigYAMLRedactionKeyFile(file *os.File) error {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	return unix.FcntlFlock(file.Fd(), unix.F_SETLKW, &lock)
}

func unlockConfigYAMLRedactionKeyFile(file *os.File) error {
	lock := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
	return unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
}

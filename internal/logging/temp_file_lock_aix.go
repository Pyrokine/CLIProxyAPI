//go:build aix

package logging

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockRequestLogFile(file *os.File) error {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	return unix.FcntlFlock(file.Fd(), unix.F_SETLKW, &lock)
}

func tryLockRequestLogFile(file *os.File) (bool, error) {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	errLock := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
	if errors.Is(errLock, unix.EACCES) || errors.Is(errLock, unix.EAGAIN) {
		return false, nil
	}
	return errLock == nil, errLock
}

func unlockRequestLogFile(file *os.File) error {
	lock := unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
	return unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
}

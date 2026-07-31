//go:build unix && !aix

package logging

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockRequestLogFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func tryLockRequestLogFile(file *os.File) (bool, error) {
	errLock := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(errLock, unix.EWOULDBLOCK) || errors.Is(errLock, unix.EAGAIN) {
		return false, nil
	}
	return errLock == nil, errLock
}

func unlockRequestLogFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

//go:build windows

package logging

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockRequestLogFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
}

func tryLockRequestLogFile(file *os.File) (bool, error) {
	errLock := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	if errors.Is(errLock, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return errLock == nil, errLock
}

func unlockRequestLogFile(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&windows.Overlapped{},
	)
}

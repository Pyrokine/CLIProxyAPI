//go:build unix && !aix

package xai

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockTokenFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockTokenFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

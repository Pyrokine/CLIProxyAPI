//go:build unix && !aix

package management

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockConfigYAMLRedactionKeyFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockConfigYAMLRedactionKeyFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

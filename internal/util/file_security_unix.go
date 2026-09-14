//go:build unix

package util

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// validateFileOwner requires file to be owned by the effective process user.
func validateFileOwner(file *os.File) error {
	if file == nil {
		return fmt.Errorf("file is nil")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect file owner: %w", err)
	}
	if uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("file is not owned by the effective process user")
	}
	return nil
}

// SecurePrivateFile restricts a file or directory to the effective process user.
func SecurePrivateFile(file *os.File, directory bool) error {
	if err := validateFileOwner(file); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	return file.Chmod(mode)
}

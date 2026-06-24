//go:build unix

package management

import (
	"errors"
	"os"
	"syscall"
)

func openLogFileNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func isLogSymlinkError(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

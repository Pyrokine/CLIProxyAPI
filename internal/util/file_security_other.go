//go:build !unix && !windows

package util

import (
	"fmt"
	"os"
)

func ValidateFileOwner(*os.File) error {
	return fmt.Errorf("file ownership validation is unsupported on this platform")
}

func SecurePrivateFile(*os.File, bool) error {
	return fmt.Errorf("private file permissions are unsupported on this platform")
}

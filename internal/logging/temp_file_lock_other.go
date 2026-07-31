//go:build !unix && !windows

package logging

import (
	"fmt"
	"os"
)

func lockRequestLogFile(*os.File) error {
	return fmt.Errorf("request log file locking is unsupported on this platform")
}

func tryLockRequestLogFile(*os.File) (bool, error) {
	return false, fmt.Errorf("request log file locking is unsupported on this platform")
}

func unlockRequestLogFile(*os.File) error {
	return nil
}

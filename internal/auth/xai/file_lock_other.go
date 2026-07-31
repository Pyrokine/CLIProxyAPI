//go:build !unix && !windows

package xai

import (
	"fmt"
	"os"
)

func lockTokenFile(*os.File) error {
	return fmt.Errorf("cross-process token file locking is unsupported on this platform")
}

func unlockTokenFile(*os.File) error {
	return nil
}

//go:build !unix && !windows

package management

import (
	"fmt"
	"os"
)

func lockConfigYAMLRedactionKeyFile(*os.File) error {
	return fmt.Errorf("cross-process redaction key locking is unsupported on this platform")
}

func unlockConfigYAMLRedactionKeyFile(*os.File) error {
	return nil
}

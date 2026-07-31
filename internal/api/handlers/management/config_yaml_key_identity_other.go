//go:build !windows

package management

import "path/filepath"

func configYAMLRedactionKeyIdentity(path string) (string, error) {
	return filepath.Clean(path), nil
}

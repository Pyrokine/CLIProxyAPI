//go:build !windows

package auth

import "os"

func replaceTokenFile(sourcePath, targetPath string) error {
	return os.Rename(sourcePath, targetPath)
}

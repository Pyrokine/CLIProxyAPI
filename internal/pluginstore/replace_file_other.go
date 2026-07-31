//go:build !windows

package pluginstore

import "os"

func replaceFile(sourcePath, targetPath string) error {
	return os.Rename(sourcePath, targetPath)
}

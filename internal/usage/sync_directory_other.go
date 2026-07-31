//go:build !windows

package usage

import "os"

func syncDirectory(path string) error {
	dir, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

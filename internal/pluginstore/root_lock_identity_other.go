//go:build !windows

package pluginstore

import "path/filepath"

func rootLockIdentity(root string) string {
	return filepath.Clean(root)
}

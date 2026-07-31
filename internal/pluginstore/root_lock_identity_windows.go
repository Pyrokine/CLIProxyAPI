//go:build windows

package pluginstore

import (
	"path/filepath"
	"strings"
)

func rootLockIdentity(root string) string {
	return strings.ToLower(filepath.Clean(root))
}

package auth

import (
	"path/filepath"
	"runtime"
	"strings"
)

// FileAuthID 返回管理接口和 watcher 共用的文件凭证标识
func FileAuthID(path, authDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	authDir = strings.TrimSpace(authDir)
	id := path
	if authDir != "" {
		resolvedPath := path
		if absolutePath, errAbsolute := filepath.Abs(path); errAbsolute == nil {
			resolvedPath = absolutePath
		}
		resolvedAuthDir := authDir
		if absoluteAuthDir, errAbsolute := filepath.Abs(authDir); errAbsolute == nil {
			resolvedAuthDir = absoluteAuthDir
		}
		if relative, errRelative := filepath.Rel(resolvedAuthDir, resolvedPath); errRelative == nil &&
			relative != "" && relative != "." && relative != ".." &&
			!filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			id = relative
		}
	}
	id = filepath.Clean(id)
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

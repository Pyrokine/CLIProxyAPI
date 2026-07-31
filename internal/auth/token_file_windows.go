//go:build windows

package auth

import "golang.org/x/sys/windows"

func replaceTokenFile(sourcePath, targetPath string) error {
	source, errSource := windows.UTF16PtrFromString(sourcePath)
	if errSource != nil {
		return errSource
	}
	target, errTarget := windows.UTF16PtrFromString(targetPath)
	if errTarget != nil {
		return errTarget
	}
	return windows.MoveFileEx(
		source,
		target,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

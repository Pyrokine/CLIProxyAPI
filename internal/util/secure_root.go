package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenOrCreateDirectoryRoot opens an absolute directory path without accepting
// symlinks in any path component. Missing components are created with perm.
func OpenOrCreateDirectoryRoot(path string, perm os.FileMode) (*os.Root, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve directory path: %w", err)
	}
	volume := filepath.VolumeName(absolutePath)
	rootPath := volume + string(os.PathSeparator)
	relativePath := strings.TrimLeft(
		strings.TrimPrefix(absolutePath, volume), string(os.PathSeparator),
	)
	if relativePath == "" {
		return nil, fmt.Errorf("directory path must not be a filesystem root")
	}
	components := strings.Split(relativePath, string(os.PathSeparator))

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	for _, component := range components {
		created := false
		pathInfo, errInfo := root.Lstat(component)
		if os.IsNotExist(errInfo) {
			if err = root.Mkdir(component, perm); err != nil && !os.IsExist(err) {
				_ = root.Close()
				return nil, fmt.Errorf("create directory component %q: %w", component, err)
			}
			created = err == nil
			pathInfo, errInfo = root.Lstat(component)
		}
		if errInfo != nil {
			_ = root.Close()
			return nil, fmt.Errorf("inspect directory component %q: %w", component, errInfo)
		}
		if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.IsDir() {
			_ = root.Close()
			return nil, fmt.Errorf("directory component %q is not a regular directory", component)
		}

		nextRoot, errOpen := root.OpenRoot(component)
		if errOpen != nil {
			_ = root.Close()
			return nil, fmt.Errorf("open directory component %q: %w", component, errOpen)
		}
		rootInfo, errStat := nextRoot.Stat(".")
		if errStat != nil {
			_ = nextRoot.Close()
			_ = root.Close()
			return nil, fmt.Errorf("inspect opened directory component %q: %w", component, errStat)
		}
		verifiedInfo, errVerify := root.Lstat(component)
		if errVerify != nil || !os.SameFile(rootInfo, pathInfo) || !os.SameFile(rootInfo, verifiedInfo) {
			_ = nextRoot.Close()
			_ = root.Close()
			if errVerify != nil {
				return nil, fmt.Errorf("verify directory component %q: %w", component, errVerify)
			}
			return nil, fmt.Errorf("directory component %q changed during validation", component)
		}
		if created {
			directory, errDirectory := nextRoot.Open(".")
			if errDirectory != nil {
				_ = nextRoot.Close()
				_ = root.Close()
				return nil, fmt.Errorf("open created directory component %q: %w", component, errDirectory)
			}
			if err = SecurePrivateFile(directory, true); err != nil {
				_ = directory.Close()
				_ = nextRoot.Close()
				_ = root.Close()
				return nil, fmt.Errorf("secure created directory component %q: %w", component, err)
			}
			if err = directory.Close(); err != nil {
				_ = nextRoot.Close()
				_ = root.Close()
				return nil, fmt.Errorf("close created directory component %q: %w", component, err)
			}
		}
		if err = root.Close(); err != nil {
			_ = nextRoot.Close()
			return nil, fmt.Errorf("close parent directory root: %w", err)
		}
		root = nextRoot
	}
	directory, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open final directory root: %w", err)
	}
	if err = SecurePrivateFile(directory, true); err != nil {
		_ = directory.Close()
		_ = root.Close()
		return nil, fmt.Errorf("secure final directory root: %w", err)
	}
	if err = directory.Close(); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("close final directory root: %w", err)
	}
	return root, nil
}

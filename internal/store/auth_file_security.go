package store

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	internalauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func secureAuthDirectory(path string) error {
	root, err := util.OpenOrCreateDirectoryRoot(path, 0o700)
	if err != nil {
		return err
	}
	if err = root.Close(); err != nil {
		return fmt.Errorf("close auth directory root: %w", err)
	}
	return nil
}

func secureAuthFilePath(path string) error {
	if err := secureAuthDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return internalauth.SecureTokenFile(path)
}

func writeAuthFile(path string, data []byte) error {
	return internalauth.WriteTokenFileAtomic(path, data)
}

func secureAuthTree(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("auth directory is empty")
	}
	if err := secureAuthDirectory(root); err != nil {
		return err
	}
	return filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
				return nil
			}
			return secureAuthFilePath(path)
		},
	)
}

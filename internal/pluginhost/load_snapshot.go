package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
)

func preparePluginFileSnapshots(
	files []pluginFile,
	items map[string]runtimeItemConfig,
) ([]pluginFile, string, error) {
	if len(files) == 0 {
		return files, "", nil
	}
	dir, errDir := os.MkdirTemp("", "cliproxy-plugin-snapshot-")
	if errDir != nil {
		return nil, "", fmt.Errorf("create plugin snapshot directory: %w", errDir)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	if errChmod := os.Chmod(dir, 0o700); errChmod != nil {
		return nil, "", fmt.Errorf("secure plugin snapshot directory: %w", errChmod)
	}

	prepared := append([]pluginFile(nil), files...)
	for index := range prepared {
		file := &prepared[index]
		item, configured := items[file.ID]
		if configured && !item.Enabled {
			continue
		}
		sourceInfo, errSource := os.Lstat(file.Path)
		if errors.Is(errSource, os.ErrNotExist) {
			continue
		}
		if errSource != nil {
			return nil, "", fmt.Errorf("inspect plugin %s: %w", file.ID, errSource)
		}
		if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
			return nil, "", fmt.Errorf("plugin artifact is not a regular file: %s", file.Path)
		}
		source, errOpen := os.Open(file.Path)
		if errOpen != nil {
			return nil, "", fmt.Errorf("open plugin %s: %w", file.ID, errOpen)
		}
		openedInfo, errOpenedInfo := source.Stat()
		if errOpenedInfo != nil || !os.SameFile(sourceInfo, openedInfo) {
			_ = source.Close()
			if errOpenedInfo != nil {
				return nil, "", fmt.Errorf("inspect opened plugin %s: %w", file.ID, errOpenedInfo)
			}
			return nil, "", fmt.Errorf("plugin artifact changed while opening: %s", file.Path)
		}
		snapshotPath := filepath.Join(
			dir,
			fmt.Sprintf("%04d-%s%s", index, file.ID, filepath.Ext(file.Path)),
		)
		snapshot, errCreate := os.OpenFile(snapshotPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errCreate != nil {
			_ = source.Close()
			return nil, "", fmt.Errorf("create plugin %s snapshot: %w", file.ID, errCreate)
		}
		hash := sha256.New()
		_, errCopy := io.Copy(io.MultiWriter(snapshot, hash), source)
		errSourceClose := source.Close()
		if errCopy != nil {
			_ = snapshot.Close()
			return nil, "", fmt.Errorf("copy plugin %s snapshot: %w", file.ID, errCopy)
		}
		if errSourceClose != nil {
			_ = snapshot.Close()
			return nil, "", fmt.Errorf("close plugin %s source: %w", file.ID, errSourceClose)
		}
		if errSync := snapshot.Sync(); errSync != nil {
			_ = snapshot.Close()
			return nil, "", fmt.Errorf("sync plugin %s snapshot: %w", file.ID, errSync)
		}
		if errClose := snapshot.Close(); errClose != nil {
			return nil, "", fmt.Errorf("close plugin %s snapshot: %w", file.ID, errClose)
		}
		verifiedInfo, errVerify := os.Lstat(file.Path)
		if errVerify != nil || !os.SameFile(sourceInfo, verifiedInfo) || verifiedInfo.Size() != openedInfo.Size() {
			if errVerify != nil {
				return nil, "", fmt.Errorf("verify plugin %s: %w", file.ID, errVerify)
			}
			return nil, "", fmt.Errorf("plugin artifact changed while snapshotting: %s", file.Path)
		}
		file.loadPath = snapshotPath
		file.digest = hex.EncodeToString(hash.Sum(nil))
		file.sourceInfo = sourceInfo
	}
	cleanup = false
	return prepared, dir, nil
}

func removePluginFileSnapshots(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

func preparePluginCleanupSnapshots(files []pluginFile) ([]pluginFile, error) {
	prepared := append([]pluginFile(nil), files...)
	for index := range prepared {
		file := &prepared[index]
		info, errInfo := os.Lstat(file.Path)
		if errInfo != nil {
			return nil, fmt.Errorf("inspect plugin cleanup candidate %s: %w", file.ID, errInfo)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("plugin cleanup candidate is not a regular file: %s", file.Path)
		}
		source, errOpen := os.Open(file.Path)
		if errOpen != nil {
			return nil, fmt.Errorf("open plugin cleanup candidate %s: %w", file.ID, errOpen)
		}
		openedInfo, errOpenedInfo := source.Stat()
		if errOpenedInfo != nil || !os.SameFile(info, openedInfo) {
			_ = source.Close()
			if errOpenedInfo != nil {
				return nil, fmt.Errorf("inspect opened plugin cleanup candidate %s: %w", file.ID, errOpenedInfo)
			}
			return nil, fmt.Errorf("plugin cleanup candidate changed while opening: %s", file.Path)
		}
		hash := sha256.New()
		_, errCopy := io.Copy(hash, source)
		errClose := source.Close()
		if errCopy != nil {
			return nil, fmt.Errorf("hash plugin cleanup candidate %s: %w", file.ID, errCopy)
		}
		if errClose != nil {
			return nil, fmt.Errorf("close plugin cleanup candidate %s: %w", file.ID, errClose)
		}
		verifiedInfo, errVerify := os.Lstat(file.Path)
		if errVerify != nil || !os.SameFile(info, verifiedInfo) {
			if errVerify != nil {
				return nil, fmt.Errorf("verify plugin cleanup candidate %s: %w", file.ID, errVerify)
			}
			return nil, fmt.Errorf("plugin cleanup candidate changed while hashing: %s", file.Path)
		}
		file.digest = hex.EncodeToString(hash.Sum(nil))
		file.sourceInfo = info
	}
	return prepared, nil
}

func pluginFileSnapshotCurrentUnderRoot(root string, file pluginFile) (bool, error) {
	rootLock, errLock := pluginstore.LockRoot(root)
	if errLock != nil {
		return false, errLock
	}
	defer rootLock.Unlock()
	return pluginFileSnapshotCurrent(file)
}

func pluginFileSnapshotCurrent(file pluginFile) (bool, error) {
	if file.digest == "" {
		return true, nil
	}
	info, errInfo := os.Lstat(file.Path)
	if errInfo != nil {
		return false, errInfo
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, nil
	}
	if file.sourceInfo != nil && !os.SameFile(file.sourceInfo, info) {
		return false, nil
	}
	source, errOpen := os.Open(file.Path)
	if errOpen != nil {
		return false, errOpen
	}
	defer source.Close()
	hash := sha256.New()
	if _, errCopy := io.Copy(hash, source); errCopy != nil {
		return false, errCopy
	}
	return hex.EncodeToString(hash.Sum(nil)) == file.digest, nil
}

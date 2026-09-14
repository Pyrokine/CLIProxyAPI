package auth

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

// WriteTokenFileAtomic publishes payload without exposing a partially written credential.
func WriteTokenFileAtomic(path string, payload []byte) error {
	return PublishTokenFile(
		path,
		func(candidatePath string) error {
			file, errCreate := os.OpenFile(candidatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errCreate != nil {
				return errCreate
			}
			written, errWrite := file.Write(payload)
			if errWrite != nil {
				_ = file.Close()
				return errWrite
			}
			if written != len(payload) {
				_ = file.Close()
				return io.ErrShortWrite
			}
			return file.Close()
		},
	)
}

func SecureTokenFile(path string) error {
	pathInfo, errInfo := os.Lstat(path)
	if errInfo != nil {
		return errInfo
	}
	if !pathInfo.Mode().IsRegular() {
		return fmt.Errorf("token target is not a regular file")
	}
	file, errOpen := os.OpenFile(path, os.O_RDWR, 0o600)
	if errOpen != nil {
		return errOpen
	}
	openInfo, errStat := file.Stat()
	if errStat != nil {
		_ = file.Close()
		return errStat
	}
	verifiedInfo, errVerify := os.Lstat(path)
	if errVerify != nil || !verifiedInfo.Mode().IsRegular() ||
		!os.SameFile(pathInfo, openInfo) || !os.SameFile(openInfo, verifiedInfo) {
		_ = file.Close()
		return fmt.Errorf("token target changed during security validation")
	}
	if errSecure := util.SecurePrivateFile(file, false); errSecure != nil {
		_ = file.Close()
		return errSecure
	}
	return file.Close()
}

// PublishTokenFile stages a provider-owned write and atomically replaces the live credential.
func PublishTokenFile(path string, writeCandidate func(string) error) error {
	if writeCandidate == nil {
		return fmt.Errorf("token file writer is nil")
	}
	dir := filepath.Dir(path)
	root, errRoot := util.OpenOrCreateDirectoryRoot(dir, 0o700)
	if errRoot != nil {
		return fmt.Errorf("secure token directory: %w", errRoot)
	}
	if errCloseRoot := root.Close(); errCloseRoot != nil {
		return fmt.Errorf("close token directory root: %w", errCloseRoot)
	}
	candidate, errCreate := os.CreateTemp(dir, ".token-candidate-*")
	if errCreate != nil {
		return fmt.Errorf("create token candidate: %w", errCreate)
	}
	candidatePath := candidate.Name()
	if errClose := candidate.Close(); errClose != nil {
		_ = os.Remove(candidatePath)
		return fmt.Errorf("close token candidate placeholder: %w", errClose)
	}
	if errRemove := os.Remove(candidatePath); errRemove != nil {
		return fmt.Errorf("remove token candidate placeholder: %w", errRemove)
	}
	defer func() { _ = os.Remove(candidatePath) }()

	if errWrite := writeCandidate(candidatePath); errWrite != nil {
		return fmt.Errorf("write token candidate: %w", errWrite)
	}
	candidateInfo, errInfo := os.Lstat(candidatePath)
	if errInfo != nil {
		return fmt.Errorf("inspect token candidate: %w", errInfo)
	}
	if !candidateInfo.Mode().IsRegular() {
		return fmt.Errorf("token candidate is not a regular file")
	}
	candidateFile, errOpen := os.OpenFile(candidatePath, os.O_RDWR, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open token candidate: %w", errOpen)
	}
	openInfo, errStat := candidateFile.Stat()
	if errStat != nil {
		_ = candidateFile.Close()
		return fmt.Errorf("stat token candidate: %w", errStat)
	}
	pathInfo, errPathInfo := os.Lstat(candidatePath)
	if errPathInfo != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(
		candidateInfo, openInfo,
	) || !os.SameFile(openInfo, pathInfo) {
		_ = candidateFile.Close()
		return fmt.Errorf("token candidate changed before publication")
	}
	if errSecure := util.SecurePrivateFile(candidateFile, false); errSecure != nil {
		_ = candidateFile.Close()
		return fmt.Errorf("secure token candidate: %w", errSecure)
	}
	if errSync := candidateFile.Sync(); errSync != nil {
		_ = candidateFile.Close()
		return fmt.Errorf("sync token candidate: %w", errSync)
	}
	if errClose := candidateFile.Close(); errClose != nil {
		return fmt.Errorf("close token candidate: %w", errClose)
	}
	if targetInfo, errTarget := os.Lstat(path); errTarget == nil {
		if !targetInfo.Mode().IsRegular() {
			return fmt.Errorf("token target is not a regular file")
		}
	} else if !os.IsNotExist(errTarget) {
		return fmt.Errorf("inspect token target: %w", errTarget)
	}
	if errReplace := replaceTokenFile(candidatePath, path); errReplace != nil {
		return fmt.Errorf("publish token file: %w", errReplace)
	}
	if errSecure := SecureTokenFile(path); errSecure != nil {
		return fmt.Errorf("secure published token file: %w", errSecure)
	}
	if runtime.GOOS != "windows" {
		directory, errOpenDir := os.Open(dir)
		if errOpenDir != nil {
			return fmt.Errorf("open token directory: %w", errOpenDir)
		}
		if errSyncDir := directory.Sync(); errSyncDir != nil {
			_ = directory.Close()
			return fmt.Errorf("sync token directory: %w", errSyncDir)
		}
		if errCloseDir := directory.Close(); errCloseDir != nil {
			return fmt.Errorf("close token directory: %w", errCloseDir)
		}
	}
	return nil
}

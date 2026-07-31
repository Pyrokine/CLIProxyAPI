package pluginstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ArtifactTransaction stages the previous artifact so a later config failure
// can restore the original file object and all metadata attached to it.
type ArtifactTransaction struct {
	targetPath string
	stagedPath string
	stageDir   string
}

func (t *ArtifactTransaction) Backup(targetPath string) error {
	if t == nil {
		return fmt.Errorf("plugin artifact transaction is nil")
	}
	if t.stagedPath != "" {
		return fmt.Errorf("plugin artifact backup already exists")
	}
	if _, err := os.Lstat(targetPath); err != nil {
		return fmt.Errorf("stat plugin artifact: %w", err)
	}
	stageDir, err := os.MkdirTemp(
		filepath.Dir(targetPath),
		"."+filepath.Base(targetPath)+".rollback-*",
	)
	if err != nil {
		return fmt.Errorf("create plugin artifact stage: %w", err)
	}
	stagedPath := filepath.Join(stageDir, filepath.Base(targetPath))
	if err = os.Link(targetPath, stagedPath); err != nil {
		_ = os.Remove(stageDir)
		return fmt.Errorf("preserve plugin artifact: %w", err)
	}
	if err = syncDirectory(stageDir); err == nil {
		err = syncDirectory(filepath.Dir(targetPath))
	}
	if err != nil {
		errRemove := os.Remove(stagedPath)
		errCleanup := os.Remove(stageDir)
		return fmt.Errorf(
			"sync preserved plugin artifact: %w",
			errors.Join(err, errRemove, errCleanup),
		)
	}
	t.targetPath = targetPath
	t.stagedPath = stagedPath
	t.stageDir = stageDir
	return nil
}

func (t *ArtifactTransaction) Rollback(result InstallResult) error {
	if t == nil || result.Skipped {
		return t.Commit()
	}
	if t.stagedPath != "" {
		targetInfo, errTarget := os.Stat(t.targetPath)
		stagedInfo, errStaged := os.Stat(t.stagedPath)
		sameFile := errTarget == nil && errStaged == nil && os.SameFile(targetInfo, stagedInfo)
		if sameFile {
			if err := os.Remove(t.stagedPath); err != nil {
				return fmt.Errorf("remove preserved plugin artifact link: %w", err)
			}
		} else if err := replaceFile(t.stagedPath, t.targetPath); err != nil {
			return fmt.Errorf("restore preserved plugin artifact: %w", err)
		}
		stageDir := t.stageDir
		t.clear()
		if err := os.Remove(stageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove plugin artifact stage: %w", err)
		}
		return syncDirectory(filepath.Dir(stageDir))
	}
	if result.Overwritten {
		return fmt.Errorf("plugin artifact was overwritten without a backup")
	}
	if result.Path == "" {
		return nil
	}
	if err := os.Remove(result.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove installed plugin artifact: %w", err)
	}
	return syncDirectory(filepath.Dir(result.Path))
}

func (t *ArtifactTransaction) Commit() error {
	if t == nil || t.stagedPath == "" {
		return nil
	}
	if err := os.Remove(t.stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove staged plugin artifact: %w", err)
	}
	stageDir := t.stageDir
	if err := os.Remove(stageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin artifact stage: %w", err)
	}
	t.clear()
	return syncDirectory(filepath.Dir(stageDir))
}

func (t *ArtifactTransaction) clear() {
	t.targetPath = ""
	t.stagedPath = ""
	t.stageDir = ""
}

// StagedRemoval hides an artifact from discovery until its config mutation
// commits, while retaining the original file for rollback.
type StagedRemoval struct {
	targetPath string
	stagedPath string
	stageDir   string
}

func StageRemoval(targetPath string) (*StagedRemoval, error) {
	if targetPath == "" {
		return &StagedRemoval{}, nil
	}
	if _, err := os.Stat(targetPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &StagedRemoval{targetPath: targetPath}, nil
		}
		return nil, fmt.Errorf("stat plugin artifact: %w", err)
	}
	stageDir, err := os.MkdirTemp(
		filepath.Dir(targetPath),
		"."+filepath.Base(targetPath)+".delete-*",
	)
	if err != nil {
		return nil, fmt.Errorf("create plugin removal stage: %w", err)
	}
	stagedPath := filepath.Join(stageDir, filepath.Base(targetPath))
	if err = os.Link(targetPath, stagedPath); err != nil {
		_ = os.Remove(stageDir)
		return nil, fmt.Errorf("preserve plugin artifact for removal: %w", err)
	}
	if err = syncDirectory(stageDir); err == nil {
		err = syncDirectory(filepath.Dir(targetPath))
	}
	if err != nil {
		_ = os.Remove(stagedPath)
		_ = os.Remove(stageDir)
		return nil, fmt.Errorf("sync preserved plugin removal: %w", err)
	}
	return &StagedRemoval{
		targetPath: targetPath,
		stagedPath: stagedPath,
		stageDir:   stageDir,
	}, nil
}

func (s *StagedRemoval) Removed() bool {
	return s != nil && s.stagedPath != ""
}

func (s *StagedRemoval) Rollback() error {
	if s == nil || s.stagedPath == "" {
		return nil
	}
	if err := os.Remove(s.stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove preserved plugin artifact link: %w", err)
	}
	stageDir := s.stageDir
	s.clear()
	if err := os.Remove(stageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin removal stage: %w", err)
	}
	return syncDirectory(filepath.Dir(stageDir))
}

func (s *StagedRemoval) Commit() error {
	if s == nil || s.stagedPath == "" {
		return nil
	}
	if err := os.Remove(s.targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin artifact: %w", err)
	}
	if err := syncDirectory(filepath.Dir(s.targetPath)); err != nil {
		return err
	}
	if err := os.Remove(s.stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove preserved plugin artifact: %w", err)
	}
	stageDir := s.stageDir
	s.clear()
	if err := os.Remove(stageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin removal stage: %w", err)
	}
	return syncDirectory(filepath.Dir(stageDir))
}

func (s *StagedRemoval) clear() {
	s.targetPath = ""
	s.stagedPath = ""
	s.stageDir = ""
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer func() { _ = handle.Close() }()
	if err = handle.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

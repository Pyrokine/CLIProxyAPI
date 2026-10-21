package management

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func validateUpdateGuardSession(
	session updateGuardSession,
	stateDBPath string,
	executablePath string,
	requireCandidate bool,
	now time.Time,
) error {
	if (session.SchemaVersion != 1 && session.SchemaVersion != updateGuardStateSchemaVersion) || !isUpdateAttemptID(session.AttemptID) {
		return fmt.Errorf("invalid update session identity")
	}
	if !validVersion.MatchString(session.TargetVersion) || !validBuildVersion.MatchString(session.PreviousVersion) {
		return fmt.Errorf("invalid update session version")
	}
	for _, value := range []string{session.ExecutablePath, session.BackupPath, session.StateDBPath} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("invalid update session path")
		}
	}
	if filepath.Clean(stateDBPath) != session.StateDBPath || filepath.Clean(executablePath) != session.ExecutablePath {
		return fmt.Errorf("update session path does not match guard process")
	}
	expectedBackup := updateBackupPath(session.ExecutablePath, session.AttemptID)
	if session.SchemaVersion == 1 {
		expectedBackup = session.ExecutablePath + ".bak"
	}
	if session.BackupPath != expectedBackup ||
		session.GuardUnit != updateGuardUnit(session.AttemptID) ||
		session.ServiceUnit != defaultUpdateServiceUnit {
		return fmt.Errorf("invalid update session service target")
	}
	if requireCandidate {
		if session.SchemaVersion != updateGuardStateSchemaVersion || session.OriginInstance == "" ||
			!session.DrainDeadline.After(now) {
			return fmt.Errorf("update session lacks a current inference drain identity")
		}
		if err := validateUpdateArtifact(session.TargetIdentity, session.TargetVersion, false); err != nil {
			return err
		}
		if err := validateUpdateArtifact(session.PreviousIdentity, session.PreviousVersion, true); err != nil {
			return err
		}
		if session.CandidatePath != updateCandidatePath(session.ExecutablePath, session.AttemptID) {
			return fmt.Errorf("invalid update candidate path")
		}
		if err := requireRegularFile(session.CandidatePath); err != nil {
			return fmt.Errorf("validate update candidate: %w", err)
		}
	}
	parsed, err := url.Parse(session.HealthBaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() != "127.0.0.1" ||
		parsed.Port() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil {
		return fmt.Errorf("invalid update health URL")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid update health port")
	}
	if parsed.Scheme == "https" {
		if session.HealthTLSCertPath == "" || !filepath.IsAbs(session.HealthTLSCertPath) {
			return fmt.Errorf("invalid update health TLS certificate path")
		}
	} else if session.HealthTLSCertPath != "" {
		return fmt.Errorf("unexpected update health TLS certificate path")
	}
	if session.DeadlineUnix != session.Deadline.Unix() || !session.Deadline.After(now) {
		return fmt.Errorf("update session deadline expired")
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	return nil
}

func installUpdateCandidate(session updateGuardSession) error {
	if session.SchemaVersion != updateGuardStateSchemaVersion {
		return fmt.Errorf("legacy update session cannot install a candidate")
	}
	if err := verifyUpdateArtifactFile(session.CandidatePath, session.TargetIdentity); err != nil {
		return fmt.Errorf("validate candidate identity: %w", err)
	}
	if err := verifyUpdateArtifactFile(session.ExecutablePath, session.PreviousIdentity); err != nil {
		return fmt.Errorf("validate previous identity: %w", err)
	}
	if err := requireRegularFile(session.ExecutablePath); err != nil {
		return fmt.Errorf("validate current executable: %w", err)
	}
	if err := requireRegularFile(session.CandidatePath); err != nil {
		return fmt.Errorf("validate candidate executable: %w", err)
	}
	if filepath.Dir(session.CandidatePath) != filepath.Dir(session.ExecutablePath) {
		return fmt.Errorf("candidate executable is not on the target filesystem")
	}
	if _, err := os.Lstat(session.BackupPath); err == nil {
		return fmt.Errorf("attempt backup already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect previous backup: %w", err)
	}
	if err := os.Rename(session.ExecutablePath, session.BackupPath); err != nil {
		return fmt.Errorf("create update backup: %w", err)
	}
	if err := os.Rename(session.CandidatePath, session.ExecutablePath); err != nil {
		restoreErr := os.Rename(session.BackupPath, session.ExecutablePath)
		return errors.Join(fmt.Errorf("install update candidate: %w", err), restoreErr)
	}
	if err := syncDirectory(filepath.Dir(session.ExecutablePath)); err != nil {
		restoreErr := os.Rename(session.BackupPath, session.ExecutablePath)
		return errors.Join(err, restoreErr)
	}
	return nil
}

func restoreUpdateBackup(session updateGuardSession) error {
	if session.SchemaVersion == updateGuardStateSchemaVersion {
		if err := validateUpdateArtifact(session.PreviousIdentity, session.PreviousVersion, true); err != nil {
			return err
		}
		if err := verifyUpdateArtifactFile(session.BackupPath, session.PreviousIdentity); err != nil {
			return fmt.Errorf("validate backup identity: %w", err)
		}
	}
	if err := requireRegularFile(session.BackupPath); err != nil {
		return fmt.Errorf("validate update backup: %w", err)
	}
	if info, err := os.Lstat(session.ExecutablePath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("current executable is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect failed update binary: %w", err)
	}
	if err := os.Rename(session.BackupPath, session.ExecutablePath); err != nil {
		return fmt.Errorf("restore update backup: %w", err)
	}
	return syncDirectory(filepath.Dir(session.ExecutablePath))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open update directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err = directory.Sync(); err != nil {
		return fmt.Errorf("sync update directory: %w", err)
	}
	return nil
}

func removeUpdateBackup(session updateGuardSession) error {
	if session.Phase != updatePhaseSucceeded || session.SchemaVersion != updateGuardStateSchemaVersion ||
		session.BackupPath != updateBackupPath(session.ExecutablePath, session.AttemptID) {
		return nil
	}
	if _, err := os.Lstat(session.BackupPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := verifyUpdateArtifactFile(session.BackupPath, session.PreviousIdentity); err != nil {
		return fmt.Errorf("validate completed attempt backup: %w", err)
	}
	if err := os.Remove(session.BackupPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(session.BackupPath))
}

func removeUpdateCandidate(session updateGuardSession) error {
	candidatePath := updateCandidatePath(session.ExecutablePath, session.AttemptID)
	if candidatePath == "" || candidatePath != session.CandidatePath {
		return nil
	}
	if err := os.Remove(candidatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove update candidate: %w", err)
	}
	return nil
}

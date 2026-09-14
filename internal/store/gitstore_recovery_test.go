package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestInstallRecoveredGitDirectoryRetainsBackupWhenRestoreFails(t *testing.T) {
	backupPath := filepath.Join("recovery", "corrupt.git")
	installErr := errors.New("install failed")
	restoreErr := errors.New("restore failed")
	calls := 0
	rename := func(_, _ string) error {
		calls++
		switch calls {
		case 1:
			return nil
		case 2:
			return installErr
		default:
			return restoreErr
		}
	}

	retain, errInstall := installRecoveredGitDirectory(
		"repo/.git", "clone/.git", backupPath, rename,
	)
	if !retain {
		t.Fatal("retain recovery = false, want true after failed rollback")
	}
	if !errors.Is(errInstall, installErr) || !errors.Is(errInstall, restoreErr) {
		t.Fatalf("install error = %v, want install and restore failures", errInstall)
	}
	if !strings.Contains(errInstall.Error(), backupPath) {
		t.Fatalf("install error = %q, want retained backup path %q", errInstall, backupPath)
	}
}

func TestInstallRecoveredGitDirectoryRestoresOriginalOnInstallFailure(t *testing.T) {
	installErr := errors.New("install failed")
	calls := 0
	rename := func(_, _ string) error {
		calls++
		if calls == 2 {
			return installErr
		}
		return nil
	}

	retain, errInstall := installRecoveredGitDirectory(
		"repo/.git", "clone/.git", "recovery/corrupt.git", rename,
	)
	if retain {
		t.Fatal("retain recovery = true, want false after successful restore")
	}
	if !errors.Is(errInstall, installErr) {
		t.Fatalf("install error = %v, want install failure", errInstall)
	}
	if calls != 3 {
		t.Fatalf("rename calls = %d, want backup, install, and restore", calls)
	}
}

func TestGitTokenStoreCorruptionRecoveryUsesLatestRemoteAndPreservesLocalChanges(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	owner := newRecoveryTestStore(t, remoteDir, filepath.Join(root, "owner"))
	saveRecoveryTestAuth(t, owner, "victim.json", "remote-old")

	workspaceDir := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remoteDir, workspaceDir)
	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("initialize workspace: %v", errEnsure)
	}
	configPath := store.ConfigPath()
	if errWrite := os.WriteFile(configPath, []byte("source: local\n"), 0o600); errWrite != nil {
		t.Fatalf("write local config: %v", errWrite)
	}
	untrackedPath := filepath.Join(workspaceDir, "local.txt")
	if errWrite := os.WriteFile(untrackedPath, []byte("local untracked\n"), 0o600); errWrite != nil {
		t.Fatalf("write untracked file: %v", errWrite)
	}
	saveRecoveryTestAuth(t, owner, "victim.json", "remote-new")
	saveRecoveryTestAuth(t, owner, "remote-only.json", "remote-only")
	removeRecoveryTestHeadFileObject(t, workspaceDir, "corrupt-object.txt")

	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("recover workspace: %v", errEnsure)
	}
	assertRecoveryTestFile(t, configPath, "source: local\n")
	assertRecoveryTestFile(t, untrackedPath, "local untracked\n")
	assertRecoveryTestAuthToken(t, filepath.Join(store.AuthDir(), "victim.json"), "remote-new")
	assertRecoveryTestAuthToken(t, filepath.Join(store.AuthDir(), "remote-only.json"), "remote-only")

	saveRecoveryTestAuth(t, store, "after-recovery.json", "local")
	cloneDir := filepath.Join(root, "verification")
	clone, errClone := git.PlainClone(cloneDir, &git.CloneOptions{URL: remoteDir})
	if errClone != nil {
		t.Fatalf("clone remote after recovery: %v", errClone)
	}
	defer closeGitRepository(clone, "close recovery verification clone")
	assertRecoveryTestAuthToken(t, filepath.Join(cloneDir, "auths", "after-recovery.json"), "local")
}

func TestGitTokenStoreCorruptionRecoveryUsesLatestRemoteDeletion(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	owner := newRecoveryTestStore(t, remoteDir, filepath.Join(root, "owner"))
	saveRecoveryTestAuth(t, owner, "victim.json", "remote")
	workspaceDir := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remoteDir, workspaceDir)
	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("initialize workspace: %v", errEnsure)
	}
	victimPath := filepath.Join(store.AuthDir(), "victim.json")
	if errDelete := owner.Delete(
		context.Background(), filepath.Join(owner.AuthDir(), "victim.json"),
	); errDelete != nil {
		t.Fatalf("delete remote auth: %v", errDelete)
	}
	removeRecoveryTestHeadFileObject(t, workspaceDir, "corrupt-object.txt")

	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("recover workspace: %v", errEnsure)
	}
	if _, errStat := os.Stat(victimPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("remote-deleted auth stat error = %v, want not exist", errStat)
	}
}

func TestGitTokenStoreCorruptionRecoveryPreservesLocalDeletion(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	owner := newRecoveryTestStore(t, remoteDir, filepath.Join(root, "owner"))
	saveRecoveryTestAuth(t, owner, "deleted.json", "remote")
	workspaceDir := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remoteDir, workspaceDir)
	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("initialize workspace: %v", errEnsure)
	}
	deletedPath := filepath.Join(store.AuthDir(), "deleted.json")
	removeRecoveryTestHeadFileObject(t, workspaceDir, "corrupt-object.txt")
	if errRemove := os.Remove(deletedPath); errRemove != nil {
		t.Fatalf("remove local tracked auth: %v", errRemove)
	}

	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("recover workspace: %v", errEnsure)
	}
	if _, errStat := os.Stat(deletedPath); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("locally deleted auth stat error = %v, want not exist", errStat)
	}
}

func TestGitTokenStoreCorruptionRecoveryRejectsSamePathConflict(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	owner := newRecoveryTestStore(t, remoteDir, filepath.Join(root, "owner"))
	saveRecoveryTestAuth(t, owner, "victim.json", "remote-old")
	workspaceDir := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remoteDir, workspaceDir)
	if errEnsure := store.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("initialize workspace: %v", errEnsure)
	}
	victimPath := filepath.Join(store.AuthDir(), "victim.json")
	localContents := `{"type":"codex","access_token":"local"}`
	if errWrite := os.WriteFile(victimPath, []byte(localContents), 0o600); errWrite != nil {
		t.Fatalf("write local auth: %v", errWrite)
	}
	saveRecoveryTestAuth(t, owner, "victim.json", "remote-new")
	removeRecoveryTestHeadFileObject(t, workspaceDir, "corrupt-object.txt")

	errEnsure := store.ensureRepositoryContext(context.Background())
	if errEnsure == nil || !strings.Contains(errEnsure.Error(), "conflicts with local change") {
		t.Fatalf("recovery error = %v, want fail-closed conflict", errEnsure)
	}
	assertRecoveryTestFile(t, victimPath, localContents)
}

func TestGitTokenStoreFullObjectCorruptionFailsClosed(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	workspaceDir := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remoteDir, workspaceDir)
	authPath := saveRecoveryTestAuth(t, store, "dirty.json", "remote")
	localContents := `{"type":"codex","access_token":"local-dirty"}`
	if errWrite := os.WriteFile(authPath, []byte(localContents), 0o600); errWrite != nil {
		t.Fatalf("write dirty auth: %v", errWrite)
	}
	corruptRecoveryTestRepository(t, workspaceDir)

	errEnsure := store.ensureRepositoryContext(context.Background())
	if errEnsure == nil || !strings.Contains(errEnsure.Error(), "inspect recovery baseline") {
		t.Fatalf("recovery error = %v, want missing baseline failure", errEnsure)
	}
	assertRecoveryTestFile(t, authPath, localContents)
}

func TestGitTokenStoreRecoveryHonorsCancelledContext(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	if _, errInit := git.PlainInit(remoteDir, true); errInit != nil {
		t.Fatalf("initialize remote repository: %v", errInit)
	}
	workspaceDir := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remoteDir, workspaceDir)
	saveRecoveryTestAuth(t, store, "saved.json", "remote")
	beforeGitInfo, errStat := os.Stat(filepath.Join(workspaceDir, ".git"))
	if errStat != nil {
		t.Fatalf("stat repository before cancelled recovery: %v", errStat)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if errRecover := store.recoverRepositoryLocked(
		ctx, workspaceDir, store.gitClientOptions(), nil, nil,
	); !errors.Is(errRecover, context.Canceled) {
		t.Fatalf("cancelled recovery error = %v, want context.Canceled", errRecover)
	}
	afterGitInfo, errStat := os.Stat(filepath.Join(workspaceDir, ".git"))
	if errStat != nil {
		t.Fatalf("stat repository after cancelled recovery: %v", errStat)
	}
	if !os.SameFile(beforeGitInfo, afterGitInfo) {
		t.Fatal("cancelled recovery replaced the repository")
	}
}

func newRecoveryTestStore(t *testing.T, remoteDir, repoDir string) *GitTokenStore {
	t.Helper()
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	return store
}

func saveRecoveryTestAuth(t *testing.T, store *GitTokenStore, name, token string) string {
	t.Helper()
	path, errSave := store.Save(
		context.Background(), &cliproxyauth.Auth{
			ID:       name,
			FileName: name,
			Provider: "codex",
			Metadata: map[string]any{"type": "codex", "access_token": token},
		},
	)
	if errSave != nil {
		t.Fatalf("save auth %s: %v", name, errSave)
	}
	return path
}

func removeRecoveryTestHeadFileObject(t *testing.T, repoDir, path string) {
	t.Helper()
	repo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("open repository before object removal: %v", errOpen)
	}
	defer closeGitRepository(repo, "close object removal repository")
	worktree, errWorktree := repo.Worktree()
	if errWorktree != nil {
		t.Fatalf("open worktree before object removal: %v", errWorktree)
	}
	fullPath := filepath.Join(repoDir, filepath.FromSlash(path))
	if errWrite := os.WriteFile(fullPath, []byte("corrupt me\n"), 0o600); errWrite != nil {
		t.Fatalf("write corruption marker: %v", errWrite)
	}
	if _, errAdd := worktree.Add(path); errAdd != nil {
		t.Fatalf("add corruption marker: %v", errAdd)
	}
	if _, errCommit := worktree.Commit(
		"Add corruption marker", &git.CommitOptions{
			Author: &object.Signature{
				Name: "CLIProxyAPI", Email: "cliproxy@local", When: time.Unix(1711929600, 0),
			},
		},
	); errCommit != nil {
		t.Fatalf("commit corruption marker: %v", errCommit)
	}
	tree, errTree := repositoryHeadTree(repo)
	if errTree != nil {
		t.Fatalf("read repository tree: %v", errTree)
	}
	file, errFile := tree.File(path)
	if errFile != nil {
		t.Fatalf("read repository file %s: %v", path, errFile)
	}
	objectPath := filepath.Join(repoDir, ".git", "objects", file.Hash.String()[:2], file.Hash.String()[2:])
	if errRemove := os.Remove(objectPath); errRemove != nil {
		t.Fatalf("remove repository object for %s: %v", path, errRemove)
	}
	if errVerify := verifyRepositoryHead(repo); !isRepositoryCorruptionError(errVerify) {
		t.Fatalf("verifyRepositoryHead error = %v, want repository corruption", errVerify)
	}
}

func corruptRecoveryTestRepository(t *testing.T, repoDir string) {
	t.Helper()
	repo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("open repository before corruption: %v", errOpen)
	}
	defer closeGitRepository(repo, "close full corruption repository")
	if errRepack := repo.RepackObjects(&git.RepackConfig{}); errRepack != nil {
		t.Fatalf("repack repository objects: %v", errRepack)
	}
	objectsDir := filepath.Join(repoDir, ".git", "objects")
	objectEntries, errReadDir := os.ReadDir(objectsDir)
	if errReadDir != nil {
		t.Fatalf("read object directory: %v", errReadDir)
	}
	for _, entry := range objectEntries {
		if entry.IsDir() && len(entry.Name()) == 2 {
			if errRemove := os.RemoveAll(filepath.Join(objectsDir, entry.Name())); errRemove != nil {
				t.Fatalf("remove loose object directory %s: %v", entry.Name(), errRemove)
			}
		}
	}
	packfiles, errGlob := filepath.Glob(filepath.Join(objectsDir, "pack", "*.pack"))
	if errGlob != nil {
		t.Fatalf("glob packfiles: %v", errGlob)
	}
	if len(packfiles) == 0 {
		t.Fatal("no packfiles found to corrupt")
	}
	for _, packfile := range packfiles {
		if errRemove := os.Remove(packfile); errRemove != nil {
			t.Fatalf("remove packfile %s: %v", filepath.Base(packfile), errRemove)
		}
	}
}

func assertRecoveryTestFile(t *testing.T, path, want string) {
	t.Helper()
	contents, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read file %s: %v", path, errRead)
	}
	if string(contents) != want {
		t.Fatalf("file %s = %q, want %q", path, contents, want)
	}
}

func assertRecoveryTestAuthToken(t *testing.T, path, want string) {
	t.Helper()
	contents, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read auth %s: %v", path, errRead)
	}
	if !strings.Contains(string(contents), `"access_token":"`+want+`"`) {
		t.Fatalf("auth %s = %s, want token %q", path, contents, want)
	}
}

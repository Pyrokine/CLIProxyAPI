package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestGitTokenStoreInitializesConfiguredBranchOnEmptyRemote(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, remote)
	repoDir := filepath.Join(t.TempDir(), "repo")
	store := NewGitTokenStore(remoteDir, "", "", "feature/gemini-fix")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	if err := store.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	saveRecoveryTestAuth(t, store, "synthetic.json", "synthetic-token")
	branch := plumbing.NewBranchReferenceName("feature/gemini-fix")
	ref, err := remote.Reference(branch, true)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := remote.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"auths/.gitkeep", "config/.gitkeep", "auths/synthetic.json"} {
		if _, err := tree.File(path); err != nil {
			t.Fatalf("configured branch lost %s: %v", path, err)
		}
	}
	local, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, local)
	if head, err := local.Head(); err != nil || head.Name() != branch {
		t.Fatalf("configured local branch: %v error=%v", head, err)
	}
}

func TestGitTokenStoreUnsetBranchFollowsRemoteDefaultOnReopen(t *testing.T) {
	for _, scenario := range []string{"previous explicit branch", "renamed remote default"} {
		t.Run(
			scenario, func(t *testing.T) {
				remoteDir := filepath.Join(t.TempDir(), "remote.git")
				remote, err := git.PlainInit(remoteDir, true)
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestGitRepository(t, remote)
				repoDir := filepath.Join(t.TempDir(), "repo")
				seed := newRecoveryTestStore(t, remoteDir, repoDir)
				saveRecoveryTestAuth(t, seed, "synthetic.json", "old-token")
				original, err := remote.Head()
				if err != nil {
					t.Fatal(err)
				}
				wanted := original.Name()
				unchanged := plumbing.NewBranchReferenceName("develop")
				if scenario == "previous explicit branch" {
					if err := remote.Storer.SetReference(
						plumbing.NewHashReference(
							unchanged, original.Hash(),
						),
					); err != nil {
						t.Fatal(err)
					}
					configured := NewGitTokenStore(remoteDir, "", "", "develop")
					configured.SetBaseDir(filepath.Join(repoDir, "auths"))
					if err := configured.EnsureRepository(); err != nil {
						t.Fatal(err)
					}
				} else {
					wanted = plumbing.NewBranchReferenceName("main")
					unchanged = original.Name()
					if err := remote.Storer.SetReference(
						plumbing.NewHashReference(
							wanted, original.Hash(),
						),
					); err != nil {
						t.Fatal(err)
					}
					if err := remote.Storer.SetReference(
						plumbing.NewSymbolicReference(
							plumbing.HEAD, wanted,
						),
					); err != nil {
						t.Fatal(err)
					}
				}
				store := newRecoveryTestStore(t, remoteDir, repoDir)
				if err := store.EnsureRepository(); err != nil {
					t.Fatal(err)
				}
				local, err := git.PlainOpen(repoDir)
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestGitRepository(t, local)
				if head, err := local.Head(); err != nil || head.Name() != wanted {
					t.Fatalf("remote default was not selected: head=%v wanted=%s error=%v", head, wanted, err)
				}
				saveRecoveryTestAuth(t, store, "synthetic.json", "updated-token")
				if ref, err := remote.Reference(wanted, true); err != nil || ref.Hash() == original.Hash() {
					t.Fatalf("save did not publish to selected branch: %v error=%v", ref, err)
				}
				if ref, err := remote.Reference(unchanged, true); err != nil || ref.Hash() != original.Hash() {
					t.Fatalf("save changed another remote branch: %v error=%v", ref, err)
				}
			},
		)
	}
}

func TestGitTokenStoreDefaultBranchChangePreservesPendingEdits(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "unstaged"
		if staged {
			name = "staged"
		}
		t.Run(
			name, func(t *testing.T) {
				remoteDir := filepath.Join(t.TempDir(), "remote.git")
				remote, err := git.PlainInit(remoteDir, true)
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestGitRepository(t, remote)
				repoDir := filepath.Join(t.TempDir(), "repo")
				store := newRecoveryTestStore(t, remoteDir, repoDir)
				path := saveRecoveryTestAuth(t, store, "synthetic.json", "old-token")
				original, err := remote.Head()
				if err != nil {
					t.Fatal(err)
				}
				branch := plumbing.NewBranchReferenceName("main")
				if err := remote.Storer.SetReference(plumbing.NewHashReference(branch, original.Hash())); err != nil {
					t.Fatal(err)
				}
				if err := remote.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
					t.Fatal(err)
				}
				local, err := git.PlainOpen(repoDir)
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestGitRepository(t, local)
				const pending = `{"type":"codex","access_token":"pending-synthetic-token"}`
				if err := os.WriteFile(path, []byte(pending), 0o600); err != nil {
					t.Fatal(err)
				}
				if staged {
					worktree, err := local.Worktree()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := worktree.Add("auths/synthetic.json"); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.EnsureRepository(); !errors.Is(err, git.ErrUnstagedChanges) {
					t.Fatalf("branch change with pending edits: %v", err)
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != pending {
					t.Fatalf("pending edits changed: %q error=%v", data, err)
				}
				if head, err := local.Head(); err != nil || head.Name() != original.Name() ||
					head.Hash() != original.Hash() {
					t.Fatalf("rejected branch change moved HEAD: %v error=%v", head, err)
				}
				worktree, err := local.Worktree()
				if err != nil {
					t.Fatal(err)
				}
				status, err := worktree.Status()
				if err != nil {
					t.Fatal(err)
				}
				file := status.File("auths/synthetic.json")
				if staged && file.Staging != git.Modified || !staged && file.Worktree != git.Modified {
					t.Fatalf("pending edit staging changed: %+v", file)
				}
			},
		)
	}
}

func TestGitTokenStoreMissingConfiguredBranchPreservesLocalIdentity(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, remote)
	repoDir := filepath.Join(t.TempDir(), "repo")
	seed := newRecoveryTestStore(t, remoteDir, repoDir)
	saveRecoveryTestAuth(t, seed, "synthetic.json", "synthetic-token")
	before, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	store := NewGitTokenStore(remoteDir, "", "", "missing-branch")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	if err := store.EnsureRepository(); err == nil {
		t.Fatal("missing remote branch was accepted")
	}
	local, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, local)
	if head, err := local.Head(); err != nil || head.Name() != before.Name() || head.Hash() != before.Hash() {
		t.Fatalf("rejected branch changed local identity: head=%v error=%v", head, err)
	}
	if _, err := local.Reference(plumbing.NewBranchReferenceName("missing-branch"), false); err == nil {
		t.Fatal("rejected branch was created locally")
	}
}

func TestGitTokenStoreRepeatedDeleteAndWatcherRemovalAreIdempotent(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, remote)
	repoDir := filepath.Join(t.TempDir(), "repo")
	store := newRecoveryTestStore(t, remoteDir, repoDir)
	path := saveRecoveryTestAuth(t, store, "synthetic.json", "synthetic-token")
	if err := store.Delete(context.Background(), "synthetic.json"); err != nil {
		t.Fatal(err)
	}
	before, err := remote.Head()
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"repeat Delete", func() error { return store.Delete(context.Background(), "synthetic.json") }},
		{
			"watcher removal",
			func() error { return store.PersistAuthFiles(context.Background(), "watcher removal", path) },
		},
	} {
		t.Run(
			operation.name, func(t *testing.T) {
				if err := operation.run(); err != nil {
					t.Fatal(err)
				}
				if head, err := remote.Head(); err != nil || head.Hash() != before.Hash() {
					t.Fatalf("repeated removal changed remote: %v error=%v", head, err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("removed credential reappeared: %v", err)
				}
				if auths, err := store.List(context.Background()); err != nil || len(auths) != 0 {
					t.Fatalf("removed credential still listed: %v error=%v", auths, err)
				}
			},
		)
	}
}

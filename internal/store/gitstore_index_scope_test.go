package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-git/v5"
	gitindex "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestGitTokenStoreScopedCommitSupportsDirectoryAndRoot(t *testing.T) {
	for _, scope := range []string{"auths", "."} {
		t.Run(
			scope, func(t *testing.T) {
				ctx := context.Background()
				remoteDir := filepath.Join(t.TempDir(), "remote.git")
				remote, err := git.PlainInit(remoteDir, true)
				if err != nil {
					t.Fatal(err)
				}
				if err := closeGitRepository(remote, "close Git index scope remote"); err != nil {
					t.Fatal(err)
				}
				repoDir := filepath.Join(t.TempDir(), "repo")
				store := NewGitTokenStore(remoteDir, "", "", "")
				store.SetBaseDir(filepath.Join(repoDir, "auths"))
				target, err := store.Save(
					ctx,
					&coreauth.Auth{
						ID:       "target.json",
						FileName: "target.json",
						Metadata: map[string]any{"type": "xai"},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				notes := filepath.Join(repoDir, "notes.txt")
				if err := os.WriteFile(notes, []byte("old notes"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := store.PersistAuthFiles(ctx, "seed notes", notes); err != nil {
					t.Fatal(err)
				}
				repo, err := git.PlainOpen(repoDir)
				if err != nil {
					t.Fatal(err)
				}
				worktree, err := repo.Worktree()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(notes, []byte("new notes"), 0o755); err != nil {
					t.Fatal(err)
				}
				if _, err := worktree.Add("notes.txt"); err != nil {
					t.Fatal(err)
				}
				if err := closeGitRepository(repo, "close Git index scope test repository"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					target, []byte(`{"type":"xai","access_token":"new-target"}`), 0o600,
				); err != nil {
					t.Fatal(err)
				}
				if err := store.PersistAuthFiles(
					ctx, "persist selected directory", filepath.Join(repoDir, scope),
				); err != nil {
					t.Fatal(err)
				}
				cloneDir := filepath.Join(t.TempDir(), "clone")
				cloned, err := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: remoteDir})
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestGitRepository(t, cloned)
				if data, err := os.ReadFile(
					filepath.Join(
						cloneDir, "auths", "target.json",
					),
				); err != nil || !bytes.Contains(data, []byte("new-target")) {
					t.Fatalf("selected descendant was not committed: %q error=%v", data, err)
				}
				want := "old notes"
				if scope == "." {
					want = "new notes"
				}
				if data, err := os.ReadFile(filepath.Join(cloneDir, "notes.txt")); err != nil ||
					string(data) != want {
					t.Fatalf("directory scope changed: notes=%q want=%q error=%v", data, want, err)
				}
				if info, err := os.Stat(
					filepath.Join(
						cloneDir, "notes.txt",
					),
				); err != nil || info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("base executable mode was lost: %v", err)
				}
			},
		)
	}
}

func TestGitTokenStoreCommitsOnlyRequestedPaths(t *testing.T) {
	for _, operation := range []string{"save", "delete", "no-op", "push-failure"} {
		for _, staged := range []string{"added", "modified", "deleted"} {
			t.Run(
				operation+"/unrelated-"+staged, func(t *testing.T) {
					ctx := context.Background()
					remoteDir := filepath.Join(t.TempDir(), "remote.git")
					remote, err := git.PlainInit(remoteDir, true)
					if err != nil {
						t.Fatal(err)
					}
					if err := closeGitRepository(remote, "close Git index scope remote"); err != nil {
						t.Fatal(err)
					}
					repoDir := filepath.Join(t.TempDir(), "repo")
					store := NewGitTokenStore(remoteDir, "", "", "")
					store.SetBaseDir(filepath.Join(repoDir, "auths"))
					target := &coreauth.Auth{
						ID: "target.json", FileName: "target.json", Provider: "xai",
						Metadata: map[string]any{"type": "xai", "access_token": "old-target"},
					}
					targetPath, err := store.Save(ctx, target)
					if err != nil {
						t.Fatal(err)
					}
					other := &coreauth.Auth{
						ID: "unrelated.json", FileName: "unrelated.json", Provider: "xai",
						Metadata: map[string]any{"type": "xai", "access_token": "old-unrelated"},
					}
					otherPath, err := store.Save(ctx, other)
					if err != nil {
						t.Fatal(err)
					}
					originalOther, err := os.ReadFile(otherPath)
					if err != nil {
						t.Fatal(err)
					}
					repo, err := git.PlainOpen(repoDir)
					if err != nil {
						t.Fatal(err)
					}
					worktree, err := repo.Worktree()
					if err != nil {
						t.Fatal(err)
					}
					rel := "auths/unrelated.json"
					if staged == "added" {
						rel = "notes.txt"
						otherPath = filepath.Join(repoDir, rel)
					}
					if staged == "deleted" {
						if _, err := worktree.Remove(rel); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.WriteFile(otherPath, []byte("staged-only synthetic content"), 0o600); err != nil {
							t.Fatal(err)
						}
						if _, err := worktree.Add(rel); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(
							otherPath, []byte("unstaged-only synthetic content"), 0o600,
						); err != nil {
							t.Fatal(err)
						}
					}
					index, err := repo.Storer.Index()
					if err != nil {
						t.Fatal(err)
					}
					beforeEntry, beforeEntryErr := index.Entry(rel)
					if beforeEntryErr != nil && !errors.Is(beforeEntryErr, gitindex.ErrEntryNotFound) {
						t.Fatal(beforeEntryErr)
					}
					beforeHead, err := repo.Head()
					if err != nil {
						t.Fatal(err)
					}
					if err := closeGitRepository(repo, "close Git index scope test repository"); err != nil {
						t.Fatal(err)
					}
					if operation == "delete" {
						err = store.Delete(ctx, target.ID)
					} else {
						updated := target.Clone()
						if operation != "no-op" {
							updated.Metadata["access_token"] = "new-target"
						}
						if operation == "push-failure" {
							var cancel context.CancelFunc
							ctx, cancel = context.WithCancel(ctx)
							defer cancel()
							store.beforePush = cancel
						}
						_, err = store.Save(ctx, updated)
					}
					if operation == "push-failure" {
						if !errors.Is(err, coreauth.ErrAuthDurableStateUncertain) {
							t.Fatalf("push cancellation error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					repo, err = git.PlainOpen(repoDir)
					if err != nil {
						t.Fatal(err)
					}
					cleanupTestGitRepository(t, repo)
					index, err = repo.Storer.Index()
					if err != nil {
						t.Fatal(err)
					}
					afterEntry, afterEntryErr := index.Entry(rel)
					if (beforeEntryErr == nil) != (afterEntryErr == nil) || !reflect.DeepEqual(
						beforeEntry, afterEntry,
					) {
						t.Errorf(
							"unrelated staged entry changed: before=%+v after=%+v error=%v",
							beforeEntry,
							afterEntry,
							afterEntryErr,
						)
					}
					localHead, err := repo.Head()
					if err != nil {
						t.Fatal(err)
					}
					if (operation == "no-op" || operation == "push-failure") &&
						localHead.Hash() != beforeHead.Hash() {
						t.Error("unchanged or rolled-back operation advanced HEAD")
					}
					if staged == "deleted" {
						if _, err := os.Stat(otherPath); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("unrelated working deletion changed: %v", err)
						}
					} else if data, err := os.ReadFile(otherPath); err !=
						nil ||
						string(data) !=
							("unstaged-only "+
								"synthetic "+
								"content") {
						t.Fatalf("unrelated worktree changed: data=%q error=%v", data, err)
					}
					remote, err = git.PlainOpen(remoteDir)
					if err != nil {
						t.Fatal(err)
					}
					cleanupTestGitRepository(t, remote)
					ref, err := remote.Reference(beforeHead.Name(), true)
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
					file, err := tree.File(rel)
					if staged == "added" {
						if !errors.Is(err, object.ErrFileNotFound) {
							t.Fatalf("unrelated staged addition reached remote: %v", err)
						}
					} else {
						if err != nil {
							t.Fatalf("unrelated committed file was deleted remotely: %v", err)
						}
						content, err := file.Contents()
						if err != nil || !bytes.Equal([]byte(content), originalOther) {
							t.Fatalf("unrelated staged content reached remote: content=%q error=%v", content, err)
						}
					}
					file, err = tree.File("auths/target.json")
					if operation == "delete" {
						if !errors.Is(err, object.ErrFileNotFound) {
							t.Fatalf("target deletion was not committed: %v", err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						content, err := file.Contents()
						wantToken := "old-target"
						if operation == "save" {
							wantToken = "new-target"
						}
						if err != nil || !bytes.Contains([]byte(content), []byte(wantToken)) {
							t.Fatalf("target content was not committed or restored: %s error=%v", content, err)
						}
						if operation == "push-failure" {
							if local, err := os.ReadFile(targetPath); err != nil || !bytes.Contains(
								local, []byte(wantToken),
							) {
								t.Fatalf("target rollback failed: %q error=%v", local, err)
							}
						}
					}
				},
			)
		}
	}
}

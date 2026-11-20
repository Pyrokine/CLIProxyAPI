package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestGitTokenStoreRejectsMissingConfiguredRemoteBranch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	bare, err := git.PlainInit(remote, true)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, bare)
	root := t.TempDir()
	seed := NewGitTokenStore(remote, "", "", "")
	seed.SetBaseDir(filepath.Join(root, "auths"))
	path, err := seed.Save(
		context.Background(), &cliproxyauth.Auth{
			ID: "synthetic.json", FileName: "synthetic.json",
			Metadata: map[string]any{"type": "xai", "access_token": "synthetic-token"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seedRepo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, seedRepo)
	head, err := seedRepo.Head()
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{head.Name().Short(), "missing-branch", ""} {
		t.Run(
			branch, func(t *testing.T) {
				store := NewGitTokenStore(remote, "", "", branch)
				store.SetBaseDir(filepath.Join(root, "auths"))
				err := store.EnsureRepository()
				if branch == "missing-branch" {
					if err == nil || !strings.Contains(err.Error(), "remote branch") {
						t.Fatalf("missing configured remote branch error=%v", err)
					}
				} else if err != nil {
					t.Fatalf("existing/default branch error=%v", err)
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("repository preparation changed credential=%q err=%v", after, readErr)
				}
			},
		)
	}
	if _, err := bare.Reference(plumbing.NewBranchReferenceName("missing-branch"), true); err == nil {
		t.Fatal("a missing configured branch was published to the remote")
	}
}

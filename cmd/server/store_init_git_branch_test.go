package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/store"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestGitStoreInitializationReturnsMissingConfiguredBranchWithoutBootstrapping(t *testing.T) {
	remotePath := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainInit(remotePath, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			if err := remote.Storer.(io.Closer).Close(); err != nil {
				t.Errorf("close Git remote: %v", err)
			}
		},
	)
	localRoot := t.TempDir()
	gitRoot := filepath.Join(localRoot, "gitstore")
	seed := store.NewGitTokenStore(remotePath, "", "", "")
	seed.SetBaseDir(filepath.Join(gitRoot, "auths"))
	credentialPath, err := seed.Save(
		t.Context(), &coreauth.Auth{
			ID: "synthetic.json", FileName: "synthetic.json",
			Metadata: map[string]any{"type": "xai", "access_token": "synthetic-token"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	working, err := git.PlainOpen(gitRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			if err := working.Storer.(io.Closer).Close(); err != nil {
				t.Errorf("close Git worktree: %v", err)
			}
		},
	)
	head, err := working.Head()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "config.example.yaml"), []byte("port: 8345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, configPath, tokenStore, err := initGitStore(
		workDir, "", remotePath, "", "", "missing-configured-branch", localRoot, false,
	)
	if err == nil || !strings.Contains(
		err.Error(), "failed to prepare git token store",
	) || !strings.Contains(err.Error(), "remote branch") {
		t.Fatalf("CLI initialization error=%v", err)
	}
	if cfg != nil || configPath != "" || tokenStore != nil {
		t.Fatal("failed Git preparation returned a usable runtime configuration")
	}
	if _, err := os.Stat(filepath.Join(gitRoot, "config", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("failed branch preparation bootstrapped configuration: %v", err)
	}
	if current, err := os.ReadFile(credentialPath); err != nil || string(current) != string(before) {
		t.Fatalf("failed CLI initialization changed credentials: %v", err)
	}
	if _, err := remote.Reference(plumbing.NewBranchReferenceName("missing-configured-branch"), true); err == nil {
		t.Fatal("CLI initialization published the missing branch")
	}
	cfg, configPath, tokenStore, err = initGitStore(
		workDir, "", remotePath, "", "", head.Name().Short(), localRoot, false,
	)
	if err != nil || cfg == nil || cfg.Port != 8345 || tokenStore == nil || configPath == "" {
		t.Fatalf("existing configured branch failed: config=%v path=%q error=%v", cfg, configPath, err)
	}
}

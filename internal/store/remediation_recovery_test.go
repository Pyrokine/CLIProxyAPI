package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5"
)

// Cancel after the worktree moves, while a replacement blocks restoration.
type blockedWorktreeRestoreContext struct {
	context.Context
	cancel                    context.CancelFunc
	mu                        sync.Mutex
	root, workspace, relative string
	injected                  bool
	err                       error
}

func (c *blockedWorktreeRestoreContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.injected {
		matches, err := filepath.Glob(filepath.Join(c.root, ".gitstore-recovery-*", "worktree", c.relative))
		if err != nil {
			c.err = err
			c.cancel()
		}
		if len(matches) > 0 {
			c.injected = true
			conflict := filepath.Join(c.workspace, strings.Split(c.relative, string(filepath.Separator))[0])
			c.err = os.MkdirAll(conflict, 0700)
			if c.err == nil {
				c.err = os.WriteFile(filepath.Join(conflict, "replacement"), []byte("preserve replacement"), 0600)
			}
			c.cancel()
		}
	}
	return c.Context.Err()
}
func TestGitRecoveryRetainsBackupWhenWorktreeRestoreFails(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	store := newRecoveryTestStore(t, remote, workspace)
	authPath := saveRecoveryTestAuth(t, store, "saved.json", "synthetic-original")
	relative, err := filepath.Rel(workspace, authPath)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &blockedWorktreeRestoreContext{
		Context: base, cancel: cancel, root: root, workspace: workspace, relative: relative,
	}
	err = store.recoverRepositoryLocked(
		ctx, workspace, store.gitAuth(), nil, nil, nil, nil,
	)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "backup retained") {
		t.Fatalf("recovery error=%v", err)
	}
	if ctx.err != nil || !ctx.injected {
		t.Fatalf("injection=%v error=%v", ctx.injected, ctx.err)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".gitstore-recovery-*", "worktree", relative))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("retained backups=%v", matches)
	}
	assertRecoveryTestAuthToken(t, matches[0], "synthetic-original")
}

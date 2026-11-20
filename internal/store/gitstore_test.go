package store

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
)

type callbackTokenStorage struct {
	save func(string) error
}

func (s *callbackTokenStorage) SaveTokenToFile(path string) error {
	return s.save(path)
}

func cleanupTestGitRepository(t *testing.T, repo *git.Repository) {
	t.Helper()
	t.Cleanup(
		func() {
			if errClose := closeGitRepository(repo, "close Git test repository"); errClose != nil {
				t.Errorf("close Git repository: %v", errClose)
			}
		},
	)
}

type closingGitTestStorer struct {
	storage.Storer
	err   error
	calls int
}

func (s *closingGitTestStorer) Close() error {
	s.calls++
	return s.err
}

func TestCloseGitRepositoryClosesUnderlyingStorerAndPreservesError(t *testing.T) {
	closeErr := errors.New("synthetic storer close failure")
	for _, failure := range []error{nil, closeErr} {
		storer := &closingGitTestStorer{Storer: memory.NewStorage(), err: failure}
		err := closeGitRepository(&git.Repository{Storer: storer}, "close synthetic repository")
		if storer.calls != 1 || !errors.Is(err, failure) {
			t.Fatalf("close calls=%d error=%v, want one close and %v", storer.calls, err, failure)
		}
		if failure != nil && !strings.Contains(err.Error(), "close synthetic repository") {
			t.Fatalf("close error lost operation: %v", err)
		}
	}
	if err := closeGitRepository(nil, "close absent repository"); err != nil {
		t.Fatal(err)
	}
	if err := closeGitRepository(&git.Repository{Storer: memory.NewStorage()}, "close memory repository"); err != nil {
		t.Fatal(err)
	}
}

type trackingGitPackFilesystem struct {
	billy.Filesystem
	packs []billy.File
}

func (f *trackingGitPackFilesystem) Open(name string) (billy.File, error) {
	file, err := f.Filesystem.Open(name)
	if err == nil && strings.HasSuffix(name, ".pack") {
		f.packs = append(f.packs, file)
	}
	return file, err
}

func TestCloseGitRepositoryReleasesPackedFilesystemHandles(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	remote, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, remote)
	repoDir := filepath.Join(t.TempDir(), "repo")
	store := newRecoveryTestStore(t, remoteDir, repoDir)
	saveRecoveryTestAuth(t, store, "synthetic.json", "synthetic-token")
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, repo)
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RepackObjects(&git.RepackConfig{}); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(repoDir, ".git")
	hash := head.Hash().String()
	if err := os.Remove(filepath.Join(gitDir, "objects", hash[:2], hash[2:])); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := closeGitRepository(repo, "close packed test repository"); err != nil {
		t.Fatal(err)
	}
	tracked := &trackingGitPackFilesystem{Filesystem: osfs.New(gitDir)}
	storer := filesystem.NewStorageWithOptions(
		tracked, cache.NewObjectLRUDefault(), filesystem.Options{KeepDescriptors: true},
	)
	reopened, err := git.Open(storer, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGitRepository(t, reopened)
	if _, err := reopened.CommitObject(head.Hash()); err != nil {
		t.Fatal(err)
	}
	if len(tracked.packs) == 0 {
		t.Fatal("reading packed commit did not open a packfile")
	}
	for _, file := range tracked.packs {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("packfile was closed before repository close: %v", err)
		}
	}
	if err := closeGitRepository(reopened, "close retained packfile handles"); err != nil {
		t.Fatal(err)
	}
	for _, file := range tracked.packs {
		if _, err := file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("packfile read after repository close=%v, want os.ErrClosed", err)
		}
	}
	if err := os.Rename(gitDir, filepath.Join(repoDir, ".git.closed")); err != nil {
		t.Fatalf("replace Git directory after repository close: %v", err)
	}
}

func TestGitTokenStoreGitAuth(t *testing.T) {
	for _, test := range []struct {
		name, remote, username, password, expectedUser string
	}{
		{name: "anonymous", remote: "https://example.invalid/repo.git"},
		{
			name: "username only", remote: "http://example.invalid/repo.git",
			username: "synthetic-user", expectedUser: "synthetic-user",
		},
		{
			name: "password only", remote: "https://example.invalid/repo.git",
			password: "synthetic-password", expectedUser: "git",
		},
		{
			name: "username and password", remote: "https://example.invalid/repo.git",
			username: "synthetic-user", password: "synthetic-password", expectedUser: "synthetic-user",
		},
		{
			name: "uppercase HTTP scheme", remote: "HTTPS://example.invalid/repo.git",
			password: "synthetic-password", expectedUser: "git",
		},
		{name: "SSH URL", remote: "ssh://git@example.invalid/repo.git", password: "synthetic-password"},
		{name: "SCP style", remote: "git@example.invalid:repo.git", password: "synthetic-password"},
		{name: "Git TCP", remote: "git://example.invalid/repo.git", password: "synthetic-password"},
		{name: "local path", remote: "repo.git", password: "synthetic-password"},
		{name: "file URL", remote: "file:///repo.git", password: "synthetic-password"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				store := NewGitTokenStore(test.remote, test.username, test.password, "")
				auth := store.gitAuth()
				if test.expectedUser == "" {
					if auth != nil {
						t.Fatalf("auth=%v, want nil", auth)
					}
					return
				}
				basic, ok := auth.(*githttp.BasicAuth)
				if !ok || basic.Username != test.expectedUser || basic.Password != test.password {
					t.Fatalf("auth does not preserve configured basic credentials")
				}
			},
		)
	}
}

func TestGitTokenStoreHTTPAuthReachesTransportAndPreservesError(t *testing.T) {
	for _, operation := range []string{"clone", "list", "push", "recovery clone"} {
		t.Run(
			operation, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							requests.Add(1)
							username, password, ok := r.BasicAuth()
							if !ok || username != "git" || password != "synthetic-password" {
								t.Error("Git transport did not send configured basic credentials")
							}
							w.WriteHeader(http.StatusForbidden)
						},
					),
				)
				defer server.Close()
				remoteDir := filepath.Join(t.TempDir(), "remote.git")
				remote, err := git.PlainInit(remoteDir, true)
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestGitRepository(t, remote)
				repoDir := filepath.Join(t.TempDir(), "repo")
				store := newRecoveryTestStore(t, remoteDir, repoDir)
				if operation != "clone" {
					saveRecoveryTestAuth(t, store, "synthetic.json", "synthetic-token")
				}
				store.remote = server.URL
				store.password = "synthetic-password"
				switch operation {
				case "clone":
					err = store.EnsureRepository()
				case "recovery clone":
					err = store.recoverRepositoryLocked(t.Context(), repoDir, store.gitAuth(), nil, nil, nil, nil)
				default:
					repo, errOpen := git.PlainOpen(repoDir)
					if errOpen != nil {
						t.Fatal(errOpen)
					}
					cleanupTestGitRepository(t, repo)
					cfg, errConfig := repo.Config()
					if errConfig != nil {
						t.Fatal(errConfig)
					}
					cfg.Remotes["origin"].URLs = []string{server.URL}
					if errConfig := repo.SetConfig(cfg); errConfig != nil {
						t.Fatal(errConfig)
					}
					if operation == "list" {
						err = store.EnsureRepository()
					} else {
						err = store.pushRepositoryHead(t.Context(), repo)
					}
				}
				if requests.Load() == 0 || !errors.Is(err, transport.ErrAuthorizationFailed) {
					t.Fatalf(
						"Git %s requests=%d error=%v, want HTTP authorization failure", operation, requests.Load(), err,
					)
				}
			},
		)
	}
}

func TestDisableGitCommitSigning(t *testing.T) {
	repoDir := t.TempDir()
	repo, errInit := git.PlainInit(repoDir, false)
	if errInit != nil {
		t.Fatalf("git.PlainInit() error = %v", errInit)
	}
	cfg, errConfig := repo.Config()
	if errConfig != nil {
		t.Fatalf("repo.Config() error = %v", errConfig)
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "true")
	cfg.Raw.Section("commit").SetOption("template", "synthetic-template")
	if errSetConfig := repo.SetConfig(cfg); errSetConfig != nil {
		t.Fatalf("repo.SetConfig() error = %v", errSetConfig)
	}
	if err := closeGitRepository(repo, "close signing test repository"); err != nil {
		t.Fatal(err)
	}

	if errDisable := disableGitCommitSigning(repoDir); errDisable != nil {
		t.Fatalf("disableGitCommitSigning() error = %v", errDisable)
	}
	reopened, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("git.PlainOpen() error = %v", errOpen)
	}
	defer func(repository *git.Repository) {
		if err := closeGitRepository(repository, "close reopened signing test repository"); err != nil {
			t.Error(err)
		}
	}(reopened)
	gotConfig, errGetConfig := reopened.Config()
	if errGetConfig != nil {
		t.Fatalf("reopened.Config() error = %v", errGetConfig)
	}
	if got := gotConfig.Raw.Section("commit").Option("gpgsign"); got != "false" {
		t.Fatalf("commit.gpgsign = %q, want false", got)
	}
	if got := gotConfig.Raw.Section("commit").Option("template"); got != "synthetic-template" {
		t.Fatalf("commit.template = %q, want preserved value", got)
	}
}

func TestNormalizeGitStoreBranch(t *testing.T) {
	tests := map[string]string{
		" main ":          "main",
		"refs/heads/main": "main",
		"/release/":       "release",
		"":                "",
	}
	for input, want := range tests {
		if got := normalizeGitStoreBranch(input); got != want {
			t.Fatalf("normalizeGitStoreBranch(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeGitPathUsesForwardSlashes(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	if got := normalizeGitPath(`auths\nested\token.json`); got != "auths/nested/token.json" {
		t.Fatalf("normalizeGitPath() = %q, want forward slashes", got)
	}
}

func TestGitTokenStoreBranchRef(t *testing.T) {
	store := NewGitTokenStore("https://example.com/repo.git", "", "", "refs/heads/main")
	if got := store.branchRef(); got != plumbing.NewBranchReferenceName("main") {
		t.Fatalf("branchRef() = %q, want refs/heads/main", got)
	}
}

type blockingJSONMarshaler struct {
	started chan struct{}
	release <-chan struct{}
}

type cancellingGitTokenStorage struct {
	cancel context.CancelFunc
	data   []byte
}

func (s *cancellingGitTokenStorage) SaveTokenToFile(path string) error {
	if err := os.WriteFile(path, s.data, 0o600); err != nil {
		return err
	}
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (m blockingJSONMarshaler) MarshalJSON() ([]byte, error) {
	close(m.started)
	<-m.release
	return []byte(`"value"`), nil
}

func TestGitTokenStoreSaveRejectsCancellationWhileWaitingForOperationLock(t *testing.T) {
	rootDir := t.TempDir()
	authDir := filepath.Join(rootDir, "auths")
	store := NewGitTokenStore("https://example.com/repo.git", "", "", "")
	store.SetBaseDir(authDir)
	store.operationMu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.Save(
			ctx, &cliproxyauth.Auth{
				ID:       "cancelled.json",
				FileName: "cancelled.json",
				Metadata: map[string]any{"type": "xai"},
			},
		)
		result <- err
	}()
	cancel()
	store.operationMu.Unlock()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "cancelled.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled auth file stat error = %v, want os.ErrNotExist", err)
	}
}

func TestGitTokenStoreEnsureRepositoryRejectsCancellationWhileWaitingForOperationLock(t *testing.T) {
	store := NewGitTokenStore("https://example.com/repo.git", "", "", "")
	store.SetBaseDir(filepath.Join(t.TempDir(), "auths"))
	store.operationMu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- store.ensureRepositoryContext(ctx)
	}()
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnsureRepositoryContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnsureRepositoryContext() did not stop while waiting for operation lock")
	}
	store.operationMu.Unlock()
}

func TestGitTokenStoreSaveStopsWhenMetadataMarshalCancelsContext(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	authDir := filepath.Join(t.TempDir(), "auths")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(authDir)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := store.Save(
			ctx, &cliproxyauth.Auth{
				ID:       "cancelled.json",
				FileName: "cancelled.json",
				Metadata: map[string]any{
					"blocking": blockingJSONMarshaler{started: started, release: release},
				},
			},
		)
		result <- err
	}()
	<-started
	cancel()
	close(release)

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "cancelled.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled auth file stat error = %v, want os.ErrNotExist", err)
	}
}

func TestGitTokenStoreSaveRollsBackFileWhenContextCancelsAfterWrite(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	initial := &cliproxyauth.Auth{
		ID:       "rollback.json",
		FileName: "rollback.json",
		Metadata: map[string]any{"type": "xai", "access_token": "old"},
	}
	path, errSave := store.Save(context.Background(), initial)
	if errSave != nil {
		t.Fatalf("initial Save() error = %v", errSave)
	}
	before, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read initial auth: %v", errRead)
	}
	repo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("open repository: %v", errOpen)
	}
	beforeHead, errHead := repo.Head()
	if errHead != nil {
		t.Fatalf("read initial head: %v", errHead)
	}
	_ = closeGitRepository(repo, "close Git test repository")

	ctx, cancel := context.WithCancel(context.Background())
	updated := initial.Clone()
	updated.Storage = &cancellingGitTokenStorage{
		cancel: cancel,
		data:   []byte(`{"type":"xai","access_token":"new"}`),
	}
	updated.Metadata = nil
	if _, errUpdate := store.Save(ctx, updated); !errors.Is(errUpdate, context.Canceled) {
		t.Fatalf("Save() error = %v, want context.Canceled", errUpdate)
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read rolled back auth: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("auth file after cancellation = %s, want %s", after, before)
	}
	repo, errOpen = git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("reopen repository: %v", errOpen)
	}
	cleanupTestGitRepository(t, repo)
	afterHead, errHead := repo.Head()
	if errHead != nil {
		t.Fatalf("read rolled back head: %v", errHead)
	}
	if afterHead.Hash() != beforeHead.Hash() {
		t.Fatalf("head after cancellation = %s, want %s", afterHead.Hash(), beforeHead.Hash())
	}
}

func TestGitTokenStoreSaveReportsUncertaintyWhenRollbackFails(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(t.TempDir(), "auths"))
	initial := &cliproxyauth.Auth{
		ID:       "rollback-failure.json",
		FileName: "rollback-failure.json",
		Metadata: map[string]any{"type": "xai", "access_token": "old"},
	}
	path, errSave := store.Save(context.Background(), initial)
	if errSave != nil {
		t.Fatalf("initial Save() error = %v", errSave)
	}

	ctx, cancel := context.WithCancel(context.Background())
	store.beforeRollback = func(target string) {
		_ = os.Remove(target)
		_ = os.Mkdir(target, 0o700)
	}
	t.Cleanup(
		func() {
			store.beforeRollback = nil
			_ = os.RemoveAll(path)
		},
	)
	updated := initial.Clone()
	updated.Storage = &cancellingGitTokenStorage{
		cancel: cancel,
		data:   []byte(`{"type":"xai","access_token":"new"}`),
	}
	updated.Metadata = nil
	if _, errUpdate := store.Save(ctx, updated); !errors.Is(
		errUpdate,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("Save() error = %v, want durable state uncertainty", errUpdate)
	}
}

func TestGitTokenStoreSaveRollsBackLocalStateWhenPushIsUncertain(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	initial := &cliproxyauth.Auth{
		ID:       "uncertain.json",
		FileName: "uncertain.json",
		Metadata: map[string]any{"type": "xai", "access_token": "old"},
	}
	path, errSave := store.Save(context.Background(), initial)
	if errSave != nil {
		t.Fatalf("initial Save() error = %v", errSave)
	}
	before, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read initial auth: %v", errRead)
	}
	repo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("open repository: %v", errOpen)
	}
	beforeHead, errHead := repo.Head()
	if errHead != nil {
		t.Fatalf("read initial head: %v", errHead)
	}
	_ = closeGitRepository(repo, "close Git test repository")

	ctx, cancel := context.WithCancel(context.Background())
	store.beforePush = cancel
	t.Cleanup(func() { store.beforePush = nil })
	updated := initial.Clone()
	updated.Metadata = map[string]any{"type": "xai", "access_token": "new"}
	if _, errUpdate := store.Save(ctx, updated); !errors.Is(
		errUpdate,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("Save() error = %v, want durable state uncertainty", errUpdate)
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read rolled back auth: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("auth file after push cancellation = %s, want %s", after, before)
	}
	repo, errOpen = git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("reopen repository: %v", errOpen)
	}
	cleanupTestGitRepository(t, repo)
	afterHead, errHead := repo.Head()
	if errHead != nil {
		t.Fatalf("read rolled back head: %v", errHead)
	}
	if afterHead.Hash() != beforeHead.Hash() {
		t.Fatalf("head after push cancellation = %s, want %s", afterHead.Hash(), beforeHead.Hash())
	}
}

func TestGitTokenStoreRequiresCreationIntentForMissingDisabledCredential(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	authDir := filepath.Join(t.TempDir(), "auths")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(authDir)
	errReachedStorage := errors.New("reached token storage")
	storageReached := false
	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		FileName: "disabled.json",
		Disabled: true,
		Storage: &callbackTokenStorage{
			save: func(string) error {
				storageReached = true
				return errReachedStorage
			},
		},
		Metadata: map[string]any{"type": "codex"},
	}

	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("runtime Save() error = %v", errSave)
	}
	if path != "" {
		t.Fatalf("runtime Save() path = %q, want empty", path)
	}
	if storageReached {
		t.Fatal("runtime Save() reached token storage for a missing disabled credential")
	}
	if _, errStat := os.Stat(authDir); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("runtime Save() auth directory stat error = %v, want os.ErrNotExist", errStat)
	}

	ctx := cliproxyauth.WithAuthCreationIntent(context.Background())
	_, errSave = store.Save(ctx, auth)
	if !errors.Is(errSave, errReachedStorage) {
		t.Fatalf("creation-intent Save() error = %v, want token storage sentinel", errSave)
	}
	if !storageReached {
		t.Fatal("creation-intent Save() did not reach token storage")
	}
}

func TestGitTokenStoreSameLocalContentStillChecksRemote(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(t.TempDir(), "auths"))
	auth := &cliproxyauth.Auth{
		ID:       "same.json",
		FileName: "same.json",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errSave := store.Save(context.Background(), auth); errSave != nil {
		t.Fatalf("initial Save() error = %v", errSave)
	}
	var pushes atomic.Int32
	store.beforePush = func() { pushes.Add(1) }
	t.Cleanup(func() { store.beforePush = nil })
	if _, errSave := store.Save(context.Background(), auth.Clone()); errSave != nil {
		t.Fatalf("repeated Save() error = %v", errSave)
	}
	if pushes.Load() != 1 {
		t.Fatalf("remote checks for unchanged credential = %d, want 1", pushes.Load())
	}
}

func TestGitTokenStoreDeleteCommitsTrackedFileAlreadyMissingLocally(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(t.TempDir(), "auths"))
	auth := &cliproxyauth.Auth{
		ID:       "deleted.json",
		FileName: "deleted.json",
		Metadata: map[string]any{"type": "codex"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	if errRemove := os.Remove(path); errRemove != nil {
		t.Fatalf("remove local auth before store delete: %v", errRemove)
	}
	if errDelete := store.Delete(context.Background(), path); errDelete != nil {
		t.Fatalf("Delete() error = %v", errDelete)
	}
	cloneDir := t.TempDir()
	remoteRepo, errClone := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: remoteDir})
	if errClone != nil {
		t.Fatalf("clone remote repository: %v", errClone)
	}
	cleanupTestGitRepository(t, remoteRepo)
	if _, errStat := os.Stat(filepath.Join(cloneDir, "auths", auth.FileName)); !os.IsNotExist(errStat) {
		t.Fatalf("remote credential still exists after deletion: %v", errStat)
	}
}

func TestGitTokenStoreRecoversInterruptedVersionedDeletionOnRepositoryOpen(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	authDir := filepath.Join(repoDir, "auths")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(authDir)
	auth := &cliproxyauth.Auth{
		ID:       "interrupted.json",
		FileName: "interrupted.json",
		Metadata: map[string]any{"type": "codex", "token": "old"},
	}
	path, errSave := store.Save(context.Background(), auth)
	if errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	if _, matched, errPrepare := cliproxyauth.PrepareDurablePersistedFileDeletion(
		path, version,
	); errPrepare != nil || !matched {
		t.Fatalf("PrepareDurablePersistedFileDeletion() matched = %v, error = %v", matched, errPrepare)
	}

	restarted := NewGitTokenStore(remoteDir, "", "", "")
	restarted.SetBaseDir(authDir)
	if errEnsure := restarted.ensureRepositoryContext(context.Background()); errEnsure != nil {
		t.Fatalf("EnsureRepositoryContext() error = %v", errEnsure)
	}
	cloneDir := t.TempDir()
	remoteRepo, errClone := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: remoteDir})
	if errClone != nil {
		t.Fatalf("clone remote repository: %v", errClone)
	}
	cleanupTestGitRepository(t, remoteRepo)
	if _, errStat := os.Stat(filepath.Join(cloneDir, "auths", auth.FileName)); !os.IsNotExist(errStat) {
		t.Fatalf("remote credential was resurrected after recovery: %v", errStat)
	}
	pending, errPending := cliproxyauth.RecoverDurablePersistedFileDeletions(authDir)
	if errPending != nil || len(pending) != 0 {
		t.Fatalf("pending deletions after recovery = %#v, %v", pending, errPending)
	}
}

func TestGitTokenStoreRecoveryRejectsNewerRemoteCredential(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo-a")
	authDir := filepath.Join(repoDir, "auths")
	storeA := NewGitTokenStore(remoteDir, "", "", "")
	storeA.SetBaseDir(authDir)
	original := &cliproxyauth.Auth{
		ID:       "interrupted.json",
		FileName: "interrupted.json",
		Metadata: map[string]any{"type": "codex", "token": "old"},
	}
	path, errSave := storeA.Save(context.Background(), original)
	if errSave != nil {
		t.Fatalf("store A Save() error = %v", errSave)
	}
	version, errVersion := cliproxyauth.ReadPersistedVersion(path)
	if errVersion != nil {
		t.Fatalf("ReadPersistedVersion() error = %v", errVersion)
	}
	if _, matched, errPrepare := cliproxyauth.PrepareDurablePersistedFileDeletion(
		path, version,
	); errPrepare != nil || !matched {
		t.Fatalf("PrepareDurablePersistedFileDeletion() matched = %v, error = %v", matched, errPrepare)
	}

	storeB := NewGitTokenStore(remoteDir, "", "", "")
	storeB.SetBaseDir(filepath.Join(t.TempDir(), "repo-b", "auths"))
	replacement := &cliproxyauth.Auth{
		ID:       original.ID,
		FileName: original.FileName,
		Metadata: map[string]any{"type": "codex", "token": "new"},
	}
	if _, errReplacement := storeB.Save(context.Background(), replacement); errReplacement != nil {
		t.Fatalf("store B replacement Save() error = %v", errReplacement)
	}

	restarted := NewGitTokenStore(remoteDir, "", "", "")
	restarted.SetBaseDir(authDir)
	if errEnsure := restarted.ensureRepositoryContext(context.Background()); errEnsure == nil || !strings.Contains(
		errEnsure.Error(),
		"remote branch diverged",
	) {
		t.Fatalf("EnsureRepositoryContext() error = %v, want remote divergence", errEnsure)
	}
	cloneDir := t.TempDir()
	remoteRepo, errClone := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: remoteDir})
	if errClone != nil {
		t.Fatalf("clone remote repository: %v", errClone)
	}
	cleanupTestGitRepository(t, remoteRepo)
	remote, errReadRemote := os.ReadFile(filepath.Join(cloneDir, "auths", original.FileName))
	if errReadRemote != nil || !strings.Contains(string(remote), `"token":"new"`) {
		t.Fatalf("remote replacement = %s, %v", remote, errReadRemote)
	}
	pending, errPending := cliproxyauth.RecoverDurablePersistedFileDeletions(authDir)
	if errPending != nil || len(pending) != 1 {
		t.Fatalf("pending deletions after rejected replacement = %#v, %v", pending, errPending)
	}
}

func TestGitTokenStoreRejectsRemoteDivergenceBeforeLocalMutation(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	storeA := NewGitTokenStore(remoteDir, "", "", "")
	storeA.SetBaseDir(filepath.Join(t.TempDir(), "store-a", "auths"))
	original := &cliproxyauth.Auth{
		ID:       "shared.json",
		FileName: "shared.json",
		Metadata: map[string]any{"type": "codex", "token": "original"},
	}
	pathA, errSave := storeA.Save(context.Background(), original)
	if errSave != nil {
		t.Fatalf("store A initial Save() error = %v", errSave)
	}
	before, errRead := os.ReadFile(pathA)
	if errRead != nil {
		t.Fatalf("read store A credential: %v", errRead)
	}

	storeB := NewGitTokenStore(remoteDir, "", "", "")
	storeB.SetBaseDir(filepath.Join(t.TempDir(), "store-b", "auths"))
	concurrent := &cliproxyauth.Auth{
		ID:       "concurrent.json",
		FileName: "concurrent.json",
		Metadata: map[string]any{"type": "xai", "token": "concurrent"},
	}
	if _, errConcurrent := storeB.Save(context.Background(), concurrent); errConcurrent != nil {
		t.Fatalf("store B Save() error = %v", errConcurrent)
	}

	updated := original.Clone()
	updated.Metadata = map[string]any{"type": "codex", "token": "updated"}
	if _, errUpdate := storeA.Save(context.Background(), updated); errUpdate == nil ||
		!strings.Contains(errUpdate.Error(), "remote branch diverged") {
		t.Fatalf("store A divergent Save() error = %v", errUpdate)
	}
	after, errRead := os.ReadFile(pathA)
	if errRead != nil {
		t.Fatalf("read store A credential after divergence: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("store A credential changed after divergence: %s, want %s", after, before)
	}
	cloneDir := t.TempDir()
	remoteRepo, errClone := git.PlainClone(cloneDir, false, &git.CloneOptions{URL: remoteDir})
	if errClone != nil {
		t.Fatalf("clone remote after divergence: %v", errClone)
	}
	cleanupTestGitRepository(t, remoteRepo)
	if _, errStat := os.Stat(filepath.Join(cloneDir, "auths", concurrent.FileName)); errStat != nil {
		t.Fatalf("concurrent remote credential missing: %v", errStat)
	}
}

func TestGitTokenStoreLeaseConflictRestoresLocalMutation(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(filepath.Join(repoDir, "auths"))
	initial := &cliproxyauth.Auth{
		ID:       "lease.json",
		FileName: "lease.json",
		Metadata: map[string]any{"type": "xai", "access_token": "old"},
	}
	path, errSave := store.Save(context.Background(), initial)
	if errSave != nil {
		t.Fatalf("initial Save() error = %v", errSave)
	}
	before, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read initial auth: %v", errRead)
	}
	localRepo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("open local repository: %v", errOpen)
	}
	beforeHead, errHead := localRepo.Head()
	if errHead != nil {
		t.Fatalf("read initial head: %v", errHead)
	}
	_ = closeGitRepository(localRepo, "close local Git test repository")

	store.beforePush = func() {
		concurrentDir := t.TempDir()
		concurrentRepo, errClone := git.PlainClone(concurrentDir, false, &git.CloneOptions{URL: remoteDir})
		if errClone != nil {
			t.Fatalf("clone concurrent repository: %v", errClone)
		}
		cleanupTestGitRepository(t, concurrentRepo)
		worktree, errWorktree := concurrentRepo.Worktree()
		if errWorktree != nil {
			t.Fatalf("open concurrent worktree: %v", errWorktree)
		}
		concurrentPath := filepath.Join(concurrentDir, "auths", "concurrent.json")
		if errWrite := os.WriteFile(concurrentPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
			t.Fatalf("write concurrent auth: %v", errWrite)
		}
		if _, errAdd := worktree.Add("auths/concurrent.json"); errAdd != nil {
			t.Fatalf("stage concurrent auth: %v", errAdd)
		}
		signature := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
		if _, errCommit := worktree.Commit(
			"concurrent update", &git.CommitOptions{Author: signature},
		); errCommit != nil {
			t.Fatalf("commit concurrent auth: %v", errCommit)
		}
		if errPush := concurrentRepo.Push(&git.PushOptions{}); errPush != nil {
			t.Fatalf("push concurrent auth: %v", errPush)
		}
	}
	t.Cleanup(func() { store.beforePush = nil })
	updated := initial.Clone()
	updated.Metadata = map[string]any{"type": "xai", "access_token": "new"}
	if _, errUpdate := store.Save(context.Background(), updated); !errors.Is(
		errUpdate,
		cliproxyauth.ErrAuthDurableStateUncertain,
	) {
		t.Fatalf("Save() error = %v, want durable state uncertainty", errUpdate)
	}
	after, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read rolled back auth: %v", errRead)
	}
	if string(after) != string(before) {
		t.Fatalf("auth after lease conflict = %s, want %s", after, before)
	}
	localRepo, errOpen = git.PlainOpen(repoDir)
	if errOpen != nil {
		t.Fatalf("reopen local repository: %v", errOpen)
	}
	cleanupTestGitRepository(t, localRepo)
	afterHead, errHead := localRepo.Head()
	if errHead != nil {
		t.Fatalf("read rolled back head: %v", errHead)
	}
	if afterHead.Hash() != beforeHead.Hash() {
		t.Fatalf("head after lease conflict = %s, want %s", afterHead.Hash(), beforeHead.Hash())
	}
}

func TestGitTokenStoreSaveWithBaseDirRestoresDefaultDirectory(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remoteDir, true); err != nil {
		t.Fatalf("initialize remote repository: %v", err)
	}

	rootDir := t.TempDir()
	defaultAuthDir := filepath.Join(rootDir, "default", "auths")
	scopedAuthDir := filepath.Join(rootDir, "scoped", "auths")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(defaultAuthDir)

	record := &cliproxyauth.Auth{
		ID:       "scoped.json",
		FileName: "scoped.json",
		Metadata: map[string]any{"type": "codex"},
	}
	savedPath, err := store.SaveWithBaseDir(context.Background(), scopedAuthDir, record)
	if err != nil {
		t.Fatalf("SaveWithBaseDir failed: %v", err)
	}
	if want := filepath.Join(scopedAuthDir, "scoped.json"); savedPath != want {
		t.Fatalf("saved path = %q, want %q", savedPath, want)
	}
	if _, err = os.Stat(savedPath); err != nil {
		t.Fatalf("scoped auth file missing: %v", err)
	}
	if got := store.AuthDir(); got != defaultAuthDir {
		t.Fatalf("AuthDir() = %q, want %q", got, defaultAuthDir)
	}
	if _, err = os.Stat(filepath.Join(defaultAuthDir, "scoped.json")); !os.IsNotExist(err) {
		t.Fatalf("scoped record unexpectedly written to default directory: %v", err)
	}
}

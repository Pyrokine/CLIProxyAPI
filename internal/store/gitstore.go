package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	internalauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	gitindex "github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
	log "github.com/sirupsen/logrus"
)

const (
	// gcInterval defines minimum time between garbage collection runs.
	gcInterval = 5 * time.Minute
	// gcPruneGracePeriod keeps recently orphaned objects available for recovery.
	gcPruneGracePeriod = 24 * time.Hour
)

type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) initialize() {
	m.token = make(chan struct{}, 1)
	m.token <- struct{}{}
}

func (m *contextMutex) Lock() {
	_ = m.lockContext(context.Background())
}

func (m *contextMutex) lockContext(ctx context.Context) error {
	m.once.Do(m.initialize)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	}
}

func (m *contextMutex) Unlock() {
	m.once.Do(m.initialize)
	select {
	case m.token <- struct{}{}:
	default:
		panic("unlock of unlocked context mutex")
	}
}

// GitTokenStore persists token records and auth metadata using git as the backing storage.
type GitTokenStore struct {
	operationMu    contextMutex
	mu             sync.Mutex
	dirLock        sync.RWMutex
	baseDir        string
	repoDir        string
	configDir      string
	remote         string
	username       string
	password       string
	branch         string
	lastGC         time.Time
	beforePush     func()
	beforeRollback func(string)
}

// NewGitTokenStore creates a token store that saves credentials to disk through the
// TokenStorage implementation embedded in the token record.
func NewGitTokenStore(remote, username, password, branch string) *GitTokenStore {
	return &GitTokenStore{
		remote:   remote,
		username: username,
		password: password,
		branch:   normalizeGitStoreBranch(branch),
	}
}

func normalizeGitStoreBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	return strings.Trim(branch, "/")
}

func (s *GitTokenStore) branchRef() plumbing.ReferenceName {
	if s == nil || s.branch == "" {
		return ""
	}
	return plumbing.NewBranchReferenceName(s.branch)
}

// SetBaseDir updates the default directory used for auth JSON persistence when no explicit path is provided.
func (s *GitTokenStore) SetBaseDir(dir string) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.setBaseDir(dir)
}

func (s *GitTokenStore) setBaseDir(dir string) {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		s.dirLock.Lock()
		s.baseDir = ""
		s.repoDir = ""
		s.configDir = ""
		s.dirLock.Unlock()
		return
	}
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	repoDir := filepath.Dir(clean)
	if repoDir == "" || repoDir == "." {
		repoDir = clean
	}
	configDir := filepath.Join(repoDir, "config")
	s.dirLock.Lock()
	s.baseDir = clean
	s.repoDir = repoDir
	s.configDir = configDir
	s.dirLock.Unlock()
}

// AuthDir returns the directory used for auth persistence.
func (s *GitTokenStore) AuthDir() string {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.baseDirSnapshot()
}

// ConfigPath returns the managed config file path.
func (s *GitTokenStore) ConfigPath() string {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.configPathSnapshot()
}

func (s *GitTokenStore) configPathSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	if s.configDir == "" {
		return ""
	}
	return filepath.Join(s.configDir, "config.yaml")
}

// EnsureRepository prepares the local git working tree by cloning or opening the repository.
func (s *GitTokenStore) EnsureRepository() error {
	return s.ensureRepositoryContext(context.Background())
}

// ensureRepositoryContext prepares the local git working tree with cancellation support.
func (s *GitTokenStore) ensureRepositoryContext(ctx context.Context) error {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return err
	}
	defer s.operationMu.Unlock()
	return s.ensureRepository(ctx)
}

func (s *GitTokenStore) ensureRepository(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.dirLock.Lock()
	if s.remote == "" {
		s.dirLock.Unlock()
		return fmt.Errorf("git token store: remote not configured")
	}
	if s.baseDir == "" {
		s.dirLock.Unlock()
		return fmt.Errorf("git token store: base directory not configured")
	}
	repoDir := s.repoDir
	if repoDir == "" {
		repoDir = filepath.Dir(s.baseDir)
		if repoDir == "" || repoDir == "." {
			repoDir = s.baseDir
		}
		s.repoDir = repoDir
	}
	if s.configDir == "" {
		s.configDir = filepath.Join(repoDir, "config")
	}
	authDir := filepath.Join(repoDir, "auths")
	configDir := filepath.Join(repoDir, "config")
	gitDir := filepath.Join(repoDir, ".git")
	clientOptions := s.gitClientOptions()
	var initPaths []string
	_, statErr := os.Stat(gitDir)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		s.dirLock.Unlock()
		return fmt.Errorf("git token store: stat repo: %w", statErr)
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		if errMk := os.MkdirAll(repoDir, 0o700); errMk != nil {
			s.dirLock.Unlock()
			return fmt.Errorf("git token store: create repo dir: %w", errMk)
		}
		branchRef := s.branchRef()
		cloneOpts := &git.CloneOptions{ClientOptions: clientOptions, URL: s.remote}
		if branchRef != "" {
			cloneOpts.ReferenceName = branchRef
			cloneOpts.SingleBranch = true
		}
		repo, errClone := git.PlainCloneContext(ctx, repoDir, cloneOpts)
		if errClone != nil {
			closeGitRepository(repo, "git token store: close failed clone repo")
			if errors.Is(errClone, transport.ErrEmptyRemoteRepository) {
				_ = os.RemoveAll(gitDir)
				repo, errInit := git.PlainInit(repoDir, false)
				if errInit != nil {
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: init empty repo: %w", errInit)
				}
				defer closeGitRepository(repo, "git token store: close initialized repo")
				if branchRef != "" {
					worktree, errWorktree := repo.Worktree()
					if errWorktree != nil {
						s.dirLock.Unlock()
						return fmt.Errorf("git token store: worktree: %w", errWorktree)
					}
					if errCheckout := worktree.Checkout(
						&git.CheckoutOptions{
							Branch: branchRef, Create: true,
						},
					); errCheckout != nil {
						s.dirLock.Unlock()
						return fmt.Errorf("git token store: checkout branch: %w", errCheckout)
					}
				}
				if _, errRemote := repo.Remote("origin"); errRemote != nil {
					if _, errCreate := repo.CreateRemote(
						&config.RemoteConfig{
							Name: "origin",
							URLs: []string{s.remote},
						},
					); errCreate != nil && !errors.Is(errCreate, git.ErrRemoteExists) {
						s.dirLock.Unlock()
						return fmt.Errorf("git token store: configure remote: %w", errCreate)
					}
				}
				if err := secureAuthDirectory(authDir); err != nil {
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: secure auth dir: %w", err)
				}
				if err := os.MkdirAll(configDir, 0o700); err != nil {
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: create config dir: %w", err)
				}
				if err := ensureEmptyFile(filepath.Join(authDir, ".gitkeep")); err != nil {
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: create auth placeholder: %w", err)
				}
				if err := ensureEmptyFile(filepath.Join(configDir, ".gitkeep")); err != nil {
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: create config placeholder: %w", err)
				}
				initPaths = []string{
					filepath.Join("auths", ".gitkeep"),
					filepath.Join("config", ".gitkeep"),
				}
			} else {
				s.dirLock.Unlock()
				return fmt.Errorf("git token store: clone remote: %w", errClone)
			}
		} else {
			defer closeGitRepository(repo, "git token store: close cloned repo")
		}
	} else {
		repo, errOpen := git.PlainOpen(repoDir)
		if errOpen != nil {
			s.dirLock.Unlock()
			return fmt.Errorf("git token store: open repo: %w", errOpen)
		}
		repoOpen := true
		closeOpenedRepo := func() {
			if !repoOpen {
				return
			}
			closeGitRepository(repo, "git token store: close opened repo")
			repoOpen = false
		}
		defer closeOpenedRepo()
		worktree, errWorktree := repo.Worktree()
		if errWorktree != nil {
			s.dirLock.Unlock()
			return fmt.Errorf("git token store: worktree: %w", errWorktree)
		}
		repositoryRecovered := false
		if errVerify := verifyRepositoryHead(repo); errVerify != nil {
			if !isRepositoryCorruptionError(errVerify) {
				s.dirLock.Unlock()
				return fmt.Errorf("git token store: verify repository before pull: %w", errVerify)
			}
			closeOpenedRepo()
			if errRecover := s.recoverRepositoryLocked(
				ctx, repoDir, clientOptions, nil, nil,
			); errRecover != nil {
				s.dirLock.Unlock()
				return errors.Join(
					fmt.Errorf("git token store: verify repository before pull: %w", errVerify),
					fmt.Errorf("git token store: repository recovery: %w", errRecover),
				)
			}
			repositoryRecovered = true
		}
		if !repositoryRecovered {
			branchRef := s.branchRef()
			if branchRef != "" {
				if errCheckout := worktree.Checkout(&git.CheckoutOptions{Branch: branchRef}); errCheckout != nil {
					if errCreate := worktree.Checkout(
						&git.CheckoutOptions{
							Branch: branchRef, Create: true,
						},
					); errCreate != nil {
						s.dirLock.Unlock()
						return fmt.Errorf("git token store: checkout branch: %w", errCreate)
					}
				}
			}
			baselineTree, errBaseline := repositoryHeadTree(repo)
			if errBaseline != nil {
				s.dirLock.Unlock()
				return fmt.Errorf("git token store: inspect head before pull: %w", errBaseline)
			}
			var baselineEntries map[string]recoveryTreeEntry
			if baselineTree != nil {
				baselineEntries, errBaseline = recoveryTreeEntries(baselineTree)
				if errBaseline != nil {
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: inspect head tree before pull: %w", errBaseline)
				}
			}
			dirtyPaths, errDirtyPaths := worktreeDirtyPaths(worktree)
			if errDirtyPaths != nil {
				s.dirLock.Unlock()
				return fmt.Errorf("git token store: inspect worktree before pull: %w", errDirtyPaths)
			}
			pullOpts := &git.PullOptions{ClientOptions: clientOptions, RemoteName: "origin"}
			if branchRef != "" {
				pullOpts.ReferenceName = branchRef
				pullOpts.SingleBranch = true
			}
			if errPull := worktree.PullContext(ctx, pullOpts); errPull != nil {
				switch {
				case errors.Is(errPull, git.NoErrAlreadyUpToDate),
					errors.Is(errPull, git.ErrUnstagedChanges):
					// Ignore clean syncs and local edits. The push lease still checks the advertised remote ref.
				case errors.Is(errPull, git.ErrNonFastForwardUpdate):
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: remote branch diverged: %w", errPull)
				case errors.Is(errPull, transport.ErrAuthenticationRequired),
					errors.Is(errPull, plumbing.ErrReferenceNotFound),
					errors.Is(errPull, transport.ErrEmptyRemoteRepository):
					// Ignore authentication prompts and empty remote references on initial sync.
				case isRepositoryCorruptionError(errPull):
					closeOpenedRepo()
					if errRecover := s.recoverRepositoryLocked(
						ctx, repoDir, clientOptions, baselineEntries, dirtyPaths,
					); errRecover != nil {
						s.dirLock.Unlock()
						return errors.Join(
							fmt.Errorf("git token store: pull: %w", errPull),
							fmt.Errorf("git token store: repository recovery: %w", errRecover),
						)
					}
					repositoryRecovered = true
				default:
					s.dirLock.Unlock()
					return fmt.Errorf("git token store: pull: %w", errPull)
				}
			}
			if !repositoryRecovered {
				if errVerify := verifyRepositoryHead(repo); errVerify != nil {
					if !isRepositoryCorruptionError(errVerify) {
						s.dirLock.Unlock()
						return fmt.Errorf("git token store: verify repository after pull: %w", errVerify)
					}
					closeOpenedRepo()
					if errRecover := s.recoverRepositoryLocked(
						ctx, repoDir, clientOptions, baselineEntries, dirtyPaths,
					); errRecover != nil {
						s.dirLock.Unlock()
						return errors.Join(
							fmt.Errorf("git token store: verify repository after pull: %w", errVerify),
							fmt.Errorf("git token store: repository recovery: %w", errRecover),
						)
					}
				}
			}
		}
	}
	if err := disableGitCommitSigning(repoDir); err != nil {
		s.dirLock.Unlock()
		return err
	}
	if err := secureAuthTree(s.baseDir); err != nil {
		s.dirLock.Unlock()
		return fmt.Errorf("git token store: secure auth tree: %w", err)
	}
	if err := os.MkdirAll(s.configDir, 0o700); err != nil {
		s.dirLock.Unlock()
		return fmt.Errorf("git token store: create config dir: %w", err)
	}
	s.dirLock.Unlock()
	if len(initPaths) > 0 {
		s.mu.Lock()
		err := s.commitAndPushLocked(ctx, "Initialize git token store", initPaths...)
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	errRecover := s.recoverAuthDeletionsLocked(ctx)
	s.mu.Unlock()
	return errRecover
}

// Save persists token storage and metadata to the resolved auth file path.
func (s *GitTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return "", err
	}
	defer s.operationMu.Unlock()
	return s.save(ctx, auth)
}

// SaveWithBaseDir persists an auth record using an operation-scoped base directory.
func (s *GitTokenStore) SaveWithBaseDir(
	ctx context.Context,
	baseDir string,
	auth *cliproxyauth.Auth,
) (string, error) {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return "", err
	}
	defer s.operationMu.Unlock()

	s.dirLock.RLock()
	previousBaseDir := s.baseDir
	previousRepoDir := s.repoDir
	previousConfigDir := s.configDir
	s.dirLock.RUnlock()

	s.setBaseDir(baseDir)
	defer func() {
		s.dirLock.Lock()
		s.baseDir = previousBaseDir
		s.repoDir = previousRepoDir
		s.configDir = previousConfigDir
		s.dirLock.Unlock()
	}()

	return s.save(ctx, auth)
}

func (s *GitTokenStore) save(ctx context.Context, auth *cliproxyauth.Auth) (savedPath string, err error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("auth filestore: missing file path attribute for %s", auth.ID)
	}

	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return "", err
		}
	}

	if err = s.ensureRepository(ctx); err != nil {
		return "", err
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return "", err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return "", err
		}
	}

	if err = secureAuthDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("auth filestore: secure dir failed: %w", err)
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return "", err
		}
	}
	fileSnapshot, errSnapshot := captureFileMutationSnapshot(path)
	if errSnapshot != nil {
		return "", errSnapshot
	}
	previousPath, hadPreviousPath := "", false
	if auth.Attributes != nil {
		previousPath, hadPreviousPath = auth.Attributes["path"]
	}
	previousFileName := auth.FileName
	fileMutated := false
	mutationComplete := false
	rollbackMutation := func() error {
		if auth.Attributes != nil {
			if hadPreviousPath {
				auth.Attributes["path"] = previousPath
			} else {
				delete(auth.Attributes, "path")
			}
		}
		auth.FileName = previousFileName
		if !fileMutated {
			return nil
		}
		fileMutated = false
		if s.beforeRollback != nil {
			s.beforeRollback(path)
		}
		return restoreFileMutationSnapshot(path, fileSnapshot)
	}
	defer func() {
		if mutationComplete {
			return
		}
		if errRollback := rollbackMutation(); errRollback != nil {
			err = errors.Join(
				cliproxyauth.ErrAuthDurableStateUncertain,
				err,
				errRollback,
			)
			savedPath = ""
		}
	}()

	cliproxyauth.PrepareAuthForPersistence(auth)
	switch {
	case auth.Storage != nil:
		fileMutated = true
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
		if err = secureAuthFilePath(path); err != nil {
			return "", fmt.Errorf("auth filestore: secure stored token failed: %w", err)
		}
	case auth.Metadata != nil:
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("auth filestore: marshal metadata failed: %w", errMarshal)
		}
		if ctx != nil {
			if err = ctx.Err(); err != nil {
				return "", err
			}
		}
		existing, errRead := os.ReadFile(path)
		if ctx != nil {
			if err = ctx.Err(); err != nil {
				return "", err
			}
		}
		if errRead == nil && util.JSONEqual(existing, raw) {
			if err = secureAuthFilePath(path); err != nil {
				return "", fmt.Errorf("auth filestore: secure existing metadata failed: %w", err)
			}
			break
		}
		if errRead != nil && !os.IsNotExist(errRead) {
			return "", fmt.Errorf("auth filestore: read existing failed: %w", errRead)
		}
		fileMutated = true
		if errWrite := writeAuthFile(path, raw); errWrite != nil {
			return "", fmt.Errorf("auth filestore: write metadata failed: %w", errWrite)
		}
	default:
		return "", fmt.Errorf("auth filestore: nothing to persist for %s", auth.ID)
	}
	if ctx != nil {
		if err = ctx.Err(); err != nil {
			return "", err
		}
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	relPath, errRel := s.relativeToRepo(path)
	if errRel != nil {
		return "", errRel
	}
	messageID := auth.ID
	if strings.TrimSpace(messageID) == "" {
		messageID = filepath.Base(path)
	}
	if errCommit := s.commitAndPushWithRollbackLocked(
		ctx,
		fmt.Sprintf("Update auth %s", strings.TrimSpace(messageID)),
		rollbackMutation,
		relPath,
	); errCommit != nil {
		return "", errCommit
	}
	mutationComplete = true
	return path, nil
}

// List enumerates all auth JSON files under the configured directory.
func (s *GitTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return nil, err
	}
	defer s.operationMu.Unlock()
	if err := s.ensureRepository(ctx); err != nil {
		return nil, err
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return nil, fmt.Errorf("auth filestore: directory not configured")
	}
	entries := make([]*cliproxyauth.Auth, 0)
	err := filepath.WalkDir(
		dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
				return nil
			}
			auth, err := s.readAuthFile(ctx, path, dir)
			if err != nil {
				log.WithError(err).Warnf("git store: skip auth %s", path)
				return nil
			}
			if auth != nil {
				entries = append(entries, auth)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// DeleteIfVersion removes and commits only the exact persisted auth file version.
func (s *GitTokenStore) DeleteIfVersion(
	ctx context.Context,
	id string,
	version cliproxyauth.PersistedVersion,
) (bool, error) {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return false, err
	}
	defer s.operationMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return false, err
	}
	if err = s.ensureRepository(ctx); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deletion, matched, errPrepare := cliproxyauth.PrepareDurablePersistedFileDeletion(path, version)
	if errPrepare != nil || !matched {
		return false, errPrepare
	}
	rel, errRel := s.relativeToRepo(path)
	if errRel != nil {
		return false, errors.Join(errRel, deletion.Rollback())
	}
	if errCommit := s.commitAndPushWithRollbackLocked(
		ctx,
		fmt.Sprintf("Delete auth %s", id),
		deletion.Rollback,
		rel,
	); errCommit != nil {
		return false, errCommit
	}
	if errCommitDeletion := deletion.Commit(); errCommitDeletion != nil {
		return false, errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, errCommitDeletion)
	}
	return true, nil
}

func (s *GitTokenStore) recoverAuthDeletionsLocked(ctx context.Context) error {
	deletions, errRecover := cliproxyauth.RecoverDurablePersistedFileDeletions(s.baseDirSnapshot())
	if errRecover != nil {
		return errRecover
	}
	for _, deletion := range deletions {
		rel, errRel := s.relativeToRepo(deletion.Path())
		if errRel != nil {
			return errRel
		}
		if errCommit := s.commitAndPushWithRollbackLocked(
			ctx,
			fmt.Sprintf("Resume auth deletion %s", filepath.Base(deletion.Path())),
			nil,
			rel,
		); errCommit != nil {
			return fmt.Errorf("git token store: resume auth deletion: %w", errCommit)
		}
		if errCommitDeletion := deletion.Commit(); errCommitDeletion != nil {
			return errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, errCommitDeletion)
		}
	}
	return nil
}

// Delete removes the auth file.
func (s *GitTokenStore) Delete(ctx context.Context, id string) error {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return err
	}
	defer s.operationMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}
	if err = s.ensureRepository(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fileSnapshot, errSnapshot := captureFileMutationSnapshot(path)
	if errSnapshot != nil {
		return errSnapshot
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth filestore: delete failed: %w", err)
	}
	rollback := func() error { return restoreFileMutationSnapshot(path, fileSnapshot) }
	rel, errRel := s.relativeToRepo(path)
	if errRel != nil {
		return errors.Join(errRel, rollback())
	}
	if errCommit := s.commitAndPushWithRollbackLocked(
		ctx, fmt.Sprintf("Delete auth %s", id), rollback, rel,
	); errCommit != nil {
		return errCommit
	}
	return nil
}

// PersistAuthFiles commits and pushes the provided paths to the remote repository.
// It no-ops when the store is not fully configured or when there are no paths.
func (s *GitTokenStore) PersistAuthFiles(ctx context.Context, message string, paths ...string) error {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return err
	}
	defer s.operationMu.Unlock()
	if len(paths) == 0 {
		return nil
	}
	if err := s.ensureRepository(ctx); err != nil {
		return err
	}

	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		rel, err := s.relativeToRepo(trimmed)
		if err != nil {
			return err
		}
		filtered = append(filtered, rel)
	}
	if len(filtered) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(message) == "" {
		message = "Sync watcher updates"
	}
	return s.commitAndPushLocked(ctx, message, filtered...)
}

func (s *GitTokenStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, id), nil
}

func (s *GitTokenStore) readAuthFile(ctx context.Context, path, baseDir string) (*cliproxyauth.Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	provider, _ := metadata["type"].(string)
	provider = strings.TrimSpace(provider)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	id := s.idFor(path, baseDir)
	pluginAuth, handled, errParse := sdkauth.ParsePluginAuthFile(
		ctx, provider, path, id, data,
	)
	if errParse != nil {
		return nil, errParse
	}
	if handled {
		pluginAuth.CreatedAt = info.ModTime()
		pluginAuth.UpdatedAt = info.ModTime()
		return pluginAuth, nil
	}
	if provider == "" {
		provider = "unknown"
	}
	disabled, _ := metadata["disabled"].(bool)
	status := cliproxyauth.StatusActive
	if disabled {
		status = cliproxyauth.StatusDisabled
	}
	auth := &cliproxyauth.Auth{
		ID:               id,
		Provider:         provider,
		FileName:         id,
		Label:            labelFor(metadata),
		Status:           status,
		Disabled:         disabled,
		Attributes:       map[string]string{"path": path},
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	if err := cliproxyauth.ApplyCustomHeadersFromMetadata(auth); err != nil {
		return nil, err
	}
	if email, ok := metadata["email"].(string); ok && email != "" {
		auth.Attributes["email"] = email
	}
	return auth, nil
}

func (s *GitTokenStore) idFor(path, baseDir string) string {
	return internalauth.FileAuthID(path, baseDir)
}

func (s *GitTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		if dir := s.baseDirSnapshot(); dir != "" {
			return filepath.Join(dir, fileName), nil
		}
		return fileName, nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("auth filestore: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, auth.ID), nil
}

func (s *GitTokenStore) baseDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.baseDir
}

func (s *GitTokenStore) repoDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.repoDir
}

func disableGitCommitSigning(repoDir string) error {
	repo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		return fmt.Errorf("git token store: open repository config: %w", errOpen)
	}
	defer closeGitRepository(repo, "git token store: close repository config")

	cfg, errConfig := repo.Config()
	if errConfig != nil {
		return fmt.Errorf("git token store: get repository config: %w", errConfig)
	}
	cfg.Commit.GpgSign = config.OptBoolFalse
	if errSetConfig := repo.SetConfig(cfg); errSetConfig != nil {
		return fmt.Errorf("git token store: disable commit signing: %w", errSetConfig)
	}
	return nil
}

func (s *GitTokenStore) gitClientOptions() []client.Option {
	if s.username == "" && s.password == "" {
		return nil
	}
	user := s.username
	if user == "" {
		user = "git"
	}
	return []client.Option{client.WithHTTPAuth(&http.BasicAuth{Username: user, Password: s.password})}
}

func (s *GitTokenStore) relativeToRepo(path string) (string, error) {
	repoDir := s.repoDirSnapshot()
	if repoDir == "" {
		return "", fmt.Errorf("git token store: repository path not configured")
	}
	absRepo := repoDir
	if abs, err := filepath.Abs(repoDir); err == nil {
		absRepo = abs
	}
	cleanPath := path
	if abs, err := filepath.Abs(path); err == nil {
		cleanPath = abs
	}
	rel, err := filepath.Rel(absRepo, cleanPath)
	if err != nil {
		return "", fmt.Errorf("git token store: relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("git token store: path outside repository")
	}
	return normalizeGitPath(rel), nil
}

func normalizeGitPath(path string) string {
	return strings.ReplaceAll(filepath.ToSlash(path), "\\", "/")
}

func repositoryHeadTree(repo *git.Repository) (*object.Tree, error) {
	if repo == nil {
		return nil, fmt.Errorf("repository is nil")
	}
	head, errHead := repo.Head()
	if errors.Is(errHead, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if errHead != nil {
		return nil, errHead
	}
	commit, errCommit := repo.CommitObject(head.Hash())
	if errCommit != nil {
		return nil, errCommit
	}
	return commit.Tree()
}

func worktreeDirtyPaths(worktree *git.Worktree) (map[string]struct{}, error) {
	if worktree == nil {
		return nil, fmt.Errorf("worktree is nil")
	}
	status, errStatus := worktree.Status()
	if errStatus != nil {
		return nil, errStatus
	}
	dirtyPaths := make(map[string]struct{}, len(status))
	for path, fileStatus := range status {
		if fileStatus.Staging == git.Unmodified && fileStatus.Worktree == git.Unmodified {
			continue
		}
		cleanPath, errPath := normalizeRecoveryPath(path)
		if errPath != nil {
			return nil, errPath
		}
		dirtyPaths[cleanPath] = struct{}{}
	}
	return dirtyPaths, nil
}

func normalizeRecoveryPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	clean := filepath.ToSlash(filepath.Clean(trimmed))
	if trimmed == "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("path %q is not repository-relative", path)
	}
	return clean, nil
}

type recoveryTreeEntry struct {
	hash plumbing.Hash
	mode string
}

func recoveryTreeEntries(tree *object.Tree) (map[string]recoveryTreeEntry, error) {
	if tree == nil {
		return nil, fmt.Errorf("tree is nil")
	}
	entries := make(map[string]recoveryTreeEntry)
	var visitTree func(*object.Tree, string) error
	visitTree = func(current *object.Tree, prefix string) error {
		for _, entry := range current.Entries {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode.IsFile() || entry.Mode == filemode.Submodule {
				cleanPath, errPath := normalizeRecoveryPath(path)
				if errPath != nil {
					return errPath
				}
				entries[cleanPath] = recoveryTreeEntry{hash: entry.Hash, mode: entry.Mode.String()}
				continue
			}
			subtree, errSubtree := current.Tree(entry.Name)
			if errSubtree != nil {
				return fmt.Errorf("inspect tree %s: %w", path, errSubtree)
			}
			if errVisit := visitTree(subtree, path); errVisit != nil {
				return errVisit
			}
		}
		return nil
	}
	if errVisit := visitTree(tree, ""); errVisit != nil {
		return nil, errVisit
	}
	return entries, nil
}

func overlappingDirtyPath(path string, dirtyPaths map[string]struct{}) (string, bool) {
	for dirtyPath := range dirtyPaths {
		if path == dirtyPath || strings.HasPrefix(path, dirtyPath+"/") ||
			strings.HasPrefix(dirtyPath, path+"/") {
			return dirtyPath, true
		}
	}
	return "", false
}

func changedRecoveryTreePaths(
	baselineEntries,
	remoteEntries map[string]recoveryTreeEntry,
) []string {
	paths := make(map[string]struct{})
	for path, baselineEntry := range baselineEntries {
		remoteEntry, exists := remoteEntries[path]
		if !exists || remoteEntry != baselineEntry {
			paths[path] = struct{}{}
		}
	}
	for path, remoteEntry := range remoteEntries {
		baselineEntry, exists := baselineEntries[path]
		if !exists || baselineEntry != remoteEntry {
			paths[path] = struct{}{}
		}
	}
	changedPaths := make([]string, 0, len(paths))
	for path := range paths {
		changedPaths = append(changedPaths, path)
	}
	sort.Strings(changedPaths)
	return changedPaths
}

func recoveryPreservedPaths(
	baselineEntries,
	remoteEntries map[string]recoveryTreeEntry,
	dirtyPaths map[string]struct{},
) (map[string]struct{}, error) {
	if len(dirtyPaths) == 0 {
		return nil, nil
	}
	if baselineEntries == nil {
		return nil, fmt.Errorf("baseline tree is unavailable for dirty worktree recovery")
	}
	changedPaths := changedRecoveryTreePaths(baselineEntries, remoteEntries)
	for _, changedPath := range changedPaths {
		if dirtyPath, conflict := overlappingDirtyPath(changedPath, dirtyPaths); conflict {
			return nil, fmt.Errorf(
				"remote path %s conflicts with local change %s during repository recovery",
				changedPath,
				dirtyPath,
			)
		}
	}
	return dirtyPaths, nil
}

func applyRecoveryLocalChanges(sourceDir, targetDir string, paths map[string]struct{}) error {
	sortedPaths := make([]string, 0, len(paths))
	for path := range paths {
		sortedPaths = append(sortedPaths, path)
	}
	sort.Strings(sortedPaths)
	for _, path := range sortedPaths {
		cleanPath, errPath := normalizeRecoveryPath(path)
		if errPath != nil {
			return errPath
		}
		source := filepath.Join(sourceDir, filepath.FromSlash(cleanPath))
		target := filepath.Join(targetDir, filepath.FromSlash(cleanPath))
		info, errStat := os.Lstat(source)
		if errors.Is(errStat, fs.ErrNotExist) {
			if errRemove := os.RemoveAll(target); errRemove != nil {
				return fmt.Errorf("preserve deletion %s: %w", cleanPath, errRemove)
			}
			continue
		}
		if errStat != nil {
			return fmt.Errorf("inspect local change %s: %w", cleanPath, errStat)
		}
		if errRemove := os.RemoveAll(target); errRemove != nil {
			return fmt.Errorf("replace recovered path %s: %w", cleanPath, errRemove)
		}
		if errMkdir := os.MkdirAll(filepath.Dir(target), 0o700); errMkdir != nil {
			return fmt.Errorf("create recovered parent for %s: %w", cleanPath, errMkdir)
		}
		switch {
		case info.Mode().IsRegular():
			contents, errRead := os.ReadFile(source)
			if errRead != nil {
				return fmt.Errorf("read local change %s: %w", cleanPath, errRead)
			}
			if errWrite := os.WriteFile(target, contents, info.Mode().Perm()); errWrite != nil {
				return fmt.Errorf("write local change %s: %w", cleanPath, errWrite)
			}
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, errReadlink := os.Readlink(source)
			if errReadlink != nil {
				return fmt.Errorf("read local symlink %s: %w", cleanPath, errReadlink)
			}
			if errSymlink := os.Symlink(linkTarget, target); errSymlink != nil {
				return fmt.Errorf("write local symlink %s: %w", cleanPath, errSymlink)
			}
		default:
			return fmt.Errorf("local change %s has unsupported file mode %s", cleanPath, info.Mode())
		}
	}
	return nil
}

func moveWorktreeEntries(sourceDir, targetDir string) error {
	if errMkdir := os.MkdirAll(targetDir, 0o700); errMkdir != nil {
		return errMkdir
	}
	entries, errRead := os.ReadDir(sourceDir)
	if errRead != nil {
		return errRead
	}
	moved := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		source := filepath.Join(sourceDir, entry.Name())
		target := filepath.Join(targetDir, entry.Name())
		if errRename := os.Rename(source, target); errRename != nil {
			errMove := fmt.Errorf("move %s: %w", entry.Name(), errRename)
			for _, name := range slices.Backward(moved) {

				if errRestore := os.Rename(
					filepath.Join(targetDir, name),
					filepath.Join(sourceDir, name),
				); errRestore != nil {
					errMove = errors.Join(errMove, fmt.Errorf("restore %s: %w", name, errRestore))
				}
			}
			return errMove
		}
		moved = append(moved, entry.Name())
	}
	return nil
}

func removeWorktreeEntries(repoDir string) error {
	entries, errRead := os.ReadDir(repoDir)
	if errRead != nil {
		return errRead
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if errRemove := os.RemoveAll(filepath.Join(repoDir, entry.Name())); errRemove != nil {
			return errRemove
		}
	}
	return nil
}

func installRecoveredGitDirectory(
	gitDir,
	clonedGitDir,
	backupGitDir string,
	rename func(string, string) error,
) (bool, error) {
	if errRename := rename(gitDir, backupGitDir); errRename != nil {
		return false, fmt.Errorf("backup corrupt git directory: %w", errRename)
	}
	if errRename := rename(clonedGitDir, gitDir); errRename != nil {
		if errRestore := rename(backupGitDir, gitDir); errRestore != nil {
			return true, errors.Join(
				fmt.Errorf("install recovered git directory: %w", errRename),
				fmt.Errorf(
					"restore corrupt git directory; backup retained at %s: %w",
					backupGitDir,
					errRestore,
				),
			)
		}
		return false, fmt.Errorf("install recovered git directory: %w", errRename)
	}
	return false, nil
}

func rollbackRecoveredGitDirectory(gitDir, backupGitDir string) error {
	if errRemove := os.RemoveAll(gitDir); errRemove != nil {
		return fmt.Errorf("remove recovered git directory: %w", errRemove)
	}
	if errRename := os.Rename(backupGitDir, gitDir); errRename != nil {
		return fmt.Errorf("restore original git directory: %w", errRename)
	}
	return nil
}

func rollbackRecoveredRepository(repoDir, gitDir, backupGitDir, backupWorktreeDir string) error {
	if errRemove := removeWorktreeEntries(repoDir); errRemove != nil {
		return fmt.Errorf("remove recovered worktree: %w", errRemove)
	}
	if errRollback := rollbackRecoveredGitDirectory(gitDir, backupGitDir); errRollback != nil {
		return errRollback
	}
	if errRestore := moveWorktreeEntries(backupWorktreeDir, repoDir); errRestore != nil {
		return fmt.Errorf("restore original worktree: %w", errRestore)
	}
	return nil
}

func inspectRecoveryBaseline(repoDir string) (map[string]recoveryTreeEntry, map[string]struct{}, error) {
	repo, errOpen := git.PlainOpen(repoDir)
	if errOpen != nil {
		return nil, nil, fmt.Errorf("open repository: %w", errOpen)
	}
	defer closeGitRepository(repo, "git token store: close recovery baseline repo")
	worktree, errWorktree := repo.Worktree()
	if errWorktree != nil {
		return nil, nil, fmt.Errorf("open worktree: %w", errWorktree)
	}
	dirtyPaths, errDirty := worktreeDirtyPaths(worktree)
	if errDirty != nil {
		return nil, nil, fmt.Errorf("inspect worktree changes: %w", errDirty)
	}
	tree, errTree := repositoryHeadTree(repo)
	if errTree != nil {
		return nil, nil, fmt.Errorf("inspect head tree: %w", errTree)
	}
	if tree == nil {
		return nil, nil, fmt.Errorf("repository has no recovery baseline")
	}
	entries, errEntries := recoveryTreeEntries(tree)
	if errEntries != nil {
		return nil, nil, fmt.Errorf("inspect head tree entries: %w", errEntries)
	}
	return entries, dirtyPaths, nil
}

func restoreBackedUpWorktree(repoDir, backupWorktreeDir string, cause error) error {
	if errRestore := moveWorktreeEntries(backupWorktreeDir, repoDir); errRestore != nil {
		return errors.Join(
			cause,
			fmt.Errorf(
				"restore worktree; backup retained at %s: %w",
				backupWorktreeDir,
				errRestore,
			),
		)
	}
	return cause
}

func (s *GitTokenStore) recoverRepositoryLocked(
	ctx context.Context,
	repoDir string,
	clientOptions []client.Option,
	baselineEntries map[string]recoveryTreeEntry,
	dirtyPaths map[string]struct{},
) (errRecovery error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	parentDir := filepath.Dir(repoDir)
	recoveryRoot, errTemp := os.MkdirTemp(parentDir, ".gitstore-recovery-")
	if errTemp != nil {
		return fmt.Errorf("create recovery directory: %w", errTemp)
	}
	cleanupRecovery := true
	defer func() {
		if !cleanupRecovery {
			return
		}
		if errRemove := os.RemoveAll(recoveryRoot); errRemove != nil {
			errCleanup := fmt.Errorf("remove recovery directory: %w", errRemove)
			if errRecovery == nil {
				errRecovery = errCleanup
			} else {
				errRecovery = errors.Join(errRecovery, errCleanup)
			}
		}
	}()

	if baselineEntries == nil {
		inspectedEntries, inspectedDirtyPaths, errInspect := inspectRecoveryBaseline(repoDir)
		if errInspect != nil {
			return fmt.Errorf("inspect recovery baseline: %w", errInspect)
		}
		baselineEntries = inspectedEntries
		dirtyPaths = inspectedDirtyPaths
	}
	cloneDir := filepath.Join(recoveryRoot, "clone")
	cloneOpts := &git.CloneOptions{ClientOptions: clientOptions, URL: s.remote}
	if branchRef := s.branchRef(); branchRef != "" {
		cloneOpts.ReferenceName = branchRef
		cloneOpts.SingleBranch = true
	}
	clonedRepo, errClone := git.PlainCloneContext(ctx, cloneDir, cloneOpts)
	if errClone != nil {
		closeGitRepository(clonedRepo, "git token store: close failed recovery clone")
		return fmt.Errorf("clone remote repository: %w", errClone)
	}
	clonedRepoOpen := true
	closeClonedRepo := func() {
		if !clonedRepoOpen {
			return
		}
		closeGitRepository(clonedRepo, "git token store: close recovery clone")
		clonedRepoOpen = false
	}
	defer closeClonedRepo()
	if errVerify := verifyRepositoryHead(clonedRepo); errVerify != nil {
		return fmt.Errorf("verify cloned repository: %w", errVerify)
	}
	remoteTree, errTree := repositoryHeadTree(clonedRepo)
	if errTree != nil {
		return fmt.Errorf("inspect cloned repository tree: %w", errTree)
	}
	if remoteTree == nil {
		return fmt.Errorf("cloned repository has no head tree")
	}
	remoteEntries, errEntries := recoveryTreeEntries(remoteTree)
	if errEntries != nil {
		return fmt.Errorf("inspect cloned repository tree entries: %w", errEntries)
	}
	preservedPaths, errPreserve := recoveryPreservedPaths(
		baselineEntries, remoteEntries, dirtyPaths,
	)
	if errPreserve != nil {
		return errPreserve
	}
	if errApply := applyRecoveryLocalChanges(repoDir, cloneDir, preservedPaths); errApply != nil {
		return fmt.Errorf("preserve local worktree changes: %w", errApply)
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	closeClonedRepo()

	backupWorktreeDir := filepath.Join(recoveryRoot, "worktree")
	if errBackup := moveWorktreeEntries(repoDir, backupWorktreeDir); errBackup != nil {
		return fmt.Errorf("backup existing worktree: %w", errBackup)
	}
	if errContext := ctx.Err(); errContext != nil {
		return restoreBackedUpWorktree(repoDir, backupWorktreeDir, errContext)
	}
	gitDir := filepath.Join(repoDir, ".git")
	clonedGitDir := filepath.Join(cloneDir, ".git")
	backupGitDir := filepath.Join(recoveryRoot, "corrupt.git")
	retainRecovery, errInstall := installRecoveredGitDirectory(
		gitDir, clonedGitDir, backupGitDir, os.Rename,
	)
	if retainRecovery {
		cleanupRecovery = false
	}
	if errInstall != nil {
		return restoreBackedUpWorktree(repoDir, backupWorktreeDir, errInstall)
	}
	if errContext := ctx.Err(); errContext != nil {
		if errRollback := rollbackRecoveredRepository(
			repoDir, gitDir, backupGitDir, backupWorktreeDir,
		); errRollback != nil {
			cleanupRecovery = false
			return errors.Join(
				errContext,
				fmt.Errorf("rollback recovered repository; backup retained at %s: %w", recoveryRoot, errRollback),
			)
		}
		return errContext
	}
	if errMove := moveWorktreeEntries(cloneDir, repoDir); errMove != nil {
		errMoveWorktree := fmt.Errorf("install recovered worktree: %w", errMove)
		if errRollback := rollbackRecoveredRepository(
			repoDir, gitDir, backupGitDir, backupWorktreeDir,
		); errRollback != nil {
			cleanupRecovery = false
			return errors.Join(
				errMoveWorktree,
				fmt.Errorf("rollback recovered repository; backup retained at %s: %w", recoveryRoot, errRollback),
			)
		}
		return errMoveWorktree
	}
	if errContext := ctx.Err(); errContext != nil {
		if errRollback := rollbackRecoveredRepository(
			repoDir, gitDir, backupGitDir, backupWorktreeDir,
		); errRollback != nil {
			cleanupRecovery = false
			return errors.Join(
				errContext,
				fmt.Errorf("rollback recovered repository; backup retained at %s: %w", recoveryRoot, errRollback),
			)
		}
		return errContext
	}
	recoveredRepo, errOpen := git.PlainOpen(repoDir)
	if errOpen == nil {
		errOpen = verifyRepositoryHead(recoveredRepo)
	}
	closeGitRepository(recoveredRepo, "git token store: close recovered repo")
	if errOpen != nil {
		errRecovered := fmt.Errorf("verify recovered repository: %w", errOpen)
		if errRollback := rollbackRecoveredRepository(
			repoDir, gitDir, backupGitDir, backupWorktreeDir,
		); errRollback != nil {
			cleanupRecovery = false
			return errors.Join(
				errRecovered,
				fmt.Errorf("rollback recovered repository; backup retained at %s: %w", recoveryRoot, errRollback),
			)
		}
		return errRecovered
	}
	return nil
}

func isRepositoryCorruptionError(err error) bool {
	return errors.Is(err, dotgit.ErrPackfileNotFound) || errors.Is(err, plumbing.ErrObjectNotFound)
}

func verifyRepositoryHead(repo *git.Repository) error {
	if repo == nil {
		return fmt.Errorf("repository is nil")
	}
	tree, errTree := repositoryHeadTree(repo)
	if errTree != nil || tree == nil {
		return errTree
	}
	files := tree.Files()
	return files.ForEach(
		func(file *object.File) error {
			_, errContents := file.Contents()
			return errContents
		},
	)
}

type fileMutationSnapshot struct {
	exists bool
	data   []byte
	mode   fs.FileMode
}

func captureFileMutationSnapshot(path string) (fileMutationSnapshot, error) {
	info, errStat := os.Stat(path)
	if errors.Is(errStat, fs.ErrNotExist) {
		return fileMutationSnapshot{}, nil
	}
	if errStat != nil {
		return fileMutationSnapshot{}, fmt.Errorf("git token store: stat mutation target: %w", errStat)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return fileMutationSnapshot{}, fmt.Errorf("git token store: read mutation target: %w", errRead)
	}
	return fileMutationSnapshot{exists: true, data: data, mode: info.Mode().Perm()}, nil
}

func restoreFileMutationSnapshot(path string, snapshot fileMutationSnapshot) error {
	if !snapshot.exists {
		if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
			return fmt.Errorf("git token store: remove rolled back target: %w", errRemove)
		}
		return nil
	}
	if errWrite := writeAuthFile(path, snapshot.data); errWrite != nil {
		return fmt.Errorf("git token store: restore mutation target: %w", errWrite)
	}
	return nil
}

type gitHeadSnapshot struct {
	exists bool
	name   plumbing.ReferenceName
	hash   plumbing.Hash
}

func captureGitHeadSnapshot(repo *git.Repository) (gitHeadSnapshot, error) {
	head, errHead := repo.Head()
	if errHead == nil {
		return gitHeadSnapshot{exists: true, name: head.Name(), hash: head.Hash()}, nil
	}
	if !errors.Is(errHead, plumbing.ErrReferenceNotFound) {
		return gitHeadSnapshot{}, fmt.Errorf("git token store: capture head: %w", errHead)
	}
	headRef, errReference := repo.Reference(plumbing.HEAD, false)
	if errReference != nil {
		return gitHeadSnapshot{}, fmt.Errorf("git token store: capture symbolic head: %w", errReference)
	}
	return gitHeadSnapshot{name: headRef.Target()}, nil
}

func captureGitIndexEntries(repo *git.Repository, paths []string) (map[string][]gitindex.Entry, error) {
	index, errIndex := repo.Storer.Index()
	if errIndex != nil {
		return nil, fmt.Errorf("git token store: capture index: %w", errIndex)
	}
	wanted := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		wanted[path] = struct{}{}
	}
	entries := make(map[string][]gitindex.Entry, len(paths))
	for _, entry := range index.Entries {
		if entry == nil {
			continue
		}
		if _, ok := wanted[entry.Name]; !ok {
			continue
		}
		entries[entry.Name] = append(entries[entry.Name], *entry)
	}
	return entries, nil
}

func restoreGitMutationState(
	repo *git.Repository,
	head gitHeadSnapshot,
	entries map[string][]gitindex.Entry,
	paths []string,
) error {
	var errRestore error
	if head.name != "" {
		if head.exists {
			errRestore = errors.Join(
				errRestore,
				repo.Storer.SetReference(plumbing.NewHashReference(head.name, head.hash)),
			)
		} else if errRemove := repo.Storer.RemoveReference(head.name); errRemove != nil &&
			!errors.Is(errRemove, plumbing.ErrReferenceNotFound) {
			errRestore = errors.Join(errRestore, errRemove)
		}
	}
	index, errIndex := repo.Storer.Index()
	if errIndex != nil {
		return errors.Join(errRestore, fmt.Errorf("git token store: reopen index for rollback: %w", errIndex))
	}
	pathSet := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		pathSet[path] = struct{}{}
	}
	retained := index.Entries[:0]
	for _, entry := range index.Entries {
		if entry == nil {
			continue
		}
		if _, replace := pathSet[entry.Name]; replace {
			continue
		}
		retained = append(retained, entry)
	}
	index.Entries = retained
	index.Cache = nil
	for _, path := range paths {
		for _, entry := range entries[path] {
			entryCopy := entry
			index.Entries = append(index.Entries, &entryCopy)
		}
	}
	if errSet := repo.Storer.SetIndex(index); errSet != nil {
		errRestore = errors.Join(errRestore, fmt.Errorf("git token store: restore index: %w", errSet))
	}
	return errRestore
}

func closeGitRepository(repo *git.Repository, operation string) {
	if repo == nil {
		return
	}
	if err := repo.Close(); err != nil {
		log.Warnf("%s: %v", operation, err)
	}
}

func (s *GitTokenStore) commitAndPushLocked(ctx context.Context, message string, relPaths ...string) error {
	return s.commitAndPushWithRollbackLocked(ctx, message, nil, relPaths...)
}

func (s *GitTokenStore) commitAndPushWithRollbackLocked(
	ctx context.Context,
	message string,
	rollback func() error,
	relPaths ...string,
) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalizedPaths := make([]string, 0, len(relPaths))
	for _, path := range relPaths {
		path = normalizeGitPath(path)
		if path != "" {
			normalizedPaths = append(normalizedPaths, path)
		}
	}
	relPaths = normalizedPaths
	var repo *git.Repository
	defer func() {
		closeGitRepository(repo, "git token store: close commit repo")
	}()
	var headSnapshot gitHeadSnapshot
	var indexEntries map[string][]gitindex.Entry
	stateCaptured := false
	pushAttempted := false
	if rollback != nil {
		defer func() {
			if err == nil {
				return
			}
			errRollback := rollback()
			var errState error
			if stateCaptured && repo != nil {
				errState = restoreGitMutationState(repo, headSnapshot, indexEntries, relPaths)
			}
			if pushAttempted || errRollback != nil || errState != nil {
				err = errors.Join(cliproxyauth.ErrAuthDurableStateUncertain, err)
			}
			err = errors.Join(err, errRollback, errState)
		}()
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	repoDir := s.repoDirSnapshot()
	if repoDir == "" {
		return fmt.Errorf("git token store: repository path not configured")
	}
	repo, err = git.PlainOpen(repoDir)
	if err != nil {
		return fmt.Errorf("git token store: open repo: %w", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("git token store: worktree: %w", err)
	}
	if rollback != nil {
		headSnapshot, err = captureGitHeadSnapshot(repo)
		if err != nil {
			return err
		}
		indexEntries, err = captureGitIndexEntries(repo, relPaths)
		if err != nil {
			return err
		}
		stateCaptured = true
	}
	added := false
	for _, rel := range relPaths {
		if strings.TrimSpace(rel) == "" {
			continue
		}
		if _, err = worktree.Add(rel); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if _, errRemove := worktree.Remove(rel); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
					return fmt.Errorf("git token store: remove %s: %w", rel, errRemove)
				}
			} else {
				return fmt.Errorf("git token store: add %s: %w", rel, err)
			}
		}
		added = true
	}
	if !added {
		return nil
	}
	status, err := worktree.Status()
	if err != nil {
		return fmt.Errorf("git token store: status: %w", err)
	}
	if status.IsClean() {
		pushAttempted = true
		if err = s.pushRepositoryHead(ctx, repo); err != nil {
			return err
		}
		s.maybeRunGC(repo)
		return nil
	}
	if strings.TrimSpace(message) == "" {
		message = "Update auth store"
	}
	signature := &object.Signature{
		Name:  "CLIProxyAPI",
		Email: "cliproxy@local",
		When:  time.Now(),
	}
	commitHash, err := worktree.Commit(
		message, &git.CommitOptions{
			Author: signature,
		},
	)
	if err != nil {
		if errors.Is(err, git.ErrEmptyCommit) {
			pushAttempted = true
			if err = s.pushRepositoryHead(ctx, repo); err != nil {
				return err
			}
			s.maybeRunGC(repo)
			return nil
		}
		return fmt.Errorf("git token store: commit: %w", err)
	}
	headRef, errHead := repo.Head()
	if errHead != nil {
		if !errors.Is(errHead, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("git token store: get head: %w", errHead)
		}
	} else if errRewrite := s.rewriteHeadAsSingleCommit(
		repo, headRef.Name(), commitHash, message, signature,
	); errRewrite != nil {
		return errRewrite
	}
	if errContext := ctx.Err(); errContext != nil {
		return errContext
	}
	pushAttempted = true
	if err = s.pushRepositoryHead(ctx, repo); err != nil {
		return err
	}
	s.maybeRunGC(repo)
	return nil
}

func (s *GitTokenStore) pushRepositoryHead(ctx context.Context, repo *git.Repository) error {
	headRef, errHead := repo.Head()
	if errHead != nil {
		return fmt.Errorf("git token store: get push head: %w", errHead)
	}
	pushBranch := s.branchRef()
	if pushBranch == "" {
		pushBranch = headRef.Name()
	}
	pushOpts := &git.PushOptions{
		ClientOptions: s.gitClientOptions(),
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("%s:%s", pushBranch.String(), pushBranch.String())),
		},
	}
	remoteRefName := plumbing.NewRemoteReferenceName("origin", pushBranch.Short())
	remoteRef, errRemoteRef := repo.Reference(remoteRefName, true)
	switch {
	case errRemoteRef == nil && remoteRef.Type() == plumbing.HashReference:
		pushOpts.ForceWithLease = &git.ForceWithLease{
			RefName: pushBranch,
			Hash:    remoteRef.Hash(),
		}
	case errRemoteRef != nil && !errors.Is(errRemoteRef, plumbing.ErrReferenceNotFound):
		return fmt.Errorf("git token store: inspect remote tracking ref: %w", errRemoteRef)
	}
	if s.beforePush != nil {
		s.beforePush()
	}
	if errPush := repo.PushContext(ctx, pushOpts); errPush != nil && !errors.Is(errPush, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("git token store: push: %w", errPush)
	}
	return nil
}

// rewriteHeadAsSingleCommit rewrites the current branch tip to a single-parentless commit and leaves history squashed.
func (s *GitTokenStore) rewriteHeadAsSingleCommit(
	repo *git.Repository,
	branch plumbing.ReferenceName,
	commitHash plumbing.Hash,
	message string,
	signature *object.Signature,
) error {
	commitObj, err := repo.CommitObject(commitHash)
	if err != nil {
		return fmt.Errorf("git token store: inspect head commit: %w", err)
	}
	squashed := &object.Commit{
		Author:       *signature,
		Committer:    *signature,
		Message:      message,
		TreeHash:     commitObj.TreeHash,
		ParentHashes: nil,
		Encoding:     commitObj.Encoding,
		ExtraHeaders: commitObj.ExtraHeaders,
	}
	mem := &plumbing.MemoryObject{}
	mem.SetType(plumbing.CommitObject)
	if err := squashed.Encode(mem); err != nil {
		return fmt.Errorf("git token store: encode squashed commit: %w", err)
	}
	newHash, err := repo.Storer.SetEncodedObject(mem)
	if err != nil {
		return fmt.Errorf("git token store: write squashed commit: %w", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(branch, newHash)); err != nil {
		return fmt.Errorf("git token store: update branch reference: %w", err)
	}
	return nil
}

func (s *GitTokenStore) maybeRunGC(repo *git.Repository) {
	now := time.Now()
	if now.Sub(s.lastGC) < gcInterval {
		return
	}
	s.lastGC = now

	pruneOpts := git.PruneOptions{
		OnlyObjectsOlderThan: now.Add(-gcPruneGracePeriod),
		Handler:              repo.DeleteObject,
	}
	if err := repo.Prune(pruneOpts); err != nil && !errors.Is(err, git.ErrLooseObjectsNotSupported) {
		return
	}
	_ = repo.RepackObjects(&git.RepackConfig{})
}

// PersistConfig commits and pushes configuration changes to git.
func (s *GitTokenStore) PersistConfig(ctx context.Context) error {
	if err := s.operationMu.lockContext(ctx); err != nil {
		return err
	}
	defer s.operationMu.Unlock()
	if err := s.ensureRepository(ctx); err != nil {
		return err
	}
	configPath := s.configPathSnapshot()
	if configPath == "" {
		return fmt.Errorf("git token store: config path not configured")
	}
	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("git token store: stat config: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, err := s.relativeToRepo(configPath)
	if err != nil {
		return err
	}
	return s.commitAndPushLocked(ctx, "Update config", rel)
}

func ensureEmptyFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return os.WriteFile(path, []byte{}, 0o600)
		}
		return err
	}
	return nil
}

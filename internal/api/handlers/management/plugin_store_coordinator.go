package management

import (
	"context"
	"errors"
	"fmt"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
)

type pluginOperationLocks struct {
	handler      *Handler
	pluginsDir   string
	commitMuHeld bool
	rootLock     *pluginstore.RootLock
}

func beginPluginOperation(handler *Handler) *pluginOperationLocks {
	handler.commitMu.Lock()
	return &pluginOperationLocks{handler: handler, commitMuHeld: true}
}

func (l *pluginOperationLocks) acquireRoot(pluginsDir string) error {
	if l.rootLock != nil {
		return nil
	}
	rootLock, errLock := pluginstore.LockRoot(pluginsDir)
	if errLock != nil {
		return errLock
	}
	l.pluginsDir = pluginsDir
	l.rootLock = rootLock
	return nil
}

func (l *pluginOperationLocks) acquireCommit() {
	if l.commitMuHeld {
		return
	}
	l.handler.commitMu.Lock()
	l.commitMuHeld = true
}

func (l *pluginOperationLocks) releaseRoot() {
	if l.rootLock == nil {
		return
	}
	l.rootLock.Unlock()
	l.rootLock = nil
}

func (l *pluginOperationLocks) releaseCommit() {
	if !l.commitMuHeld {
		return
	}
	l.handler.commitMu.Unlock()
	l.commitMuHeld = false
}

func (l *pluginOperationLocks) release() {
	l.releaseRoot()
	l.releaseCommit()
}

type pluginInstallCoordinator struct {
	locks              *pluginOperationLocks
	id                 string
	host               pluginRuntimeHost
	transaction        *pluginstore.InstallTransaction
	runtimePrepared    bool
	previousConfig     *config.Config
	previousGeneration uint64
}

func beginPluginInstallCoordinator(
	handler *Handler,
	id string,
	pluginsDir string,
) (*pluginInstallCoordinator, error) {
	coordinator := &pluginInstallCoordinator{
		locks: beginPluginOperation(handler),
		id:    id,
	}
	if errLock := coordinator.locks.acquireRoot(pluginsDir); errLock != nil {
		coordinator.release()
		return nil, errLock
	}
	return coordinator, nil
}

func (c *pluginInstallCoordinator) configureRuntime(
	host pluginRuntimeHost,
	previousConfig *config.Config,
	previousGeneration uint64,
) {
	c.host = host
	c.previousConfig = previousConfig
	c.previousGeneration = previousGeneration
}

func (c *pluginInstallCoordinator) installOptions(goos, goarch string) pluginstore.InstallOptions {
	return pluginstore.InstallOptions{
		PluginsDir:   c.locks.pluginsDir,
		GOOS:         goos,
		GOARCH:       goarch,
		PluginLoaded: func() bool { return pluginBusy(c.host, c.id) },
		BeforePublish: func(path, version string, overwritten bool, digest string) error {
			transaction, errTransaction := pluginstore.PrepareInstallTransactionLocked(
				c.locks.pluginsDir,
				c.id,
				path,
				version,
				digest,
				overwritten,
				c.locks.rootLock,
			)
			if errTransaction != nil {
				return errTransaction
			}
			c.transaction = transaction
			return nil
		},
		BeforeWrite: func() error {
			wasBusy := pluginBusy(c.host, c.id)
			if errOverwrite := preparePluginStoreOverwrite(c.id, c.host); errOverwrite != nil {
				return errOverwrite
			}
			c.runtimePrepared = wasBusy
			return nil
		},
		RootLock: c.locks.rootLock,
	}
}

func (c *pluginInstallCoordinator) hasTransaction() bool {
	return c.transaction != nil
}

func (c *pluginInstallCoordinator) setExpectedConfigFingerprint(fingerprint string) error {
	if c.transaction == nil {
		return nil
	}
	return c.transaction.SetExpectedConfigFingerprint(fingerprint)
}

func (c *pluginInstallCoordinator) commitArtifact() error {
	if c.transaction == nil {
		return nil
	}
	return c.transaction.Commit()
}

func (c *pluginInstallCoordinator) abort(result pluginstore.InstallResult) error {
	var abortErrors []error
	if c.transaction != nil {
		if errRollback := c.transaction.Rollback(result); errRollback != nil {
			abortErrors = append(abortErrors, fmt.Errorf("roll back plugin artifact: %w", errRollback))
		}
	}
	c.locks.release()
	if c.runtimePrepared {
		previousCommit := ConfigCommit{Config: c.previousConfig, Generation: c.previousGeneration}
		if errSync := c.locks.handler.runPluginRuntimeSyncForCommit(
			context.Background(), previousCommit, c.locks.handler.pluginRuntimeSync,
		); errSync != nil {
			abortErrors = append(abortErrors, fmt.Errorf("restore plugin runtime: %w", errSync))
		}
	}
	return errors.Join(abortErrors...)
}

func (c *pluginInstallCoordinator) release() {
	if c == nil || c.locks == nil {
		return
	}
	c.locks.release()
}

type pluginDeleteCoordinator struct {
	locks       *pluginOperationLocks
	transaction *pluginstore.DeletionTransaction
}

func beginPluginDeleteCoordinator(handler *Handler) *pluginDeleteCoordinator {
	return &pluginDeleteCoordinator{locks: beginPluginOperation(handler)}
}

func (c *pluginDeleteCoordinator) acquireRoot(pluginsDir string) error {
	return c.locks.acquireRoot(pluginsDir)
}

func (c *pluginDeleteCoordinator) rootLock() *pluginstore.RootLock {
	return c.locks.rootLock
}

func (c *pluginDeleteCoordinator) setTransaction(transaction *pluginstore.DeletionTransaction) {
	c.transaction = transaction
}

func (c *pluginDeleteCoordinator) releaseLocks() {
	c.locks.release()
}

func (c *pluginDeleteCoordinator) reacquireCommit() {
	c.locks.acquireCommit()
}

func (c *pluginDeleteCoordinator) rollback() error {
	var rollbackErrors []error
	if errLock := c.locks.acquireRoot(c.locks.pluginsDir); errLock != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("lock root for rollback: %w", errLock))
	} else if c.transaction != nil {
		if errRollback := c.transaction.Rollback(); errRollback != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restore deleted artifacts: %w", errRollback))
		}
	}
	c.locks.release()
	return errors.Join(rollbackErrors...)
}

func (c *pluginDeleteCoordinator) commitArtifact() error {
	if errLock := c.locks.acquireRoot(c.locks.pluginsDir); errLock != nil {
		return errLock
	}
	defer c.locks.releaseRoot()
	if c.transaction == nil {
		return nil
	}
	return c.transaction.Commit()
}

func (c *pluginDeleteCoordinator) release() {
	if c == nil || c.locks == nil {
		return
	}
	c.locks.release()
	if c.transaction != nil {
		c.transaction.Deactivate()
	}
}

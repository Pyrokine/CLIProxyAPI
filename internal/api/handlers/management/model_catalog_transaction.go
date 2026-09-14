package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type modelCatalogMutationOptions struct {
	backup bool
}

type modelCatalogMutationResult struct {
	doc        modelcatalog.File
	backupPath string
	config     *config.Config
	generation uint64
}

func (h *Handler) mutateModelCatalog(
	c *gin.Context,
	options modelCatalogMutationOptions,
	mutate func(*modelcatalog.File) error,
) (modelCatalogMutationResult, bool) {
	if c == nil {
		return modelCatalogMutationResult{}, false
	}
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return modelCatalogMutationResult{}, false
	}

	h.commitMu.Lock()
	defer h.commitMu.Unlock()
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return modelCatalogMutationResult{}, false
	}
	previousConfig, err := config.Clone(h.cfg)
	if err != nil {
		h.mu.Unlock()
		log.WithError(err).Error("failed to clone config for model catalog rollback")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare model catalog update"})
		return modelCatalogMutationResult{}, false
	}
	next, err := config.Clone(h.cfg)
	if err != nil {
		h.mu.Unlock()
		log.WithError(err).Error("failed to clone config for model catalog update")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare model catalog update"})
		return modelCatalogMutationResult{}, false
	}
	transaction, err := modelcatalog.PrepareUpdate(
		h.configFilePath,
		h.cfg,
		options.backup,
		mutate,
	)
	if err != nil {
		h.mu.Unlock()
		respondModelCatalogMutationError(c, err)
		return modelCatalogMutationResult{}, false
	}
	baseDoc := transaction.BaseDocument()
	doc := transaction.Document()
	previousConfig.ModelCatalog.Models = baseDoc.ToModelCatalogModels()
	previousConfig.OAuthModelAlias = baseDoc.ToOAuthModelAlias()
	next.ModelCatalog.Models = doc.ToModelCatalogModels()
	next.OAuthModelAlias = doc.ToOAuthModelAlias()
	persistedHash, err := h.persistConfigCandidate(next)
	if err != nil {
		h.mu.Unlock()
		rollbackErr := transaction.Rollback()
		respondModelCatalogMutationError(c, errors.Join(err, rollbackErr))
		return modelCatalogMutationResult{}, false
	}
	commit, callback := h.publishConfigLocked(next, persistedHash)
	commit.ModelCatalogPrepared = true
	h.mu.Unlock()

	ctx := context.Background()
	if c.Request != nil {
		ctx = context.WithoutCancel(c.Request.Context())
	}
	if err = h.commitModelCatalogTransaction(
		ctx,
		callback,
		commit,
		doc,
		baseDoc,
		previousConfig,
		transaction,
	); err != nil {
		log.WithError(err).WithField("generation", commit.Generation).
			Error("model catalog runtime transaction failed")
		c.JSON(
			http.StatusServiceUnavailable,
			gin.H{"error": "model_catalog_runtime_sync_failed", "generation": commit.Generation},
		)
		return modelCatalogMutationResult{}, false
	}
	return modelCatalogMutationResult{
		doc:        doc,
		backupPath: transaction.BackupPath(),
		config:     commit.Config,
		generation: commit.Generation,
	}, true
}

func (h *Handler) commitModelCatalogTransaction(
	ctx context.Context,
	callback ConfigCommittedFunc,
	commit ConfigCommit,
	doc modelcatalog.File,
	baseDoc modelcatalog.File,
	previousConfig *config.Config,
	transaction *modelcatalog.UpdateTransaction,
) error {
	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	if !h.configCommitCurrent(commit) {
		return errors.Join(
			fmt.Errorf("model catalog config generation changed before runtime commit"),
			transaction.Rollback(),
		)
	}

	var transactionErr error
	if callback != nil {
		transactionErr = invokeConfigCommitted(ctx, callback, commit)
	}
	if transactionErr == nil && !h.configCommitCurrent(commit) {
		transactionErr = fmt.Errorf("model catalog config generation changed during service runtime commit")
	}
	if transactionErr == nil {
		transactionErr = h.applyModelCatalogRuntime(commit.Config, doc)
	}
	if transactionErr == nil && !h.configCommitCurrent(commit) {
		transactionErr = fmt.Errorf("model catalog config generation changed during handler runtime commit")
	}
	if transactionErr == nil {
		transactionErr = transaction.CommitFiles()
	}
	if transactionErr == nil {
		return transaction.Finish()
	}

	rollbackErr := transaction.Rollback()
	configRollbackErr := h.rollbackModelCatalogConfig(ctx, commit, previousConfig, baseDoc)
	return errors.Join(transactionErr, rollbackErr, configRollbackErr)
}

func (h *Handler) rollbackModelCatalogConfig(
	ctx context.Context,
	failedCommit ConfigCommit,
	previousConfig *config.Config,
	baseDoc modelcatalog.File,
) error {
	if previousConfig == nil {
		return fmt.Errorf("previous model catalog config is nil")
	}
	h.mu.Lock()
	if h.configGeneration != failedCommit.Generation {
		h.mu.Unlock()
		return fmt.Errorf("model catalog config generation changed before rollback")
	}
	persistedHash, persistErr := h.persistModelCatalogRollback(previousConfig)
	rollbackCommit, callback := h.publishConfigLocked(previousConfig, persistedHash)
	rollbackCommit.ModelCatalogPrepared = true
	h.mu.Unlock()

	var errs []error
	if persistErr != nil {
		errs = append(errs, persistErr)
	}
	if callback != nil {
		if err := invokeConfigCommitted(ctx, callback, rollbackCommit); err != nil {
			errs = append(errs, fmt.Errorf("restore model catalog service runtime: %w", err))
		}
	}
	if err := h.applyModelCatalogRuntime(previousConfig, baseDoc); err != nil {
		errs = append(errs, fmt.Errorf("restore model catalog handler runtime: %w", err))
	}
	return errors.Join(errs...)
}

func (h *Handler) persistModelCatalogRollback(previousConfig *config.Config) (string, error) {
	if strings.TrimSpace(h.configFilePath) == "" {
		return "", nil
	}
	persistedHash, err := config.SaveConfigPreserveCommentsWithDigest(h.configFilePath, previousConfig)
	if err != nil {
		return "", fmt.Errorf("restore config after model catalog failure: %w", err)
	}
	return persistedHash, nil
}

func respondModelCatalogMutationError(c *gin.Context, err error) {
	if mutationError, ok := errors.AsType[*configMutationError](err); ok {
		c.JSON(mutationError.status, gin.H{"error": mutationError.message})
		return
	}
	if persistenceError, ok := errors.AsType[*configPersistenceError](err); ok {
		respondConfigPersistenceError(c, persistenceError)
		return
	}
	log.WithError(err).Error("failed to update model catalog")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update model catalog"})
}

func (h *Handler) applyModelCatalogRuntime(
	cfg *config.Config,
	doc modelcatalog.File,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("model catalog runtime sync panicked: %v", recovered)
		}
	}()

	catalog := doc.ToModelCatalogModels()
	aliases := doc.ToOAuthModelAlias()
	config.SetRuntimeModelCatalog(catalog)
	if h != nil && h.authManager != nil {
		h.authManager.SetOAuthModelAlias(aliases)
	}
	if h != nil && h.modelCatalogRuntimeSync != nil {
		return h.modelCatalogRuntimeSync(cfg)
	}
	return nil
}

package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return modelCatalogMutationResult{}, false
	}

	h.commitMu.Lock()

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		h.commitMu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return modelCatalogMutationResult{}, false
	}
	next, err := config.Clone(h.cfg)
	if err != nil {
		h.mu.Unlock()
		h.commitMu.Unlock()
		log.WithError(err).Error("failed to clone config for model catalog update")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare model catalog update"})
		return modelCatalogMutationResult{}, false
	}

	var commit ConfigCommit
	var callback ConfigCommittedFunc
	doc, backupPath, err := modelcatalog.Update(
		h.configFilePath,
		h.cfg,
		modelcatalog.UpdateOptions{
			Backup: options.backup,
			Commit: func(saved modelcatalog.File) error {
				next.ModelCatalog.Models = saved.ToModelCatalogModels()
				next.OAuthModelAlias = saved.ToOAuthModelAlias()
				persistedHash, errPersist := h.persistConfigCandidate(next)
				if errPersist != nil {
					return errPersist
				}
				commit, callback = h.publishConfigLocked(next, persistedHash)
				return nil
			},
		},
		mutate,
	)
	h.mu.Unlock()
	h.commitMu.Unlock()
	if err != nil {
		respondModelCatalogMutationError(c, err)
		return modelCatalogMutationResult{}, false
	}

	result := modelCatalogMutationResult{
		doc:        doc,
		backupPath: backupPath,
		config:     commit.Config,
		generation: commit.Generation,
	}
	if err = h.syncModelCatalogCommit(c, callback, commit, doc); err != nil {
		log.WithError(err).WithField("generation", commit.Generation).
			Error("model catalog committed but runtime sync failed")
		c.JSON(
			http.StatusServiceUnavailable,
			gin.H{"error": "model_catalog_runtime_sync_failed", "generation": commit.Generation},
		)
		return result, false
	}
	return result, true
}

func (h *Handler) syncModelCatalogCommit(
	c *gin.Context,
	callback ConfigCommittedFunc,
	commit ConfigCommit,
	doc modelcatalog.File,
) error {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = context.WithoutCancel(c.Request.Context())
	}
	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	if !h.configCommitCurrent(commit) {
		return nil
	}
	var errs []error
	if callback != nil {
		if err := invokeConfigCommitted(ctx, callback, commit); err != nil {
			errs = append(errs, err)
		}
	}
	runtimeCfg := commit.Config
	if callback != nil {
		runtimeCfg = h.configSnapshot()
	}
	if h.configCommitCurrent(commit) {
		if err := h.applyModelCatalogRuntime(runtimeCfg, doc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func respondModelCatalogMutationError(c *gin.Context, err error) {
	var mutationError *configMutationError
	if errors.As(err, &mutationError) {
		c.JSON(mutationError.status, gin.H{"error": mutationError.message})
		return
	}
	var persistenceError *configPersistenceError
	if errors.As(err, &persistenceError) {
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
		h.modelCatalogRuntimeSync(cfg)
	}
	return nil
}

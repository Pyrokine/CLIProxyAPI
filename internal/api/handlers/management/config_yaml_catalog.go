package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
)

type configYAMLCatalogTransaction struct {
	transaction    *modelcatalog.UpdateTransaction
	previousConfig *config.Config
	previousYAML   []byte
	previousExists bool
	rollbackUsage  func() error
}

func (h *Handler) prepareConfigYAMLCatalog(
	body []byte,
	next *config.Config,
	previous []byte,
	exists bool,
) (*configYAMLCatalogTransaction, error) {
	document, err := decodeConfigYAML(body)
	if err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return nil, nil
	}
	aliases, _, err := yamlEffectiveMappingValue(document.Content[0], "oauth-model-alias", nil)
	if err != nil || aliases == nil {
		return nil, err
	}
	previousConfig, err := config.Clone(h.cfg)
	if err != nil {
		return nil, err
	}
	transaction, err := modelcatalog.PrepareUpdate(
		h.configFilePath, h.cfg, false, func(doc *modelcatalog.File) error {
			modelcatalog.ReplaceOAuthModelAlias(doc, next.OAuthModelAlias)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	base, candidate := transaction.BaseDocument(), transaction.Document()
	previousConfig.ModelCatalog.Models = base.ToModelCatalogModels()
	previousConfig.OAuthModelAlias = base.ToOAuthModelAlias()
	next.ModelCatalog.Models = candidate.ToModelCatalogModels()
	next.OAuthModelAlias = candidate.ToOAuthModelAlias()
	return &configYAMLCatalogTransaction{
		transaction: transaction, previousConfig: previousConfig, previousYAML: append([]byte(nil), previous...),
		previousExists: exists,
	}, nil
}

func (h *Handler) commitConfigYAMLCatalog(
	ctx context.Context,
	callback ConfigCommittedFunc,
	commit ConfigCommit,
) error {
	prepared := commit.yamlCatalog
	restore := func() (string, error) {
		var usageError error
		if prepared.rollbackUsage != nil {
			usageError = prepared.rollbackUsage()
		}
		if !prepared.previousExists {
			err := os.Remove(h.configFilePath)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			return "", errors.Join(usageError, err)
		}
		if err := writeConfigFileAtomically(h.configFilePath, prepared.previousYAML); err != nil {
			return "", errors.Join(usageError, err)
		}
		digest := sha256.Sum256(prepared.previousYAML)
		return hex.EncodeToString(digest[:]), usageError
	}
	err := h.commitModelCatalogTransaction(
		ctx, callback, commit, prepared.transaction.Document(), prepared.transaction.BaseDocument(),
		prepared.previousConfig, prepared.transaction, restore,
	)
	if err != nil {
		failMutation(ctx)
		return err
	}
	RecordMutationEffect(ctx, "model.persisted", "model")
	RecordMutationEffect(ctx, "model.published", "model")
	return nil
}

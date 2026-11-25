package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"gopkg.in/yaml.v3"
)

type configYAMLCatalogTransaction struct {
	transaction    *modelcatalog.UpdateTransaction
	previousConfig *config.Config
	previousYAML   []byte
	previousExists bool
	writesCatalog  bool
	rollbackUsage  func() error
}

func (h *Handler) prepareConfigYAMLCatalog(
	body []byte,
	next *config.Config,
	previous []byte,
	exists bool,
	conditional bool,
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
	replaceAliases := true
	if conditional && exists {
		unchanged, err := unchangedYAMLOAuthAliases(previous, aliases)
		if err != nil {
			return nil, err
		}
		replaceAliases = !unchanged
	}
	previousConfig, err := config.Clone(h.cfg)
	if err != nil {
		return nil, err
	}
	var mutate func(*modelcatalog.File) error
	if replaceAliases {
		mutate = func(doc *modelcatalog.File) error {
			modelcatalog.ReplaceOAuthModelAlias(doc, next.OAuthModelAlias)
			return nil
		}
	}
	transaction, err := modelcatalog.PrepareUpdate(h.configFilePath, h.cfg, false, mutate)
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
		previousExists: exists, writesCatalog: replaceAliases,
	}, nil
}

func unchangedYAMLOAuthAliases(previous []byte, aliases *yaml.Node) (bool, error) {
	document, err := decodeConfigYAML(previous)
	if err != nil || len(document.Content) == 0 {
		return false, err
	}
	before, _, err := yamlEffectiveMappingValue(document.Content[0], "oauth-model-alias", nil)
	if err != nil || before == nil {
		return false, err
	}
	var previousValue, nextValue any
	if err = before.Decode(&previousValue); err != nil {
		return false, err
	}
	if err = aliases.Decode(&nextValue); err != nil {
		return false, err
	}
	return reflect.DeepEqual(previousValue, nextValue), nil
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
	if prepared.writesCatalog {
		RecordMutationEffect(ctx, "model.persisted", "model")
	}
	RecordMutationEffect(ctx, "model.published", "model")
	return nil
}

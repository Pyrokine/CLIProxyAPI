package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type modelCatalogResponse struct {
	Models []modelCatalogRow `json:"models"`
	Meta   gin.H             `json:"meta"`
}

type modelCatalogRow struct {
	ID                        string           `json:"id"`
	Name                      string           `json:"name"`
	Provider                  string           `json:"provider"`
	Channel                   string           `json:"channel"`
	Group                     string           `json:"group"`
	DisplayName               string           `json:"display_name,omitempty"`
	UserCreated               bool             `json:"user_created"`
	Enabled                   bool             `json:"enabled"`
	RuntimeAvailable          bool             `json:"runtime_available"`
	RuntimeProviders          []string         `json:"runtime_providers,omitempty"`
	Requestable               bool             `json:"requestable"`
	NotRequestableReason      string           `json:"not_requestable_reason,omitempty"`
	Aliases                   []string         `json:"aliases,omitempty"`
	Price                     *modelPriceEntry `json:"price,omitempty"`
	APIKeyBlockedCount        int              `json:"api_key_blocked_count,omitempty"`
	CredentialExcludedCount   int              `json:"credential_excluded_count,omitempty"`
	CredentialConfiguredCount int              `json:"credential_configured_count,omitempty"`
}

type modelCatalogPatchRequest struct {
	Name             string           `json:"name"`
	Provider         *string          `json:"provider"`
	Channel          *string          `json:"channel"`
	Group            *string          `json:"group"`
	DisplayName      *string          `json:"display_name"`
	DisplayNameKebab *string          `json:"display-name"`
	Enabled          *bool            `json:"enabled"`
	Aliases          *[]string        `json:"aliases"`
	Price            *modelPriceEntry `json:"price"`
	ClearPrice       *bool            `json:"clear_price"`
}

func (h *Handler) GetModelCatalog(c *gin.Context) {
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.cfg)
	if err != nil {
		log.Warnf("model-catalog: failed to load models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load model catalog"})
		return
	}
	h.applyModelCatalogRuntime(doc)
	c.JSON(http.StatusOK, h.modelCatalogResponse(doc))
}

func (h *Handler) PutModelCatalog(c *gin.Context) {
	var body struct {
		Models []modelcatalog.Model `json:"models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	existing, err := modelcatalog.LoadOrInit(h.configFilePath, h.cfg)
	if err != nil {
		log.Warnf("model-catalog: failed to load models.json before save: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load model catalog"})
		return
	}
	defaultsVersion := strings.TrimSpace(existing.DefaultsVersion)
	if defaultsVersion == "" {
		defaultsVersion = modelcatalog.DefaultsVersion
	}
	doc := modelcatalog.File{
		SchemaVersion:        modelcatalog.SchemaVersion,
		DefaultsVersion:      defaultsVersion,
		RemovedDefaultModels: append([]string(nil), existing.RemovedDefaultModels...),
		Models:               body.Models,
	}
	doc.Sanitize()
	if err := modelcatalog.Save(h.configFilePath, doc); err != nil {
		log.Warnf("model-catalog: failed to save models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model catalog"})
		return
	}
	h.applyModelCatalogRuntime(doc)
	h.refreshModelPricing(false)
	c.JSON(http.StatusOK, h.modelCatalogResponse(doc))
}

func (h *Handler) PatchModelCatalogModel(c *gin.Context) {
	var body modelCatalogPatchRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	body.Name = strings.TrimSpace(strings.TrimPrefix(body.Name, "models/"))
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.cfg)
	if err != nil {
		log.Warnf("model-catalog: failed to load models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load model catalog"})
		return
	}
	model := doc.EnsureModel(body.Name, true)
	applyModelCatalogPatch(model, body)
	doc.Sanitize()
	if err := modelcatalog.Save(h.configFilePath, doc); err != nil {
		log.Warnf("model-catalog: failed to save models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model catalog"})
		return
	}
	h.applyModelCatalogRuntime(doc)
	h.refreshModelPricing(body.Price != nil || (body.ClearPrice != nil && *body.ClearPrice))
	c.JSON(http.StatusOK, h.modelCatalogResponse(doc))
}

func (h *Handler) DeleteModelCatalogModel(c *gin.Context) {
	name := strings.TrimSpace(strings.TrimPrefix(c.Query("name"), "models/"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.cfg)
	if err != nil {
		log.Warnf("model-catalog: failed to load models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load model catalog"})
		return
	}
	if !doc.RemoveModel(name, modelcatalog.DefaultFile()) {
		c.JSON(http.StatusNotFound, gin.H{"error": "model catalog entry not found"})
		return
	}
	doc.Sanitize()
	if err := modelcatalog.Save(h.configFilePath, doc); err != nil {
		log.Warnf("model-catalog: failed to save models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save model catalog"})
		return
	}
	h.applyModelCatalogRuntime(doc)
	h.refreshModelPricing(false)
	c.JSON(http.StatusOK, h.modelCatalogResponse(doc))
}

func (h *Handler) GetModelCatalogDefaultUpdate(c *gin.Context) {
	preview, err := modelcatalog.PreviewDefaultUpdate(h.configFilePath)
	if err != nil {
		log.Warnf("model-catalog: failed to build default update preview: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build default update preview"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

func (h *Handler) ApplyModelCatalogDefaultUpdate(c *gin.Context) {
	var body struct {
		Decisions []modelcatalog.ApplyDecision `json:"decisions"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	result, err := modelcatalog.ApplyDefaultUpdate(h.configFilePath, body.Decisions)
	if err != nil {
		log.Warnf("model-catalog: failed to apply default update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply default update"})
		return
	}
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.cfg)
	if err != nil {
		log.Warnf("model-catalog: failed to reload models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reload model catalog"})
		return
	}
	h.applyModelCatalogRuntime(doc)
	h.refreshModelPricing(true)
	c.JSON(http.StatusOK, gin.H{"result": result, "catalog": h.modelCatalogResponse(doc)})
}

func applyModelCatalogPatch(model *modelcatalog.Model, body modelCatalogPatchRequest) {
	if model == nil {
		return
	}
	if body.Provider != nil {
		model.Provider = strings.TrimSpace(*body.Provider)
	}
	if body.Channel != nil {
		model.Channel = strings.ToLower(strings.TrimSpace(*body.Channel))
	}
	if body.Group != nil {
		model.Group = strings.TrimSpace(*body.Group)
	}
	if body.DisplayName != nil {
		model.DisplayName = strings.TrimSpace(*body.DisplayName)
	}
	if body.DisplayNameKebab != nil {
		model.DisplayName = strings.TrimSpace(*body.DisplayNameKebab)
	}
	if body.Enabled != nil {
		model.Enabled = *body.Enabled
	}
	if body.Aliases != nil {
		model.Aliases = append([]string(nil), *body.Aliases...)
	}
	if body.Price != nil {
		price := modelcatalog.Price{
			Prompt: body.Price.Prompt, Completion: body.Price.Completion, Cache: body.Price.Cache,
		}
		model.Price = &price
	}
	if body.ClearPrice != nil && *body.ClearPrice {
		model.Price = nil
	}
}

func (h *Handler) modelCatalogResponse(doc modelcatalog.File) modelCatalogResponse {
	rows := make([]modelCatalogRow, 0, len(doc.Models))
	for _, model := range doc.Models {
		providers := registry.GetGlobalRegistry().GetModelProviders(model.Name)
		runtimeAvailable := len(providers) > 0
		apiKeyBlocked := apiKeyBlockedCount(h.cfg, model.Name)
		credentialExcluded := credentialExcludedCount(h.cfg, model.Name)
		credentialConfigured := credentialConfiguredCount(h.cfg, model.Name)
		accessRestricted := (apiKeyRuleCount(h.cfg) > 0 && apiKeyBlocked >= apiKeyRuleCount(h.cfg)) ||
			(credentialConfigured > 0 && credentialExcluded >= credentialConfigured)
		requestable := model.Enabled && runtimeAvailable && !accessRestricted
		reason := ""
		if !model.Enabled {
			reason = "disabled"
		} else if !runtimeAvailable {
			reason = "not_runtime"
		} else if accessRestricted {
			reason = "access_restricted"
		}
		row := modelCatalogRow{
			ID:                        "model:" + strings.ToLower(model.Name),
			Name:                      model.Name,
			Provider:                  model.Provider,
			Channel:                   model.Channel,
			Group:                     model.Group,
			DisplayName:               model.DisplayName,
			UserCreated:               model.UserCreated,
			Enabled:                   model.Enabled,
			RuntimeAvailable:          runtimeAvailable,
			RuntimeProviders:          providers,
			Requestable:               requestable,
			NotRequestableReason:      reason,
			Aliases:                   append([]string(nil), model.Aliases...),
			APIKeyBlockedCount:        apiKeyBlocked,
			CredentialExcludedCount:   credentialExcluded,
			CredentialConfiguredCount: credentialConfigured,
		}
		if model.Price != nil {
			price := modelPriceEntry{
				Prompt: model.Price.Prompt, Completion: model.Price.Completion, Cache: model.Price.Cache,
			}
			row.Price = &price
		}
		rows = append(rows, row)
	}
	sort.Slice(
		rows, func(i, j int) bool {
			if rows[i].Provider == rows[j].Provider {
				if rows[i].Group == rows[j].Group {
					return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
				}
				return rows[i].Group < rows[j].Group
			}
			return rows[i].Provider < rows[j].Provider
		},
	)
	return modelCatalogResponse{
		Models: rows,
		Meta: gin.H{
			"models_config":         modelcatalog.FileName,
			"defaults_version":      doc.DefaultsVersion,
			"schema_version":        doc.SchemaVersion,
			"removed_default_count": len(doc.RemovedDefaultModels),
		},
	}
}

func (h *Handler) applyModelCatalogRuntime(doc modelcatalog.File) {
	catalog := doc.ToModelCatalogModels()
	aliases := doc.ToOAuthModelAlias()
	config.SetRuntimeModelCatalog(catalog)
	if h != nil && h.cfg != nil {
		h.cfg.ModelCatalog.Models = catalog
		h.cfg.OAuthModelAlias = aliases
	}
	if h != nil && h.authManager != nil {
		h.authManager.SetOAuthModelAlias(aliases)
	}
	if h != nil && h.modelCatalogRuntimeSync != nil {
		h.modelCatalogRuntimeSync(h.cfg)
	}
}

func (h *Handler) refreshModelPricing(recalculate bool) {
	if h == nil || h.usagePersister == nil {
		return
	}
	priceFn := internalUsage.BuildModelPriceFunc(h.configFilePath)
	h.usagePersister.SetPriceFunc(priceFn)
	if !recalculate || !h.usagePersister.HasPricing() {
		return
	}
	if _, err := h.usagePersister.RecalculateCosts(); err != nil {
		log.Warnf("model-catalog: cost recalculation failed: %v", err)
	}
}

func modelRuntimeAvailable(name string) bool {
	return len(
		registry.GetGlobalRegistry().GetModelProviders(
			strings.TrimSpace(
				strings.TrimPrefix(
					name, "models/",
				),
			),
		),
	) > 0
}

func apiKeyBlockedCount(cfg *config.Config, name string) int {
	if cfg == nil || len(cfg.APIKeyRules) == 0 {
		return 0
	}
	count := 0
	for apiKey := range cfg.APIKeyRules {
		if cfg.APIKeyBlocksModel(apiKey, name) {
			count++
		}
	}
	return count
}

func apiKeyRuleCount(cfg *config.Config) int {
	if cfg == nil {
		return 0
	}
	return len(cfg.APIKeyRules)
}

func credentialExcludedCount(cfg *config.Config, name string) int {
	if cfg == nil {
		return 0
	}
	count := 0
	for _, models := range cfg.OAuthExcludedModels {
		for _, model := range models {
			if catalogNamesMatch(model, name) {
				count++
			}
		}
	}
	return count
}

func credentialConfiguredCount(cfg *config.Config, name string) int {
	if cfg == nil {
		return 0
	}
	count := 0
	for _, aliases := range cfg.OAuthModelAlias {
		for _, alias := range aliases {
			if catalogNamesMatch(alias.Name, name) || catalogNamesMatch(alias.Alias, name) {
				count++
			}
		}
	}
	return count
}

func catalogNamesMatch(left, right string) bool {
	left = strings.TrimSpace(strings.TrimPrefix(left, "models/"))
	right = strings.TrimSpace(strings.TrimPrefix(right, "models/"))
	if left == "" || right == "" {
		return false
	}
	return strings.EqualFold(left, right)
}

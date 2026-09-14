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
	Models               []modelCatalogRow `json:"models"`
	Meta                 gin.H             `json:"meta"`
	RecalculationPending bool              `json:"recalculation_pending,omitempty"`
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
	RuntimeOnly               bool             `json:"runtime_only,omitempty"`
	RuntimeProviders          []string         `json:"runtime_providers,omitempty"`
	RuntimeAvailableProviders []string         `json:"runtime_available_providers,omitempty"`
	RuntimeConfiguredClients  int              `json:"runtime_configured_clients,omitempty"`
	RuntimeAvailableClients   int              `json:"runtime_available_clients,omitempty"`
	RuntimeQuotaCooldown      int              `json:"runtime_quota_cooldown_clients,omitempty"`
	RuntimeSuspended          int              `json:"runtime_suspended_clients,omitempty"`
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
	cfg := h.configSnapshot()
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, cfg)
	if err != nil {
		log.Warnf("model-catalog: failed to load models.json: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load model catalog"})
		return
	}
	if doc.LoadMetadata.RecoveredFromPrevious {
		log.WithField("current_error", doc.LoadMetadata.CurrentError).
			Warn("model-catalog: restored models.json from models.previous.json")
	}
	c.JSON(http.StatusOK, h.modelCatalogResponse(cfg, doc))
}

func (h *Handler) PutModelCatalog(c *gin.Context) {
	var body struct {
		Models []modelcatalog.Model `json:"models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	result, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			defaultsVersion := strings.TrimSpace(doc.DefaultsVersion)
			if defaultsVersion == "" {
				defaultsVersion = modelcatalog.DefaultsVersion
			}
			*doc = modelcatalog.File{
				SchemaVersion:        modelcatalog.SchemaVersion,
				DefaultsVersion:      defaultsVersion,
				RemovedDefaultModels: append([]string(nil), doc.RemovedDefaultModels...),
				OAuthModelAlias:      doc.ToOAuthModelAlias(),
				Prices:               doc.Prices,
				Models:               body.Models,
			}
			return nil
		},
	)
	if result.config != nil {
		h.refreshModelPricing(false)
	}
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.modelCatalogResponse(result.config, result.doc))
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
	result, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			model := doc.EnsureModel(body.Name, true)
			applyModelCatalogPatch(doc, model, body)
			if body.Aliases != nil {
				modelcatalog.ReplaceModelAliases(doc, body.Name, *body.Aliases)
			}
			return nil
		},
	)
	recalculate := body.Price != nil || (body.ClearPrice != nil && *body.ClearPrice)
	recalculationPending := false
	if result.config != nil {
		recalculationPending = h.refreshModelPricing(recalculate)
	}
	if !ok {
		return
	}
	response := h.modelCatalogResponse(result.config, result.doc)
	response.RecalculationPending = recalculationPending
	c.JSON(http.StatusOK, response)
}

func (h *Handler) DeleteModelCatalogModel(c *gin.Context) {
	name := strings.TrimSpace(strings.TrimPrefix(c.Query("name"), "models/"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	result, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			if !doc.RemoveModel(name, modelcatalog.DefaultFile()) {
				return newConfigMutationError(http.StatusNotFound, "model catalog entry not found")
			}
			return nil
		},
	)
	if result.config != nil {
		h.refreshModelPricing(false)
	}
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.modelCatalogResponse(result.config, result.doc))
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
	applied := 0
	result, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{backup: true},
		func(doc *modelcatalog.File) error {
			var err error
			applied, err = modelcatalog.ApplyDefaultDecisions(doc, body.Decisions)
			return err
		},
	)
	recalculationPending := false
	if result.config != nil {
		recalculationPending = h.refreshModelPricing(true)
	}
	if !ok {
		return
	}
	applyResult := modelcatalog.ApplyResult{
		Status:     "ok",
		BackupPath: result.backupPath,
		Applied:    applied,
	}
	catalog := h.modelCatalogResponse(result.config, result.doc)
	catalog.RecalculationPending = recalculationPending
	c.JSON(
		http.StatusOK,
		gin.H{
			"result":  applyResult,
			"catalog": catalog,
		},
	)
}

func applyModelCatalogPatch(doc *modelcatalog.File, model *modelcatalog.Model, body modelCatalogPatchRequest) {
	if doc == nil || model == nil {
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
	if body.Price != nil {
		doc.SetPrice(model.Name, *body.Price)
	}
	if body.ClearPrice != nil && *body.ClearPrice {
		doc.DeletePrice(model.Name)
	}
}

func (h *Handler) modelCatalogResponse(
	cfg *config.Config,
	doc modelcatalog.File,
) modelCatalogResponse {
	doc.Models = append([]modelcatalog.Model(nil), doc.Models...)
	aliasesByModel := make(map[string][]string, len(doc.Models))
	seenAliases := make(map[string]map[string]struct{}, len(doc.Models))
	catalogNameByCandidate := make(map[string]string, len(doc.Models))
	appendAlias := func(name, alias string) {
		nameKey := modelCatalogResponseKey(name)
		alias = strings.TrimSpace(strings.TrimPrefix(alias, "models/"))
		aliasKey := modelCatalogResponseKey(alias)
		if nameKey == "" || aliasKey == "" || nameKey == aliasKey {
			return
		}
		if seenAliases[nameKey] == nil {
			seenAliases[nameKey] = make(map[string]struct{})
		}
		if _, exists := seenAliases[nameKey][aliasKey]; exists {
			return
		}
		seenAliases[nameKey][aliasKey] = struct{}{}
		aliasesByModel[nameKey] = append(aliasesByModel[nameKey], alias)
		catalogNameByCandidate[aliasKey] = nameKey
	}
	for _, model := range doc.Models {
		nameKey := modelCatalogResponseKey(model.Name)
		catalogNameByCandidate[nameKey] = nameKey
		for _, alias := range model.Aliases {
			appendAlias(model.Name, alias)
		}
	}
	for _, entries := range doc.OAuthModelAlias {
		for _, entry := range entries {
			nameKey := modelCatalogResponseKey(entry.Name)
			catalogNameByCandidate[nameKey] = nameKey
			appendAlias(entry.Name, entry.Alias)
		}
	}
	for name := range aliasesByModel {
		sort.Strings(aliasesByModel[name])
	}

	availabilityByModel := make(map[string]registry.ModelAvailability)
	runtimeOnly := make(map[string]bool)
	for _, availability := range registry.GetGlobalRegistry().GetModelAvailabilitySnapshot() {
		candidateKey := modelCatalogResponseKey(availability.ModelID)
		nameKey := catalogNameByCandidate[candidateKey]
		if nameKey == "" {
			displayName := ""
			if availability.Info != nil {
				displayName = availability.Info.DisplayName
			}
			model := doc.EnsureRuntimeModel(availability.ModelID, availability.PreferredProvider, displayName)
			if model == nil {
				continue
			}
			nameKey = modelCatalogResponseKey(model.Name)
			catalogNameByCandidate[nameKey] = nameKey
			runtimeOnly[nameKey] = true
		}
		availabilityByModel[nameKey] = mergeModelAvailability(availabilityByModel[nameKey], availability)
	}

	rows := make([]modelCatalogRow, 0, len(doc.Models))
	for _, model := range doc.Models {
		nameKey := modelCatalogResponseKey(model.Name)
		availability := availabilityByModel[nameKey]
		runtimeAvailable := availability.AvailableClients > 0
		apiKeyBlocked := apiKeyBlockedCount(cfg, model.Name)
		credentialExcluded := h.credentialExcludedCount(
			model, aliasesByModel[nameKey], availability.ConfiguredProviders,
		)
		credentialConfigured := availability.ConfiguredClients + credentialExcluded
		accessRestricted := apiKeyRuleCount(cfg) > 0 && apiKeyBlocked >= apiKeyRuleCount(cfg)
		requestable := model.Enabled && runtimeAvailable && !accessRestricted
		reason := ""
		switch {
		case !model.Enabled:
			reason = "disabled"
		case accessRestricted:
			reason = "access_restricted"
		case availability.ConfiguredClients == 0 && credentialExcluded > 0:
			reason = "credentials_excluded"
		case availability.ConfiguredClients == 0:
			reason = "not_runtime"
		case availability.QuotaCooldownClients == availability.ConfiguredClients:
			reason = "quota_cooldown"
		case availability.SuspendedClients == availability.ConfiguredClients:
			reason = "suspended"
		case !runtimeAvailable:
			reason = "runtime_unavailable"
		}
		row := modelCatalogRow{
			ID:                        "model:" + nameKey,
			Name:                      model.Name,
			Provider:                  model.Provider,
			Channel:                   model.Channel,
			Group:                     model.Group,
			DisplayName:               model.DisplayName,
			UserCreated:               model.UserCreated,
			Enabled:                   model.Enabled,
			RuntimeAvailable:          runtimeAvailable,
			RuntimeOnly:               runtimeOnly[nameKey],
			RuntimeProviders:          availability.ConfiguredProviders,
			RuntimeAvailableProviders: availability.AvailableProviders,
			RuntimeConfiguredClients:  availability.ConfiguredClients,
			RuntimeAvailableClients:   availability.AvailableClients,
			RuntimeQuotaCooldown:      availability.QuotaCooldownClients,
			RuntimeSuspended:          availability.SuspendedClients,
			Requestable:               requestable,
			NotRequestableReason:      reason,
			Aliases:                   aliasesByModel[nameKey],
			APIKeyBlockedCount:        apiKeyBlocked,
			CredentialExcludedCount:   credentialExcluded,
			CredentialConfiguredCount: credentialConfigured,
		}
		if price, ok := doc.Price(model.Name); ok {
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
	loadSource := doc.LoadMetadata.Source
	if loadSource == "" {
		loadSource = modelcatalog.LoadSourceCurrent
	}
	meta := gin.H{
		"models_config":           modelcatalog.FileName,
		"previous_models_config":  modelcatalog.PreviousFileName,
		"load_source":             loadSource,
		"recovered_from_previous": doc.LoadMetadata.RecoveredFromPrevious,
		"defaults_version":        doc.DefaultsVersion,
		"schema_version":          doc.SchemaVersion,
		"removed_default_count":   len(doc.RemovedDefaultModels),
	}
	if doc.LoadMetadata.CurrentError != "" {
		meta["current_error"] = doc.LoadMetadata.CurrentError
	}
	return modelCatalogResponse{Models: rows, Meta: meta}
}

func modelCatalogResponseKey(name string) string {
	name = strings.TrimSpace(name)
	if len(name) >= len("models/") && strings.EqualFold(name[:len("models/")], "models/") {
		name = name[len("models/"):]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func mergeModelAvailability(left, right registry.ModelAvailability) registry.ModelAvailability {
	if left.ModelID == "" {
		return right
	}
	left.ConfiguredClients += right.ConfiguredClients
	left.AvailableClients += right.AvailableClients
	left.QuotaCooldownClients += right.QuotaCooldownClients
	left.SuspendedClients += right.SuspendedClients
	left.ConfiguredProviders = mergeCatalogStrings(left.ConfiguredProviders, right.ConfiguredProviders)
	left.AvailableProviders = mergeCatalogStrings(left.AvailableProviders, right.AvailableProviders)
	if left.PreferredProvider == "" || left.AvailableClients == right.AvailableClients {
		left.PreferredProvider = right.PreferredProvider
		left.Info = right.Info
	}
	return left
}

func mergeCatalogStrings(left, right []string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	merged := make([]string, 0, len(left)+len(right))
	for _, values := range [][]string{left, right} {
		for _, value := range values {
			key := strings.ToLower(strings.TrimSpace(value))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, value)
		}
	}
	return merged
}

func (h *Handler) refreshModelPricing(recalculate bool) bool {
	if h == nil || h.usagePersister == nil {
		return false
	}
	priceProvider := internalUsage.BuildModelPriceProvider(h.configFilePath)
	h.usagePersister.SetPriceProvider(priceProvider)
	if !recalculate || !h.usagePersister.HasPricing() {
		return false
	}
	if !h.usagePersister.StartRecalculateCosts() {
		log.Debug("model-catalog: historical cost recalculation already running")
	}
	return true
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

func (h *Handler) credentialExcludedCount(
	model modelcatalog.Model,
	aliases []string,
	runtimeProviders []string,
) int {
	if h == nil || h.authManager == nil {
		return 0
	}
	providers := make(map[string]struct{}, len(runtimeProviders)+1)
	for _, provider := range runtimeProviders {
		if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" && provider != "other" {
			providers[provider] = struct{}{}
		}
	}
	if channel := strings.ToLower(strings.TrimSpace(model.Channel)); channel != "" && channel != "other" {
		providers[channel] = struct{}{}
	}
	candidates := append([]string{model.Name}, aliases...)
	count := 0
	for _, auth := range h.authManager.List() {
		if auth == nil || auth.Disabled || !catalogCredentialProviderMatches(auth.Provider, providers) {
			continue
		}
		patterns := authFileExcludedModels(auth)
		if auth.Attributes != nil {
			patterns = append(patterns, strings.Split(auth.Attributes["excluded_models"], ",")...)
		}
		if catalogModelExcluded(patterns, candidates) {
			count++
		}
	}
	return count
}

func catalogCredentialProviderMatches(provider string, candidates map[string]struct{}) bool {
	if len(candidates) == 0 {
		return true
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if _, ok := candidates[provider]; ok {
		return true
	}
	equivalent := map[string][]string{
		"openai": {"codex"},
		"codex":  {"openai"},
		"xai":    {"grok"},
		"grok":   {"xai"},
	}
	for _, candidate := range equivalent[provider] {
		if _, ok := candidates[candidate]; ok {
			return true
		}
	}
	return false
}

func catalogModelExcluded(patterns, names []string) bool {
	for _, pattern := range patterns {
		for _, name := range names {
			if catalogWildcardMatch(pattern, name) {
				return true
			}
		}
	}
	return false
}

func catalogWildcardMatch(pattern, value string) bool {
	pattern = modelCatalogResponseKey(pattern)
	value = modelCatalogResponseKey(value)
	if pattern == "" || value == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	position := 0
	if first := parts[0]; first != "" {
		if !strings.HasPrefix(value, first) {
			return false
		}
		position = len(first)
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		index := strings.Index(value[position:], part)
		if index < 0 {
			return false
		}
		position += index + len(part)
	}
	last := parts[len(parts)-1]
	return last == "" || strings.HasSuffix(value[position:], last)
}

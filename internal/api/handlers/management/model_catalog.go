package management

import (
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
	Speed                     string           `json:"speed,omitempty"`
	SpeedOptions              []string         `json:"speed_options,omitempty"`
	RuntimeAvailable          bool             `json:"runtime_available"`
	RuntimeOnly               bool             `json:"runtime_only,omitempty"`
	RuntimeProviders          []string         `json:"runtime_providers,omitempty"`
	RuntimeAvailableProviders []string         `json:"runtime_available_providers,omitempty"`
	RuntimeConfiguredClients  int              `json:"runtime_configured_clients,omitempty"`
	RuntimeAvailableClients   int              `json:"runtime_available_clients,omitempty"`
	RuntimeQuotaCooldown      int              `json:"runtime_quota_cooldown_clients,omitempty"`
	RuntimeSuspended          int              `json:"runtime_suspended_clients,omitempty"`
	RuntimeAmbiguousAliases   []string         `json:"runtime_ambiguous_aliases,omitempty"`
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
	Speed            *string          `json:"speed"`
	Aliases          *[]string        `json:"aliases"`
	AliasChannel     *string          `json:"alias_channel"`
	Price            *modelPriceEntry `json:"price"`
	ClearPrice       *bool            `json:"clear_price"`
	RecalculateCosts *bool            `json:"recalculate_costs"`
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
			for i := range body.Models {
				model := &body.Models[i]
				if model.Price != nil {
					if err := validateModelPriceEntries(map[string]modelPriceEntry{model.Name: *model.Price}); err != nil {
						return newConfigMutationError(http.StatusBadRequest, err.Error())
					}
				}
				if model.Speed == "" {
					if previous, exists := doc.Find(model.Name); exists {
						model.Speed = previous.Speed
					}
				}
				if !config.ModelSpeedAllowed(model.Name, model.Channel, model.Speed) {
					return newConfigMutationError(http.StatusBadRequest, "unsupported model speed")
				}
			}
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
	if body.Price != nil {
		if err := validateModelPriceEntries(map[string]modelPriceEntry{body.Name: *body.Price}); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if body.AliasChannel != nil && body.Aliases == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "alias_channel requires aliases"})
		return
	}
	priceChanged := false
	result, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			priceRequested := body.Price != nil || body.ClearPrice != nil && *body.ClearPrice
			priceOnly := body.RecalculateCosts != nil && !*body.RecalculateCosts && priceRequested &&
				body.Provider == nil && body.Channel == nil && body.Group == nil && body.DisplayName == nil &&
				body.DisplayNameKebab == nil && body.Enabled == nil && body.Speed == nil && body.Aliases == nil
			if !priceOnly {
				model := doc.EnsureModel(body.Name, true)
				applyModelCatalogPatch(model, body)
				if !config.ModelSpeedAllowed(model.Name, model.Channel, model.Speed) {
					return newConfigMutationError(http.StatusBadRequest, "unsupported model speed")
				}
				if body.Aliases != nil {
					if err := modelcatalog.ReplaceModelAliases(
						doc, model.Name, *body.Aliases, body.AliasChannel,
					); err != nil {
						rejectMutation(mutationRequestContext(c))
						return newConfigMutationError(http.StatusBadRequest, err.Error())
					}
				}
			}
			if priceRequested {
				before, hadPrice := doc.Price(body.Name)
				if body.Price != nil {
					doc.SetPrice(body.Name, *body.Price)
				}
				if body.ClearPrice != nil && *body.ClearPrice {
					doc.DeletePrice(body.Name)
				}
				after, hasPrice := doc.Price(body.Name)
				priceChanged = hadPrice != hasPrice || (hadPrice && !before.Equal(after))
			}
			return nil
		},
	)
	if !ok {
		return
	}
	response := h.modelCatalogResponse(result.config, result.doc)
	response.RecalculationPending = h.refreshModelPricing(
		priceChanged &&
			(body.RecalculateCosts == nil || *body.RecalculateCosts),
	)
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
	preview, err := modelcatalog.PreviewDefaultUpdate(h.configFilePath, h.configSnapshot())
	if err != nil {
		log.Warnf("model-catalog: failed to build default update preview: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build default update preview"})
		return
	}
	c.Header("ETag", preview.Revision)
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
	ifMatch := strings.TrimSpace(c.GetHeader("If-Match"))
	checkRevision := func(doc modelcatalog.File) error {
		if ifMatch == "" {
			return nil
		}
		revision, err := modelcatalog.DefaultUpdateRevision(doc)
		if err != nil {
			return err
		}
		if ifMatch != revision {
			rejectMutation(mutationRequestContext(c))
			return newConfigMutationError(
				http.StatusPreconditionFailed, "model catalog changed; reload the default update preview",
			)
		}
		return nil
	}

	applied := 0
	applyResult := modelcatalog.ApplyResult{Status: "ok"}
	var catalog modelCatalogResponse
	if defaultUpdateKeepsCatalog(body.Decisions) {
		cfg := h.configSnapshot()
		doc, err := modelcatalog.LoadOrInit(h.configFilePath, cfg)
		if err == nil {
			err = checkRevision(doc)
		}
		if err != nil {
			respondModelCatalogMutationError(c, err)
			return
		}
		catalog = h.modelCatalogResponse(cfg, doc)
	} else {
		priceChanged := false
		result, ok := h.mutateModelCatalog(
			c,
			modelCatalogMutationOptions{backup: true},
			func(doc *modelcatalog.File) error {
				if err := checkRevision(*doc); err != nil {
					return err
				}
				prices := make(map[string]modelcatalog.Price, len(body.Decisions))
				for _, decision := range body.Decisions {
					if price, ok := doc.Price(decision.Name); ok {
						prices[decision.Name] = price
					}
				}
				var err error
				applied, err = modelcatalog.ApplyDefaultDecisions(doc, body.Decisions)
				if err != nil {
					return err
				}
				for _, decision := range body.Decisions {
					before, hadPrice := prices[decision.Name]
					after, hasPrice := doc.Price(decision.Name)
					if hadPrice != hasPrice || (hadPrice && !before.Equal(after)) {
						priceChanged = true
						break
					}
				}
				return nil
			},
		)
		if !ok {
			return
		}
		applyResult.BackupPath = result.backupPath
		applyResult.Applied = applied
		catalog = h.modelCatalogResponse(result.config, result.doc)
		catalog.RecalculationPending = h.refreshModelPricing(priceChanged)
	}
	c.JSON(
		http.StatusOK,
		gin.H{
			"result":  applyResult,
			"catalog": catalog,
		},
	)
}

func defaultUpdateKeepsCatalog(decisions []modelcatalog.ApplyDecision) bool {
	for _, decision := range decisions {
		if strings.TrimSpace(strings.TrimPrefix(decision.Name, "models/")) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(decision.Action)) {
		case "keep_current", "skip", "":
		default:
			return false
		}
	}
	return true
}

func applyModelCatalogPatch(model *modelcatalog.Model, body modelCatalogPatchRequest) {
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
	if body.Speed != nil {
		model.Speed = strings.ToLower(strings.TrimSpace(*body.Speed))
	}
}

func (h *Handler) modelCatalogResponse(
	cfg *config.Config,
	doc modelcatalog.File,
) modelCatalogResponse {
	doc.Models = append([]modelcatalog.Model(nil), doc.Models...)
	aliasesByModel := make(map[string]map[string]string, len(doc.Models))
	aliasScopesByModel := make(map[string]map[string][]string)
	catalogNames := make(map[string]struct{}, len(doc.Models))
	for _, model := range doc.Models {
		name := modelCatalogResponseKey(model.Name)
		catalogNames[name] = struct{}{}
		aliasesByModel[name] = make(map[string]string)
		for _, alias := range model.Aliases {
			aliasesByModel[name][modelCatalogResponseKey(alias)] = alias
		}
	}
	ownersByAlias := make(map[string]map[string]string)
	oauthAliases := doc.ToOAuthModelAlias()
	for _, channel := range slices.Sorted(maps.Keys(oauthAliases)) {
		for _, entry := range oauthAliases[channel] {
			name, alias := modelCatalogResponseKey(entry.Name), modelCatalogResponseKey(entry.Alias)
			if _, exists := catalogNames[name]; !exists || alias == "" || alias == name {
				continue
			}
			aliasesByModel[name][alias] = entry.Alias
			if aliasScopesByModel[name] == nil {
				aliasScopesByModel[name] = make(map[string][]string)
			}
			aliasScopesByModel[name][channel] = append(aliasScopesByModel[name][channel], entry.Alias)
			if ownersByAlias[alias] == nil {
				ownersByAlias[alias] = make(map[string]string)
			}
			ownersByAlias[alias][channel] = name
		}
	}
	runtimeModels := projectCatalogRuntime(&doc, catalogNames, ownersByAlias)

	rows := make([]modelCatalogRow, 0, len(doc.Models))
	for _, model := range doc.Models {
		nameKey := modelCatalogResponseKey(model.Name)
		runtime := runtimeModels[nameKey]
		availability := runtime.availability()
		runtimeAvailable := availability.AvailableClients > 0
		apiKeyBlocked := apiKeyBlockedCount(cfg, model.Name)
		credentialExcluded := h.credentialExcludedCount(
			model, aliasScopesByModel[nameKey], availability.ConfiguredProviders,
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
		case !runtimeAvailable && len(runtime.ambiguousAliases) > 0:
			reason = "ambiguous_alias"
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
			Speed:                     model.Speed,
			SpeedOptions:              config.ModelSpeedOptions(model.Name, model.Channel),
			RuntimeAvailable:          runtimeAvailable,
			RuntimeOnly:               runtime.runtimeOnly,
			RuntimeProviders:          availability.ConfiguredProviders,
			RuntimeAvailableProviders: availability.AvailableProviders,
			RuntimeConfiguredClients:  availability.ConfiguredClients,
			RuntimeAvailableClients:   availability.AvailableClients,
			RuntimeQuotaCooldown:      availability.QuotaCooldownClients,
			RuntimeSuspended:          availability.SuspendedClients,
			RuntimeAmbiguousAliases:   slices.Sorted(maps.Keys(runtime.ambiguousAliases)),
			Requestable:               requestable,
			NotRequestableReason:      reason,
			Aliases:                   slices.Sorted(maps.Values(aliasesByModel[nameKey])),
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

type catalogRuntimeModel struct {
	clients          map[string]registry.ModelAvailabilityClient
	ambiguousAliases map[string]struct{}
	runtimeOnly      bool
}

func projectCatalogRuntime(
	doc *modelcatalog.File, catalogNames map[string]struct{}, ownersByAlias map[string]map[string]string,
) map[string]catalogRuntimeModel {
	models := make(map[string]catalogRuntimeModel)
	addClient := func(name string, client registry.ModelAvailabilityClient, runtimeOnly bool) {
		model := models[name]
		if model.clients == nil {
			model.clients = make(map[string]registry.ModelAvailabilityClient)
		}
		// A fork can register both the original and aliases on the same credential.
		if previous, exists := model.clients[client.ID]; !exists || previous.State < client.State {
			model.clients[client.ID] = client
		}
		model.runtimeOnly = runtimeOnly
		models[name] = model
	}
	markAmbiguous := func(name, alias string) {
		model := models[name]
		if model.ambiguousAliases == nil {
			model.ambiguousAliases = make(map[string]struct{})
		}
		model.ambiguousAliases[alias] = struct{}{}
		models[name] = model
	}
	for _, availability := range registry.GetGlobalRegistry().GetModelAvailabilitySnapshot() {
		public := modelCatalogResponseKey(availability.ModelID)
		owners := ownersByAlias[public]
		_, direct := catalogNames[public]
		var discovered *modelcatalog.Model
		for _, client := range availability.Clients {
			registeredName := modelCatalogResponseKey(client.CatalogName)
			if _, known := catalogNames[registeredName]; registeredName != "" && known {
				addClient(registeredName, client, false)
				continue
			}
			owner := owners[client.Provider]
			switch {
			case registeredName != "" && direct:
				markAmbiguous(public, availability.ModelID)
				continue
			case registeredName == "" && owner != "" && !direct:
				addClient(owner, client, false)
				continue
			case registeredName == "" && direct && owner == "" && (client.Provider != "" || len(owners) == 0):
				addClient(public, client, false)
				continue
			case registeredName == "" && len(owners) > 0:
				if direct {
					markAmbiguous(public, availability.ModelID)
				}
				if owner != "" {
					markAmbiguous(owner, availability.ModelID)
				} else {
					for _, possible := range owners {
						markAmbiguous(possible, availability.ModelID)
					}
				}
				continue
			}
			if discovered == nil {
				displayName := ""
				if availability.Info != nil {
					displayName = availability.Info.DisplayName
				}
				discovered = doc.EnsureRuntimeModel(availability.ModelID, availability.PreferredProvider, displayName)
			}
			if discovered != nil {
				addClient(modelCatalogResponseKey(discovered.Name), client, true)
			}
		}
	}
	return models
}

func (r catalogRuntimeModel) availability() registry.ModelAvailability {
	availability := registry.ModelAvailability{ConfiguredClients: len(r.clients)}
	providers, availableProviders := make(map[string]struct{}), make(map[string]struct{})
	for _, client := range r.clients {
		if client.Provider != "" {
			providers[client.Provider] = struct{}{}
		}
		switch client.State {
		case registry.ModelClientAvailable:
			availability.AvailableClients++
			if client.Provider != "" {
				availableProviders[client.Provider] = struct{}{}
			}
		case registry.ModelClientQuotaCooldown:
			availability.QuotaCooldownClients++
		case registry.ModelClientSuspended:
			availability.SuspendedClients++
		}
	}
	availability.ConfiguredProviders = slices.Sorted(maps.Keys(providers))
	availability.AvailableProviders = slices.Sorted(maps.Keys(availableProviders))
	return availability
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
	aliasesByChannel map[string][]string,
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
	for channel := range aliasesByChannel {
		providers[channel] = struct{}{}
	}
	count := 0
	for _, auth := range h.authManager.List() {
		if auth == nil || auth.Disabled || !catalogCredentialProviderMatches(auth.Provider, providers) {
			continue
		}
		channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
		candidates := append([]string{model.Name}, aliasesByChannel[channel]...)
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

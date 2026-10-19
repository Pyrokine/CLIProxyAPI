package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type pluginListResponse struct {
	PluginsEnabled bool              `json:"plugins_enabled"`
	PluginsDir     string            `json:"plugins_dir"`
	Plugins        []pluginListEntry `json:"plugins"`
}

type pluginListEntry struct {
	ID               string                  `json:"id"`
	Path             string                  `json:"path"`
	Configured       bool                    `json:"configured"`
	Registered       bool                    `json:"registered"`
	Enabled          bool                    `json:"enabled"`
	EffectiveEnabled bool                    `json:"effective_enabled"`
	SupportsOAuth    bool                    `json:"supports_oauth"`
	OAuthProvider    string                  `json:"oauth_provider"`
	SupportsQuota    bool                    `json:"supports_quota"`
	QuotaProvider    string                  `json:"quota_provider,omitempty"`
	Logo             string                  `json:"logo"`
	Config           map[string]any          `json:"config"`
	ConfigFields     []pluginConfigFieldInfo `json:"config_fields"`
	Menus            []pluginMenuInfo        `json:"menus"`
	Metadata         *pluginMetadataInfo     `json:"metadata"`
}

type pluginMetadataInfo struct {
	Name             string                  `json:"name"`
	Version          string                  `json:"version"`
	Author           string                  `json:"author"`
	GitHubRepository string                  `json:"github_repository"`
	Logo             string                  `json:"logo"`
	ConfigFields     []pluginConfigFieldInfo `json:"config_fields"`
}

type pluginConfigFieldInfo struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	EnumValues  []string `json:"enum_values"`
	Description string   `json:"description"`
}

type pluginMenuInfo struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

// ListPlugins returns discovered, configured, and registered plugin entries.
func (h *Handler) ListPlugins(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(
			http.StatusOK, pluginListResponse{
				PluginsDir: "plugins",
				Plugins:    []pluginListEntry{},
			},
		)
		return
	}

	h.mu.Lock()
	pluginsEnabled := h.cfg.Plugins.Enabled
	pluginsDir := h.cfg.Plugins.Dir
	configs := make(map[string]config.PluginInstanceConfig, len(h.cfg.Plugins.Configs))
	for id, item := range h.cfg.Plugins.Configs {
		configs[pluginstore.CanonicalPluginID(id)] = item
	}
	host := h.pluginHost
	h.mu.Unlock()

	quotaProvidersByPlugin := make(map[string]pluginhost.RegisteredQuotaProviderInfo)
	if quotaHost, okQuotaHost := host.(quotaPluginHost); okQuotaHost && quotaHost != nil {
		for _, info := range quotaHost.QuotaProviders(c.Request.Context()) {
			pluginID := pluginstore.CanonicalPluginID(info.PluginID)
			if pluginID != "" {
				quotaProvidersByPlugin[pluginID] = info
			}
		}
	}

	resolvedPluginsDir, errResolvePluginsDir := config.ResolvePluginsDir(pluginsDir)
	if errResolvePluginsDir != nil {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "plugin_directory_invalid", "message": errResolvePluginsDir.Error()},
		)
		return
	}
	pluginsDir = resolvedPluginsDir
	entries := make(map[string]pluginListEntry)
	files, errDiscover := pluginhost.DiscoverPluginFiles(pluginsDir)
	if errDiscover != nil {
		c.JSON(
			http.StatusInternalServerError, gin.H{"error": "plugin_discovery_failed", "message": errDiscover.Error()},
		)
		return
	}
	for _, file := range files {
		entries[file.ID] = pluginListEntry{
			ID:           file.ID,
			Path:         file.Path,
			Enabled:      true,
			Config:       map[string]any{},
			ConfigFields: []pluginConfigFieldInfo{},
			Menus:        []pluginMenuInfo{},
		}
	}
	for id, item := range configs {
		entry := entries[id]
		entry.ID = id
		entry.Configured = true
		entry.Enabled = pluginInstanceEnabled(item)
		entry.Config = pluginConfigObject(item, nil)
		if entry.ConfigFields == nil {
			entry.ConfigFields = []pluginConfigFieldInfo{}
		}
		if entry.Menus == nil {
			entry.Menus = []pluginMenuInfo{}
		}
		entries[id] = entry
	}
	if host != nil {
		for _, info := range host.RegisteredPlugins() {
			id := pluginstore.CanonicalPluginID(info.ID)
			entry := entries[id]
			entry.ID = id
			entry.Registered = true
			entry.SupportsOAuth = info.SupportsOAuth
			entry.OAuthProvider = info.OAuthProvider
			if quotaInfo, supportsQuota := quotaProvidersByPlugin[id]; supportsQuota {
				entry.SupportsQuota = true
				entry.QuotaProvider = quotaInfo.Provider
			}
			entry.Logo = info.Metadata.Logo
			entry.ConfigFields = pluginConfigFields(info.Metadata.ConfigFields)
			_, configuredItem, configured := pluginConfigByID(configs, id)
			if configured {
				entry.Config = pluginConfigObject(configuredItem, info.Metadata.ConfigFields)
			}
			entry.Menus = pluginMenus(info.Menus)
			entry.Metadata = pluginMetadata(info.Metadata)
			if !configured && !entry.Enabled {
				entry.Enabled = true
			}
			entries[id] = entry
		}
	}

	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]pluginListEntry, 0, len(ids))
	for _, id := range ids {
		entry := entries[id]
		entry.EffectiveEnabled = pluginsEnabled && entry.Enabled && entry.Registered
		if entry.Config == nil {
			entry.Config = map[string]any{}
		}
		if entry.ConfigFields == nil {
			entry.ConfigFields = []pluginConfigFieldInfo{}
		}
		if entry.Menus == nil {
			entry.Menus = []pluginMenuInfo{}
		}
		out = append(out, entry)
	}

	c.JSON(
		http.StatusOK, pluginListResponse{
			PluginsEnabled: pluginsEnabled,
			PluginsDir:     pluginsDir,
			Plugins:        out,
		},
	)
}

// PatchPluginEnabled updates plugins.configs.<id>.enabled without touching plugins.enabled.
func (h *Handler) PatchPluginEnabled(c *gin.Context) {
	id, okID := pluginIDFromRequest(c)
	if !okID {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "enabled is required"})
		return
	}

	if !h.mutatePluginConfig(
		c, func(next *config.Config) error {
			ensurePluginConfigMap(next)
			_, item, _ := pluginConfigByID(next.Plugins.Configs, id)
			node := pluginConfigNode(item)
			setYAMLMappingValue(node, "enabled", boolYAMLNode(*body.Enabled))
			updated, errConfig := pluginInstanceConfigFromNode(node)
			if errConfig != nil {
				return newConfigMutationError(
					http.StatusBadRequest,
					fmt.Sprintf("invalid plugin config: %v", errConfig),
				)
			}
			setPluginConfigByID(next.Plugins.Configs, id, updated)
			return nil
		},
	) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PutPluginConfig replaces plugins.configs.<id> with the request object.
func (h *Handler) PutPluginConfig(c *gin.Context) {
	id, okID := pluginIDFromRequest(c)
	if !okID {
		return
	}
	body, okBody := readPluginConfigObject(c)
	if !okBody {
		return
	}
	if _, hasStore := body["store"]; hasStore {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "reserved_plugin_config_field", "message": "store is managed by the plugin store"},
		)
		return
	}
	node, errNode := yamlNodeFromJSONObject(body)
	if errNode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": errNode.Error()})
		return
	}
	updated, errConfig := pluginInstanceConfigFromNode(node)
	if errConfig != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_config", "message": errConfig.Error()})
		return
	}

	if !h.mutatePluginConfig(
		c, func(next *config.Config) error {
			ensurePluginConfigMap(next)
			candidate := updated
			_, current, _ := pluginConfigByID(next.Plugins.Configs, id)
			if storeNode := pluginStoreConfigNode(current); storeNode != nil {
				node := pluginConfigNode(candidate)
				setYAMLMappingValue(node, "store", cloneYAMLNode(storeNode))
				preserved, errPreserve := pluginInstanceConfigFromNode(node)
				if errPreserve != nil {
					return newConfigMutationError(
						http.StatusInternalServerError,
						fmt.Sprintf("preserve plugin store metadata: %v", errPreserve),
					)
				}
				candidate = preserved
			}
			setPluginConfigByID(next.Plugins.Configs, id, candidate)
			return nil
		},
	) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PatchPluginConfig shallow-merges plugins.configs.<id> with the request object.
func (h *Handler) PatchPluginConfig(c *gin.Context) {
	id, okID := pluginIDFromRequest(c)
	if !okID {
		return
	}
	body, okBody := readPluginConfigObject(c)
	if !okBody {
		return
	}
	if _, hasStore := body["store"]; hasStore {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "reserved_plugin_config_field", "message": "store is managed by the plugin store"},
		)
		return
	}

	if !h.mutatePluginConfig(
		c, func(next *config.Config) error {
			ensurePluginConfigMap(next)
			_, current, _ := pluginConfigByID(next.Plugins.Configs, id)
			node := pluginConfigNode(current)
			keys := make([]string, 0, len(body))
			for key := range body {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				value := body[key]
				if value == nil {
					deleteYAMLMappingKey(node, key)
					continue
				}
				valueNode, errNode := yamlNodeFromJSONValue(value)
				if errNode != nil {
					return newConfigMutationError(
						http.StatusBadRequest,
						fmt.Sprintf("invalid plugin config field %q: %v", key, errNode),
					)
				}
				setYAMLMappingValue(node, key, valueNode)
			}
			updated, errConfig := pluginInstanceConfigFromNode(node)
			if errConfig != nil {
				return newConfigMutationError(
					http.StatusBadRequest,
					fmt.Sprintf("invalid plugin config: %v", errConfig),
				)
			}
			setPluginConfigByID(next.Plugins.Configs, id, updated)
			return nil
		},
	) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) mutatePluginConfig(
	c *gin.Context,
	mutate func(*config.Config) error,
) bool {
	if c == nil {
		return false
	}
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return false
	}

	h.commitMu.Lock()
	h.mu.Lock()
	commit, callback, ok := h.mutateConfigLocked(c, mutate)
	h.mu.Unlock()
	h.commitMu.Unlock()
	if !ok {
		return false
	}
	if !h.notifyConfigCommitted(c, callback, commit) {
		return false
	}
	if callback != nil {
		return true
	}

	ctx := context.Background()
	if c.Request != nil {
		ctx = context.WithoutCancel(c.Request.Context())
	}
	if err := h.runPluginRuntimeSyncForCommit(ctx, commit, h.pluginRuntimeSync); err != nil {
		log.WithError(err).Error("plugin config committed but runtime sync failed")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin_runtime_sync_failed"})
		return false
	}
	return true
}

func runPluginRuntimeSync(
	ctx context.Context,
	syncFn func(context.Context, *config.Config) error,
	cfg *config.Config,
) (err error) {
	if syncFn == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("plugin runtime sync panicked: %v", recovered)
		}
	}()
	return syncFn(ctx, cfg)
}

func pluginRuntimeConfigSnapshot(cfg *config.Config) *config.Config {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Plugins.Configs = make(map[string]config.PluginInstanceConfig, len(cfg.Plugins.Configs))
	for id, item := range cfg.Plugins.Configs {
		copied := item
		if item.Enabled != nil {
			enabled := *item.Enabled
			copied.Enabled = &enabled
		}
		copied.Raw = *cloneYAMLNode(&item.Raw)
		out.Plugins.Configs[id] = copied
	}
	return &out
}

func pluginInstanceEnabled(item config.PluginInstanceConfig) bool {
	if item.Enabled == nil {
		return true
	}
	return *item.Enabled
}

func pluginConfigObject(item config.PluginInstanceConfig, fields []pluginapi.ConfigField) map[string]any {
	out := map[string]any{}
	if item.Enabled != nil {
		out["enabled"] = *item.Enabled
	}
	if item.Priority != 0 {
		out["priority"] = item.Priority
	}
	if len(fields) == 0 {
		return out
	}
	node := pluginConfigNode(item)
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		if name == "" {
			continue
		}
		valueNode := yamlMappingValue(node, name)
		if valueNode == nil {
			continue
		}
		value, ok := yamlNodeJSONValue(valueNode)
		if ok {
			out[name] = value
		}
	}
	return out
}

func yamlNodeJSONValue(node *yaml.Node) (any, bool) {
	var value any
	if node == nil || node.Decode(&value) != nil {
		return nil, false
	}
	return normalizeJSONValue(value), true
}

func normalizeJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeJSONValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[fmt.Sprint(key)] = normalizeJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, normalizeJSONValue(item))
		}
		return out
	default:
		return typed
	}
}

func pluginConfigFields(fields []pluginapi.ConfigField) []pluginConfigFieldInfo {
	out := make([]pluginConfigFieldInfo, 0, len(fields))
	for _, field := range fields {
		enumValues := append([]string{}, field.EnumValues...)
		out = append(
			out, pluginConfigFieldInfo{
				Name:        field.Name,
				Type:        string(field.Type),
				EnumValues:  enumValues,
				Description: field.Description,
			},
		)
	}
	return out
}

func pluginMenus(menus []pluginhost.RegisteredPluginMenu) []pluginMenuInfo {
	out := make([]pluginMenuInfo, 0, len(menus))
	for _, menu := range menus {
		out = append(
			out, pluginMenuInfo{
				Path:        menu.Path,
				Menu:        menu.Menu,
				Description: menu.Description,
			},
		)
	}
	return out
}

func pluginMetadata(meta pluginapi.Metadata) *pluginMetadataInfo {
	return &pluginMetadataInfo{
		Name:             meta.Name,
		Version:          meta.Version,
		Author:           meta.Author,
		GitHubRepository: meta.GitHubRepository,
		Logo:             meta.Logo,
		ConfigFields:     pluginConfigFields(meta.ConfigFields),
	}
}

func pluginIDFromRequest(c *gin.Context) (string, bool) {
	id := pluginstore.CanonicalPluginID(c.Param("id"))
	if !pluginhost.ValidatePluginID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_plugin_id", "message": "invalid plugin id"})
		return "", false
	}
	return id, true
}

func readPluginConfigObject(c *gin.Context) (map[string]any, bool) {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	var body map[string]any
	if errDecode := decoder.Decode(&body); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": errDecode.Error()})
		return nil, false
	}
	if body == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "body must be a JSON object"})
		return nil, false
	}
	return body, true
}

func pluginConfigByID(
	configs map[string]config.PluginInstanceConfig,
	id string,
) (string, config.PluginInstanceConfig, bool) {
	canonicalID := pluginstore.CanonicalPluginID(id)
	if item, exists := configs[canonicalID]; exists {
		return canonicalID, item, true
	}
	for configuredID, item := range configs {
		if pluginstore.CanonicalPluginID(configuredID) == canonicalID {
			return configuredID, item, true
		}
	}
	return canonicalID, config.PluginInstanceConfig{}, false
}

func setPluginConfigByID(
	configs map[string]config.PluginInstanceConfig,
	id string,
	item config.PluginInstanceConfig,
) {
	canonicalID := pluginstore.CanonicalPluginID(id)
	for configuredID := range configs {
		if configuredID != canonicalID && pluginstore.CanonicalPluginID(configuredID) == canonicalID {
			delete(configs, configuredID)
		}
	}
	configs[canonicalID] = item
}

func deletePluginConfigByID(configs map[string]config.PluginInstanceConfig, id string) {
	canonicalID := pluginstore.CanonicalPluginID(id)
	for configuredID := range configs {
		if pluginstore.CanonicalPluginID(configuredID) == canonicalID {
			delete(configs, configuredID)
		}
	}
}

func ensurePluginConfigMap(cfg *config.Config) {
	if cfg == nil {
		return
	}
	cfg.NormalizePluginsConfig()
}

func pluginConfigNode(item config.PluginInstanceConfig) *yaml.Node {
	if item.Raw.Kind == yaml.MappingNode {
		return cloneYAMLNode(&item.Raw)
	}
	node := emptyYAMLMappingNode()
	if item.Enabled != nil {
		setYAMLMappingValue(node, "enabled", boolYAMLNode(*item.Enabled))
	}
	if item.Priority != 0 {
		setYAMLMappingValue(node, "priority", intYAMLNode(item.Priority))
	}
	return node
}

func pluginInstanceConfigFromNode(node *yaml.Node) (config.PluginInstanceConfig, error) {
	if node == nil {
		node = emptyYAMLMappingNode()
	}
	var item config.PluginInstanceConfig
	if errDecode := node.Decode(&item); errDecode != nil {
		return config.PluginInstanceConfig{}, errDecode
	}
	return item, nil
}

func yamlNodeFromJSONObject(body map[string]any) (*yaml.Node, error) {
	node := emptyYAMLMappingNode()
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		valueNode, errNode := yamlNodeFromJSONValue(body[key])
		if errNode != nil {
			return nil, fmt.Errorf("%s: %w", key, errNode)
		}
		setYAMLMappingValue(node, key, valueNode)
	}
	return node, nil
}

func yamlNodeFromJSONValue(value any) (*yaml.Node, error) {
	switch typed := value.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: typed}, nil
	case bool:
		return boolYAMLNode(typed), nil
	case json.Number:
		if _, errInt64 := typed.Int64(); errInt64 == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: typed.String()}, nil
		}
		if _, errFloat64 := typed.Float64(); errFloat64 == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: typed.String()}, nil
		}
		return nil, fmt.Errorf("invalid number %q", typed.String())
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(typed, 'f', -1, 64)}, nil
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range typed {
			child, errChild := yamlNodeFromJSONValue(item)
			if errChild != nil {
				return nil, errChild
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	case map[string]any:
		return yamlNodeFromJSONObject(typed)
	default:
		return nil, fmt.Errorf("unsupported value type %T", value)
	}
}

func emptyYAMLMappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func boolYAMLNode(value bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(value)}
}

func intYAMLNode(value int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)}
}

func setYAMLMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping.Kind != yaml.MappingNode {
		*mapping = *emptyYAMLMappingNode()
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index] != nil && mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func deleteYAMLMappingKey(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index] != nil && mapping.Content[index].Value == key {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return
		}
	}
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index] != nil && mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	out := *node
	if len(node.Content) > 0 {
		out.Content = make([]*yaml.Node, 0, len(node.Content))
		for _, child := range node.Content {
			out.Content = append(out.Content, cloneYAMLNode(child))
		}
	}
	return &out
}

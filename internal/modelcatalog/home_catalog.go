package modelcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

// HomeModelRoute keeps one model entry bound to the Home execution route that supplied it.
type HomeModelRoute struct {
	Section            string
	Provider           string
	Model              map[string]any
	NativeCapabilities *registry.NativeCapabilities
}

// HomeCatalog keeps Home routes separate until a response builder resolves their shared public model ID.
type HomeCatalog struct {
	routes []HomeModelRoute
}

// DecodeHomeCatalog parses a Home model payload without merging same-ID routes.
func DecodeHomeCatalog(raw []byte) (*HomeCatalog, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("home models payload is empty")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var sections map[string][]map[string]any
	if err := decoder.Decode(&sections); err != nil {
		return nil, fmt.Errorf("parse home models payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse home models payload: multiple JSON values")
		}
		return nil, fmt.Errorf("parse home models payload: %w", err)
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("home models payload has no sections")
	}

	sectionNames := make([]string, 0, len(sections))
	for section := range sections {
		sectionNames = append(sectionNames, section)
	}
	sort.Strings(sectionNames)

	routes := make([]HomeModelRoute, 0, 256)
	for _, section := range sectionNames {
		provider := homeRouteProvider(section)
		for _, rawModel := range sections[section] {
			route, ok, err := buildHomeModelRoute(section, provider, rawModel)
			if err != nil {
				return nil, fmt.Errorf("parse home models section %q: %w", section, err)
			}
			if ok {
				routes = append(routes, route)
			}
		}
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("home models payload contains no models")
	}

	sort.SliceStable(routes, func(i, j int) bool {
		leftID := homeStringValue(routes[i].Model, "id")
		rightID := homeStringValue(routes[j].Model, "id")
		if leftID != rightID {
			return leftID < rightID
		}
		if routes[i].Provider != routes[j].Provider {
			return routes[i].Provider < routes[j].Provider
		}
		return routes[i].Section < routes[j].Section
	})
	return &HomeCatalog{routes: routes}, nil
}

// Routes returns a deep copy of every Home execution route.
func (c *HomeCatalog) Routes() []HomeModelRoute {
	if c == nil || len(c.routes) == 0 {
		return nil
	}
	routes := make([]HomeModelRoute, 0, len(c.routes))
	for _, route := range c.routes {
		routes = append(routes, cloneHomeModelRoute(route))
	}
	return routes
}

// Models returns route-bound model metadata. Same-ID routes remain separate.
func (c *HomeCatalog) Models() []map[string]any {
	if c == nil || len(c.routes) == 0 {
		return nil
	}
	models := make([]map[string]any, 0, len(c.routes))
	for _, route := range c.routes {
		models = append(models, cloneHomeModelMap(route.Model))
	}
	return models
}

// ResponsesWebSearchCapability resolves native search across every Home route for modelID.
func (c *HomeCatalog) ResponsesWebSearchCapability(modelID string) *bool {
	modelID = strings.TrimSpace(modelID)
	if c == nil || modelID == "" {
		return nil
	}

	routes := make([]registry.NativeCapabilityRoute, 0)
	for _, route := range c.routes {
		if homeStringValue(route.Model, "id") != modelID {
			continue
		}
		routes = append(routes, registry.NativeCapabilityRoute{
			Provider:           route.Provider,
			NativeCapabilities: cloneHomeNativeCapabilities(route.NativeCapabilities),
		})
	}
	return registry.ResolveResponsesWebSearchCapability(routes)
}

func buildHomeModelRoute(section, provider string, rawModel map[string]any) (HomeModelRoute, bool, error) {
	model := cloneHomeModelMap(rawModel)
	id := homeStringValue(model, "id")
	if id == "" {
		id = strings.TrimPrefix(homeStringValue(model, "name"), "models/")
	}
	if id == "" {
		return HomeModelRoute{}, false, nil
	}
	model["id"] = id

	catalogID := homeStringValue(model, "catalog_name")
	if catalogID == "" {
		catalogID = homeStringValue(model, "catalogName")
	}
	if catalogID == "" {
		catalogID = id
	}
	staticInfo := lookupHomeStaticModelInfo(catalogID, provider)

	contextLength := homeIntValue(model, "context_length", "contextLength", "inputTokenLimit", "max_input_tokens")
	if contextLength <= 0 && staticInfo != nil {
		contextLength = staticInfo.ContextLength
	}
	if contextLength > 0 {
		model["context_length"] = contextLength
	}
	maxContextLength := homeIntValue(model, "max_context_length", "maxContextLength")
	if maxContextLength <= 0 && staticInfo != nil {
		maxContextLength = staticInfo.MaxContextLength
	}
	if maxContextLength > 0 {
		model["max_context_length"] = maxContextLength
	}
	maxCompletionTokens := homeIntValue(model, "max_completion_tokens", "max_tokens", "outputTokenLimit")
	if maxCompletionTokens <= 0 && staticInfo != nil {
		maxCompletionTokens = staticInfo.MaxCompletionTokens
	}
	if maxCompletionTokens > 0 {
		model["max_completion_tokens"] = maxCompletionTokens
	}

	if _, present := model["thinking"]; !present && staticInfo != nil && staticInfo.Thinking != nil {
		model["thinking"] = cloneHomeThinkingSupport(staticInfo.Thinking)
	}
	if _, present := model["supportedInputModalities"]; !present {
		_, alternate := model["input_modalities"]
		if !alternate && staticInfo != nil && len(staticInfo.SupportedInputModalities) > 0 {
			model["supportedInputModalities"] = append([]string(nil), staticInfo.SupportedInputModalities...)
		}
	}
	if _, present := model["supportedOutputModalities"]; !present && staticInfo != nil &&
		len(staticInfo.SupportedOutputModalities) > 0 {
		model["supportedOutputModalities"] = append([]string(nil), staticInfo.SupportedOutputModalities...)
	}
	applyHomeStaticStrings(model, staticInfo)

	nativeCapabilities, explicit, err := homeNativeCapabilities(rawModel)
	if err != nil {
		return HomeModelRoute{}, false, fmt.Errorf("model %q native capabilities: %w", id, err)
	}
	if !explicit && staticInfo != nil {
		nativeCapabilities = cloneHomeNativeCapabilities(staticInfo.NativeCapabilities)
	}
	delete(model, "native_capabilities")
	delete(model, "catalog_name")
	delete(model, "catalogName")

	return HomeModelRoute{
		Section:            strings.TrimSpace(section),
		Provider:           provider,
		Model:              model,
		NativeCapabilities: nativeCapabilities,
	}, true, nil
}

func lookupHomeStaticModelInfo(modelID, provider string) *registry.ModelInfo {
	if info := registry.LookupStaticModelInfoByChannel(modelID, provider); info != nil {
		return info
	}
	if index := strings.Index(modelID, "/"); index != -1 {
		return registry.LookupStaticModelInfoByChannel(strings.TrimSpace(modelID[index+1:]), provider)
	}
	return nil
}

func applyHomeStaticStrings(model map[string]any, info *registry.ModelInfo) {
	if info == nil {
		return
	}
	for _, field := range []struct {
		key   string
		value string
	}{
		{key: "object", value: info.Object},
		{key: "owned_by", value: info.OwnedBy},
		{key: "type", value: info.Type},
		{key: "display_name", value: info.DisplayName},
		{key: "version", value: info.Version},
		{key: "description", value: info.Description},
	} {
		if homeStringValue(model, field.key) == "" && strings.TrimSpace(field.value) != "" {
			model[field.key] = field.value
		}
	}
	if homeIntValue(model, "created") <= 0 && info.Created > 0 {
		model["created"] = info.Created
	}
}

func homeRouteProvider(section string) string {
	section = strings.ToLower(strings.TrimSpace(section))
	switch {
	case strings.HasPrefix(section, "codex-"):
		return "codex"
	case section == "anthropic":
		return "claude"
	case section == "grok" || section == "x-ai":
		return "xai"
	case section == "moonshot":
		return "kimi"
	case section == "gemini-cli":
		return "gemini"
	default:
		return section
	}
}

func homeNativeCapabilities(model map[string]any) (*registry.NativeCapabilities, bool, error) {
	raw, present := model["native_capabilities"]
	if !present {
		return nil, false, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, true, err
	}
	var capabilities registry.NativeCapabilities
	if err = json.Unmarshal(data, &capabilities); err != nil {
		return nil, true, err
	}
	return &capabilities, true, nil
}

func homeStringValue(model map[string]any, key string) string {
	value, _ := model[key].(string)
	return strings.TrimSpace(value)
}

func homeIntValue(model map[string]any, keys ...string) int {
	for _, key := range keys {
		value := 0
		switch typed := model[key].(type) {
		case int:
			value = typed
		case int64:
			value = int(typed)
		case float64:
			value = int(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				value = int(parsed)
			}
		}
		if value > 0 {
			return value
		}
	}
	return 0
}

func cloneHomeModelRoute(route HomeModelRoute) HomeModelRoute {
	return HomeModelRoute{
		Section:            route.Section,
		Provider:           route.Provider,
		Model:              cloneHomeModelMap(route.Model),
		NativeCapabilities: cloneHomeNativeCapabilities(route.NativeCapabilities),
	}
}

func cloneHomeModelMap(model map[string]any) map[string]any {
	if model == nil {
		return nil
	}
	cloned := make(map[string]any, len(model))
	for key, value := range model {
		cloned[key] = cloneHomeModelValue(value)
	}
	return cloned
}

func cloneHomeModelValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneHomeModelMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneHomeModelValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	case *registry.ThinkingSupport:
		return cloneHomeThinkingSupport(typed)
	default:
		return value
	}
}

func cloneHomeThinkingSupport(thinking *registry.ThinkingSupport) *registry.ThinkingSupport {
	if thinking == nil {
		return nil
	}
	cloned := *thinking
	cloned.Levels = append([]string(nil), thinking.Levels...)
	return &cloned
}

func cloneHomeNativeCapabilities(capabilities *registry.NativeCapabilities) *registry.NativeCapabilities {
	if capabilities == nil {
		return nil
	}
	cloned := *capabilities
	if capabilities.WebSearch != nil {
		webSearch := *capabilities.WebSearch
		cloned.WebSearch = &webSearch
	}
	return &cloned
}

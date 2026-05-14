package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

// Patch field helpers — apply optional pointer fields to entry fields.

// setTrimmed sets *dst = TrimSpace(**src) if src is non-nil.
func setTrimmed(src *string, dst *string) {
	if src != nil {
		*dst = strings.TrimSpace(*src)
	}
}

// setRequiredTrimmed is like setTrimmed but returns errEntryDeleted when the trimmed value is empty.
func setRequiredTrimmed(src *string, dst *string) error {
	if src == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*src)
	if trimmed == "" {
		return errEntryDeleted
	}
	*dst = trimmed
	return nil
}

// setHeaders applies NormalizeHeaders to *dst when src is non-nil.
func setHeaders(src *map[string]string, dst *map[string]string) {
	if src != nil {
		*dst = config.NormalizeHeaders(*src)
	}
}

// setExcludedModels applies NormalizeExcludedModels to *dst when src is non-nil.
func setExcludedModels(src *[]string, dst *[]string) {
	if src != nil {
		*dst = config.NormalizeExcludedModels(*src)
	}
}

// deleteMapKey removes key from *m, nils out the map if empty, and returns true.
// If the key doesn't exist, it writes a 404 to c with label and returns false.
func deleteMapKey[V any](c *gin.Context, m *map[string]V, key, label string) bool {
	if *m == nil {
		c.JSON(404, gin.H{"error": label + " not found"})
		return false
	}
	if _, ok := (*m)[key]; !ok {
		c.JSON(404, gin.H{"error": label + " not found"})
		return false
	}
	delete(*m, key)
	if len(*m) == 0 {
		*m = nil
	}
	return true
}

// parseBody reads the request body and tries direct JSON unmarshal into T,
// falling back to a {"items": T} wrapper.
func parseBody[T any](c *gin.Context) (T, bool) {
	var zero T
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return zero, false
	}
	var result T
	if err = json.Unmarshal(data, &result); err != nil {
		var wrapper struct {
			Items T `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return zero, false
		}
		result = wrapper.Items
	}
	return result, true
}

// Generic helpers for list[string]

func (h *Handler) putStringList(c *gin.Context, set func([]string), after func(), validate func(string) error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []string
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if validate != nil {
		for _, v := range arr {
			if errV := validate(v); errV != nil {
				c.JSON(400, gin.H{"error": errV.Error()})
				return
			}
		}
	}
	set(arr)
	if after != nil {
		after()
	}
	h.persist(c)
}

func (h *Handler) patchStringList(c *gin.Context, target *[]string, after func(), validate func(string) error) {
	var body struct {
		Old   *string `json:"old"`
		New   *string `json:"new"`
		Index *int    `json:"index"`
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if body.Index != nil && body.Value != nil && *body.Index >= 0 && *body.Index < len(*target) {
		if validate != nil {
			if errV := validate(*body.Value); errV != nil {
				c.JSON(400, gin.H{"error": errV.Error()})
				return
			}
		}
		(*target)[*body.Index] = *body.Value
		if after != nil {
			after()
		}
		h.persist(c)
		return
	}
	if body.Old != nil && body.New != nil {
		if validate != nil {
			if errV := validate(*body.New); errV != nil {
				c.JSON(400, gin.H{"error": errV.Error()})
				return
			}
		}
		for i := range *target {
			if (*target)[i] == *body.Old {
				(*target)[i] = *body.New
				if after != nil {
					after()
				}
				h.persist(c)
				return
			}
		}
		*target = append(*target, *body.New)
		if after != nil {
			after()
		}
		h.persist(c)
		return
	}
	c.JSON(400, gin.H{"error": "missing fields"})
}

func (h *Handler) deleteFromStringList(c *gin.Context, target *[]string, after func()) {
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(*target) {
			*target = append((*target)[:idx], (*target)[idx+1:]...)
			if after != nil {
				after()
			}
			h.persist(c)
			return
		}
	}
	if val := strings.TrimSpace(c.Query("value")); val != "" {
		out := make([]string, 0, len(*target))
		for _, v := range *target {
			if strings.TrimSpace(v) != val {
				out = append(out, v)
			}
		}
		*target = out
		if after != nil {
			after()
		}
		h.persist(c)
		return
	}
	c.JSON(400, gin.H{"error": "missing index or value"})
}

// validateAPIKey checks that an API key meets minimum requirements.
// Only ASCII printable characters (0x21-0x7E) are allowed.
func validateAPIKey(key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return fmt.Errorf("api key must not be empty")
	}
	if len(trimmed) < 8 {
		return fmt.Errorf("api key must be at least 8 characters")
	}
	for _, b := range []byte(trimmed) {
		if b < 0x21 || b > 0x7E {
			return fmt.Errorf("api key contains invalid character (only ASCII printable characters allowed)")
		}
	}
	return nil
}

// validateAlias checks that an alias is at most 20 characters and contains
// only alphanumeric characters, dashes, and underscores.
func validateAlias(alias string) error {
	if len(alias) > 20 {
		return fmt.Errorf("alias must be at most 20 characters")
	}
	for _, r := range alias {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return fmt.Errorf("alias must contain only alphanumeric characters, dashes, and underscores")
		}
	}
	return nil
}

// api-keys

func (h *Handler) GetAPIKeys(c *gin.Context) { c.JSON(200, gin.H{"api-keys": h.cfg.APIKeys}) }
func (h *Handler) PutAPIKeys(c *gin.Context) {
	h.putStringList(
		c, func(v []string) {
			h.cfg.APIKeys = append([]string(nil), v...)
		}, nil, validateAPIKey,
	)
}
func (h *Handler) PatchAPIKeys(c *gin.Context) {
	h.patchStringList(c, &h.cfg.APIKeys, func() {}, validateAPIKey)
}
func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	h.deleteFromStringList(c, &h.cfg.APIKeys, func() {})
}

// api-key-aliases

func (h *Handler) GetAPIKeyAliases(c *gin.Context) {
	aliases := h.cfg.APIKeyAliases
	if aliases == nil {
		aliases = map[string]string{}
	}
	c.JSON(200, gin.H{"api-key-aliases": aliases})
}

func (h *Handler) PutAPIKeyAliases(c *gin.Context) {
	var body map[string]string
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if len(body) == 0 {
		h.cfg.APIKeyAliases = nil
	} else {
		h.cfg.APIKeyAliases = body
	}
	h.persist(c)
}

func (h *Handler) PatchAPIKeyAlias(c *gin.Context) {
	var body struct {
		Key   string `json:"key"`
		Alias string `json:"alias"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Key == "" {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if h.cfg.APIKeyAliases == nil {
		h.cfg.APIKeyAliases = map[string]string{}
	}
	if body.Alias == "" {
		delete(h.cfg.APIKeyAliases, body.Key)
	} else {
		if err := validateAlias(body.Alias); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		h.cfg.APIKeyAliases[body.Key] = body.Alias
	}
	if len(h.cfg.APIKeyAliases) == 0 {
		h.cfg.APIKeyAliases = nil
	}
	h.persist(c)
}

func (h *Handler) DeleteAPIKeyAlias(c *gin.Context) {
	key := c.Query("key")
	if key == "" {
		c.JSON(400, gin.H{"error": "missing key parameter"})
		return
	}
	if h.cfg.APIKeyAliases != nil {
		delete(h.cfg.APIKeyAliases, key)
		if len(h.cfg.APIKeyAliases) == 0 {
			h.cfg.APIKeyAliases = nil
		}
	}
	h.persist(c)
}

// gemini-api-key: []GeminiKey

func (h *Handler) GetGeminiKeys(c *gin.Context) {
	c.JSON(200, gin.H{"gemini-api-key": h.geminiKeysWithAuthIndex()})
}
func (h *Handler) PutGeminiKeys(c *gin.Context)   { putStructList(h, c, geminiKeyResource) }
func (h *Handler) DeleteGeminiKey(c *gin.Context) { deleteStructEntry(h, c, geminiKeyResource) }
func (h *Handler) PatchGeminiKey(c *gin.Context) {
	patchStructEntry(
		h, c, geminiKeyResource, func(entry *config.GeminiKey, raw json.RawMessage) error {
			var p struct {
				APIKey         *string            `json:"api-key"`
				Prefix         *string            `json:"prefix"`
				BaseURL        *string            `json:"base-url"`
				ProxyURL       *string            `json:"proxy-url"`
				Headers        *map[string]string `json:"headers"`
				ExcludedModels *[]string          `json:"excluded-models"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if err := setRequiredTrimmed(p.APIKey, &entry.APIKey); err != nil {
				return err
			}
			setTrimmed(p.Prefix, &entry.Prefix)
			setTrimmed(p.BaseURL, &entry.BaseURL)
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			setHeaders(p.Headers, &entry.Headers)
			setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
			return nil
		},
	)
}

// claude-api-key: []ClaudeKey

func (h *Handler) GetClaudeKeys(c *gin.Context) {
	c.JSON(200, gin.H{"claude-api-key": h.claudeKeysWithAuthIndex()})
}
func (h *Handler) PutClaudeKeys(c *gin.Context)   { putStructList(h, c, claudeKeyResource) }
func (h *Handler) DeleteClaudeKey(c *gin.Context) { deleteStructEntry(h, c, claudeKeyResource) }
func (h *Handler) PatchClaudeKey(c *gin.Context) {
	patchStructEntry(
		h, c, claudeKeyResource, func(entry *config.ClaudeKey, raw json.RawMessage) error {
			var p struct {
				APIKey         *string               `json:"api-key"`
				Prefix         *string               `json:"prefix"`
				BaseURL        *string               `json:"base-url"`
				ProxyURL       *string               `json:"proxy-url"`
				Models         *[]config.ClaudeModel `json:"models"`
				Headers        *map[string]string    `json:"headers"`
				ExcludedModels *[]string             `json:"excluded-models"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			setTrimmed(p.APIKey, &entry.APIKey)
			setTrimmed(p.Prefix, &entry.Prefix)
			setTrimmed(p.BaseURL, &entry.BaseURL)
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			if p.Models != nil {
				entry.Models = append([]config.ClaudeModel(nil), *p.Models...)
			}
			setHeaders(p.Headers, &entry.Headers)
			setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
			return nil
		},
	)
}

// openai-compatibility: []OpenAICompatibility

func (h *Handler) GetOpenAICompat(c *gin.Context) {
	c.JSON(200, gin.H{"openai-compatibility": h.openAICompatibilityWithAuthIndex()})
}
func (h *Handler) PutOpenAICompat(c *gin.Context)    { putStructList(h, c, openAICompatResource) }
func (h *Handler) DeleteOpenAICompat(c *gin.Context) { deleteStructEntry(h, c, openAICompatResource) }
func (h *Handler) PatchOpenAICompat(c *gin.Context) {
	patchStructEntry(
		h, c, openAICompatResource, func(entry *config.OpenAICompatibility, raw json.RawMessage) error {
			var p struct {
				Name          *string                             `json:"name"`
				Prefix        *string                             `json:"prefix"`
				BaseURL       *string                             `json:"base-url"`
				Disabled      *bool                               `json:"disabled"`
				APIKeyEntries *[]config.OpenAICompatibilityAPIKey `json:"api-key-entries"`
				Models        *[]config.OpenAICompatibilityModel  `json:"models"`
				Headers       *map[string]string                  `json:"headers"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			setTrimmed(p.Name, &entry.Name)
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setRequiredTrimmed(p.BaseURL, &entry.BaseURL); err != nil {
				return err
			}
			if p.Disabled != nil {
				entry.Disabled = *p.Disabled
			}
			if p.APIKeyEntries != nil {
				entry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), *p.APIKeyEntries...)
			}
			if p.Models != nil {
				entry.Models = append([]config.OpenAICompatibilityModel(nil), *p.Models...)
			}
			setHeaders(p.Headers, &entry.Headers)
			return nil
		},
	)
}

// vertex-api-key: []VertexCompatKey

func (h *Handler) GetVertexCompatKeys(c *gin.Context) {
	c.JSON(200, gin.H{"vertex-api-key": h.vertexCompatKeysWithAuthIndex()})
}
func (h *Handler) PutVertexCompatKeys(c *gin.Context) { putStructList(h, c, vertexCompatKeyResource) }
func (h *Handler) DeleteVertexCompatKey(c *gin.Context) {
	deleteStructEntry(h, c, vertexCompatKeyResource)
}
func (h *Handler) PatchVertexCompatKey(c *gin.Context) {
	patchStructEntry(
		h, c, vertexCompatKeyResource, func(entry *config.VertexCompatKey, raw json.RawMessage) error {
			var p struct {
				APIKey   *string                     `json:"api-key"`
				Prefix   *string                     `json:"prefix"`
				BaseURL  *string                     `json:"base-url"`
				ProxyURL *string                     `json:"proxy-url"`
				Headers  *map[string]string          `json:"headers"`
				Models   *[]config.VertexCompatModel `json:"models"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if err := setRequiredTrimmed(p.APIKey, &entry.APIKey); err != nil {
				return err
			}
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setRequiredTrimmed(p.BaseURL, &entry.BaseURL); err != nil {
				return err
			}
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			setHeaders(p.Headers, &entry.Headers)
			if p.Models != nil {
				entry.Models = append([]config.VertexCompatModel(nil), *p.Models...)
			}
			return nil
		},
	)
}

// oauth-excluded-models: map[string][]string

func (h *Handler) GetOAuthExcludedModels(c *gin.Context) {
	c.JSON(200, gin.H{"oauth-excluded-models": config.NormalizeOAuthExcludedModels(h.cfg.OAuthExcludedModels)})
}

func (h *Handler) PutOAuthExcludedModels(c *gin.Context) {
	entries, ok := parseBody[map[string][]string](c)
	if !ok {
		return
	}
	h.cfg.OAuthExcludedModels = config.NormalizeOAuthExcludedModels(entries)
	h.persist(c)
}

func (h *Handler) PatchOAuthExcludedModels(c *gin.Context) {
	var body struct {
		Provider *string  `json:"provider"`
		Models   []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Provider == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(*body.Provider))
	if provider == "" {
		c.JSON(400, gin.H{"error": "invalid provider"})
		return
	}
	normalized := config.NormalizeExcludedModels(body.Models)
	if len(normalized) == 0 {
		if !deleteMapKey(c, &h.cfg.OAuthExcludedModels, provider, "provider") {
			return
		}
		h.persist(c)
		return
	}
	if h.cfg.OAuthExcludedModels == nil {
		h.cfg.OAuthExcludedModels = make(map[string][]string)
	}
	h.cfg.OAuthExcludedModels[provider] = normalized
	h.persist(c)
}

func (h *Handler) DeleteOAuthExcludedModels(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		c.JSON(400, gin.H{"error": "missing provider"})
		return
	}
	if !deleteMapKey(c, &h.cfg.OAuthExcludedModels, provider, "provider") {
		return
	}
	h.persist(c)
}

// oauth-model-alias: map[string][]OAuthModelAlias

func (h *Handler) GetOAuthModelAlias(c *gin.Context) {
	c.JSON(200, gin.H{"oauth-model-alias": sanitizedOAuthModelAlias(h.cfg.OAuthModelAlias)})
}

func (h *Handler) PutOAuthModelAlias(c *gin.Context) {
	entries, ok := parseBody[map[string][]config.OAuthModelAlias](c)
	if !ok {
		return
	}
	h.cfg.OAuthModelAlias = sanitizedOAuthModelAlias(entries)
	h.persist(c)
}

func (h *Handler) PatchOAuthModelAlias(c *gin.Context) {
	var body struct {
		Provider *string                  `json:"provider"`
		Channel  *string                  `json:"channel"`
		Aliases  []config.OAuthModelAlias `json:"aliases"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	channelRaw := ""
	if body.Channel != nil {
		channelRaw = *body.Channel
	} else if body.Provider != nil {
		channelRaw = *body.Provider
	}
	channel := strings.ToLower(strings.TrimSpace(channelRaw))
	if channel == "" {
		c.JSON(400, gin.H{"error": "invalid channel"})
		return
	}

	normalizedMap := sanitizedOAuthModelAlias(map[string][]config.OAuthModelAlias{channel: body.Aliases})
	normalized := normalizedMap[channel]
	if len(normalized) == 0 {
		if !deleteMapKey(c, &h.cfg.OAuthModelAlias, channel, "channel") {
			return
		}
		h.persist(c)
		return
	}
	if h.cfg.OAuthModelAlias == nil {
		h.cfg.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	h.cfg.OAuthModelAlias[channel] = normalized
	h.persist(c)
}

func (h *Handler) DeleteOAuthModelAlias(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(c.Query("provider")))
	}
	if channel == "" {
		c.JSON(400, gin.H{"error": "missing channel"})
		return
	}
	if !deleteMapKey(c, &h.cfg.OAuthModelAlias, channel, "channel") {
		return
	}
	h.persist(c)
}

// codex-api-key: []CodexKey

func (h *Handler) GetCodexKeys(c *gin.Context) {
	c.JSON(200, gin.H{"codex-api-key": h.codexKeysWithAuthIndex()})
}
func (h *Handler) PutCodexKeys(c *gin.Context)   { putStructList(h, c, codexKeyResource) }
func (h *Handler) DeleteCodexKey(c *gin.Context) { deleteStructEntry(h, c, codexKeyResource) }
func (h *Handler) PatchCodexKey(c *gin.Context) {
	patchStructEntry(
		h, c, codexKeyResource, func(entry *config.CodexKey, raw json.RawMessage) error {
			var p struct {
				APIKey         *string              `json:"api-key"`
				Prefix         *string              `json:"prefix"`
				BaseURL        *string              `json:"base-url"`
				ProxyURL       *string              `json:"proxy-url"`
				Models         *[]config.CodexModel `json:"models"`
				Headers        *map[string]string   `json:"headers"`
				ExcludedModels *[]string            `json:"excluded-models"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			setTrimmed(p.APIKey, &entry.APIKey)
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setRequiredTrimmed(p.BaseURL, &entry.BaseURL); err != nil {
				return err
			}
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			if p.Models != nil {
				entry.Models = append([]config.CodexModel(nil), *p.Models...)
			}
			setHeaders(p.Headers, &entry.Headers)
			setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
			return nil
		},
	)
}

func normalizeOpenAICompatibilityEntry(entry *config.OpenAICompatibility) {
	if entry == nil {
		return
	}
	// Trim base-url; empty base-url indicates provider should be removed by sanitization
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	existing := make(map[string]struct{}, len(entry.APIKeyEntries))
	for i := range entry.APIKeyEntries {
		trimmed := strings.TrimSpace(entry.APIKeyEntries[i].APIKey)
		entry.APIKeyEntries[i].APIKey = trimmed
		if trimmed != "" {
			existing[trimmed] = struct{}{}
		}
	}
}

func normalizedOpenAICompatibilityEntries(entries []config.OpenAICompatibility) []config.OpenAICompatibility {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenAICompatibility, len(entries))
	for i := range entries {
		copyEntry := entries[i]
		if len(copyEntry.APIKeyEntries) > 0 {
			copyEntry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), copyEntry.APIKeyEntries...)
		}
		normalizeOpenAICompatibilityEntry(&copyEntry)
		out[i] = copyEntry
	}
	return out
}

// normalizeModels trims Name/Alias in each model entry and keeps entries satisfying the keep predicate.
func normalizeModels[T any](models []T, nameAlias func(*T) (*string, *string), keep func(name, alias string) bool) []T {
	if len(models) == 0 {
		return models
	}
	out := make([]T, 0, len(models))
	for i := range models {
		name, alias := nameAlias(&models[i])
		*name = strings.TrimSpace(*name)
		*alias = strings.TrimSpace(*alias)
		if keep(*name, *alias) {
			out = append(out, models[i])
		}
	}
	return out
}

// keepEither keeps models where at least one of Name/Alias is non-empty.
func keepEither(name, alias string) bool { return name != "" || alias != "" }

// keepBoth keeps models where both Name and Alias are non-empty.
func keepBoth(name, alias string) bool { return name != "" && alias != "" }

func normalizeClaudeKey(entry *config.ClaudeKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	entry.Models = normalizeModels(
		entry.Models, func(m *config.ClaudeModel) (*string, *string) {
			return &m.Name, &m.Alias
		}, keepEither,
	)
}

func normalizeCodexKey(entry *config.CodexKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	entry.Models = normalizeModels(
		entry.Models, func(m *config.CodexModel) (*string, *string) {
			return &m.Name, &m.Alias
		}, keepEither,
	)
}

func normalizeVertexCompatKey(entry *config.VertexCompatKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.Models = normalizeModels(
		entry.Models, func(m *config.VertexCompatModel) (*string, *string) {
			return &m.Name, &m.Alias
		}, keepBoth,
	)
}

func sanitizedOAuthModelAlias(entries map[string][]config.OAuthModelAlias) map[string][]config.OAuthModelAlias {
	if len(entries) == 0 {
		return nil
	}
	copied := make(map[string][]config.OAuthModelAlias, len(entries))
	for channel, aliases := range entries {
		if len(aliases) == 0 {
			continue
		}
		copied[channel] = append([]config.OAuthModelAlias(nil), aliases...)
	}
	if len(copied) == 0 {
		return nil
	}
	cfg := config.Config{OAuthModelAlias: copied}
	cfg.SanitizeOAuthModelAlias()
	if len(cfg.OAuthModelAlias) == 0 {
		return nil
	}
	return cfg.OAuthModelAlias
}

// GetAmpCode returns the complete ampcode configuration.
func (h *Handler) GetAmpCode(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"ampcode": config.AmpCode{}})
		return
	}
	c.JSON(200, gin.H{"ampcode": h.cfg.AmpCode})
}

// GetAmpUpstreamURL returns the ampcode upstream URL.
func (h *Handler) GetAmpUpstreamURL(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"upstream-url": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-url": h.cfg.AmpCode.UpstreamURL})
}

// PutAmpUpstreamURL updates the ampcode upstream URL.
func (h *Handler) PutAmpUpstreamURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.AmpCode.UpstreamURL = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamURL clears the ampcode upstream URL.
func (h *Handler) DeleteAmpUpstreamURL(c *gin.Context) {
	h.cfg.AmpCode.UpstreamURL = ""
	h.persist(c)
}

// GetAmpUpstreamAPIKey returns the ampcode upstream API key.
func (h *Handler) GetAmpUpstreamAPIKey(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"upstream-api-key": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-api-key": h.cfg.AmpCode.UpstreamAPIKey})
}

// PutAmpUpstreamAPIKey updates the ampcode upstream API key.
func (h *Handler) PutAmpUpstreamAPIKey(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.AmpCode.UpstreamAPIKey = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamAPIKey clears the ampcode upstream API key.
func (h *Handler) DeleteAmpUpstreamAPIKey(c *gin.Context) {
	h.cfg.AmpCode.UpstreamAPIKey = ""
	h.persist(c)
}

// GetAmpRestrictManagementToLocalhost returns the localhost restriction setting.
func (h *Handler) GetAmpRestrictManagementToLocalhost(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"restrict-management-to-localhost": true})
		return
	}
	c.JSON(200, gin.H{"restrict-management-to-localhost": h.cfg.AmpCode.RestrictManagementToLocalhost})
}

// PutAmpRestrictManagementToLocalhost updates the localhost restriction setting.
func (h *Handler) PutAmpRestrictManagementToLocalhost(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AmpCode.RestrictManagementToLocalhost = v })
}

// GetAmpModelMappings returns the ampcode model mappings.
func (h *Handler) GetAmpModelMappings(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"model-mappings": []config.AmpModelMapping{}})
		return
	}
	c.JSON(200, gin.H{"model-mappings": h.cfg.AmpCode.ModelMappings})
}

// PutAmpModelMappings replaces all ampcode model mappings.
func (h *Handler) PutAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.cfg.AmpCode.ModelMappings = body.Value
	h.persist(c)
}

// PatchAmpModelMappings adds or updates model mappings.
func (h *Handler) PatchAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	existing := make(map[string]int)
	for i, m := range h.cfg.AmpCode.ModelMappings {
		existing[strings.TrimSpace(m.From)] = i
	}

	for _, newMapping := range body.Value {
		from := strings.TrimSpace(newMapping.From)
		if idx, ok := existing[from]; ok {
			h.cfg.AmpCode.ModelMappings[idx] = newMapping
		} else {
			h.cfg.AmpCode.ModelMappings = append(h.cfg.AmpCode.ModelMappings, newMapping)
			existing[from] = len(h.cfg.AmpCode.ModelMappings) - 1
		}
	}
	h.persist(c)
}

// DeleteAmpModelMappings removes specified model mappings by "from" field.
func (h *Handler) DeleteAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Value) == 0 {
		h.cfg.AmpCode.ModelMappings = nil
		h.persist(c)
		return
	}

	toRemove := make(map[string]bool)
	for _, from := range body.Value {
		toRemove[strings.TrimSpace(from)] = true
	}

	newMappings := make([]config.AmpModelMapping, 0, len(h.cfg.AmpCode.ModelMappings))
	for _, m := range h.cfg.AmpCode.ModelMappings {
		if !toRemove[strings.TrimSpace(m.From)] {
			newMappings = append(newMappings, m)
		}
	}
	h.cfg.AmpCode.ModelMappings = newMappings
	h.persist(c)
}

// GetAmpForceModelMappings returns whether model mappings are forced.
func (h *Handler) GetAmpForceModelMappings(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"force-model-mappings": false})
		return
	}
	c.JSON(200, gin.H{"force-model-mappings": h.cfg.AmpCode.ForceModelMappings})
}

// PutAmpForceModelMappings updates the force model mappings setting.
func (h *Handler) PutAmpForceModelMappings(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AmpCode.ForceModelMappings = v })
}

// GetAmpUpstreamAPIKeys returns the ampcode upstream API keys mapping.
func (h *Handler) GetAmpUpstreamAPIKeys(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"upstream-api-keys": []config.AmpUpstreamAPIKeyEntry{}})
		return
	}
	c.JSON(200, gin.H{"upstream-api-keys": h.cfg.AmpCode.UpstreamAPIKeys})
}

// PutAmpUpstreamAPIKeys replaces all ampcode upstream API keys mappings.
func (h *Handler) PutAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	// Normalize entries: trim whitespace, filter empty
	normalized := normalizeAmpUpstreamAPIKeyEntries(body.Value)
	h.cfg.AmpCode.UpstreamAPIKeys = normalized
	h.persist(c)
}

// PatchAmpUpstreamAPIKeys adds or updates upstream API keys entries.
// Matching is done by upstream-api-key value.
func (h *Handler) PatchAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	existing := make(map[string]int)
	for i, entry := range h.cfg.AmpCode.UpstreamAPIKeys {
		existing[strings.TrimSpace(entry.UpstreamAPIKey)] = i
	}

	for _, newEntry := range body.Value {
		upstreamKey := strings.TrimSpace(newEntry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		normalizedEntry := config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        normalizeAPIKeysList(newEntry.APIKeys),
		}
		if idx, ok := existing[upstreamKey]; ok {
			h.cfg.AmpCode.UpstreamAPIKeys[idx] = normalizedEntry
		} else {
			h.cfg.AmpCode.UpstreamAPIKeys = append(h.cfg.AmpCode.UpstreamAPIKeys, normalizedEntry)
			existing[upstreamKey] = len(h.cfg.AmpCode.UpstreamAPIKeys) - 1
		}
	}
	h.persist(c)
}

// DeleteAmpUpstreamAPIKeys removes specified upstream API keys entries.
// Body must be JSON: {"value": ["<upstream-api-key>", ...]}.
// If "value" is an empty array, clears all entries.
// If JSON is invalid or "value" is missing/null, returns 400 and does not persist any change.
func (h *Handler) DeleteAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	if body.Value == nil {
		c.JSON(400, gin.H{"error": "missing value"})
		return
	}

	// Empty array means clear all
	if len(body.Value) == 0 {
		h.cfg.AmpCode.UpstreamAPIKeys = nil
		h.persist(c)
		return
	}

	toRemove := make(map[string]bool)
	for _, key := range body.Value {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		toRemove[trimmed] = true
	}
	if len(toRemove) == 0 {
		c.JSON(400, gin.H{"error": "empty value"})
		return
	}

	newEntries := make([]config.AmpUpstreamAPIKeyEntry, 0, len(h.cfg.AmpCode.UpstreamAPIKeys))
	for _, entry := range h.cfg.AmpCode.UpstreamAPIKeys {
		if !toRemove[strings.TrimSpace(entry.UpstreamAPIKey)] {
			newEntries = append(newEntries, entry)
		}
	}
	h.cfg.AmpCode.UpstreamAPIKeys = newEntries
	h.persist(c)
}

// normalizeAmpUpstreamAPIKeyEntries normalizes a list of upstream API key entries.
func normalizeAmpUpstreamAPIKeyEntries(entries []config.AmpUpstreamAPIKeyEntry) []config.AmpUpstreamAPIKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.AmpUpstreamAPIKeyEntry, 0, len(entries))
	for _, entry := range entries {
		upstreamKey := strings.TrimSpace(entry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		apiKeys := normalizeAPIKeysList(entry.APIKeys)
		out = append(
			out, config.AmpUpstreamAPIKeyEntry{
				UpstreamAPIKey: upstreamKey,
				APIKeys:        apiKeys,
			},
		)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeAPIKeysList trims and filters empty strings from a list of API keys.
func normalizeAPIKeysList(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

package management

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
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

func setBaseURL(src *string, dst *string) error {
	if src == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*src)
	if trimmed != "" {
		if err := proxyutil.ValidateHTTPURL(trimmed); err != nil {
			return fmt.Errorf("invalid base-url: %w", err)
		}
	}
	*dst = trimmed
	return nil
}

func setRequiredBaseURL(src *string, dst *string) error {
	if src == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*src)
	if trimmed == "" {
		return errEntryDeleted
	}
	if err := proxyutil.ValidateHTTPURL(trimmed); err != nil {
		return fmt.Errorf("invalid base-url: %w", err)
	}
	*dst = trimmed
	return nil
}

func validateBaseURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if err := proxyutil.ValidateHTTPURL(trimmed); err != nil {
		return fmt.Errorf("invalid base-url: %w", err)
	}
	return nil
}

func validateConfigHeaders(headers map[string]string, existing map[string]string) error {
	for name, value := range headers {
		trimmedName := strings.TrimSpace(name)
		trimmedValue := strings.TrimSpace(value)
		if trimmedName == "" || trimmedValue == "" {
			continue
		}
		if !proxyutil.IsRestrictedCustomHeader(trimmedName) {
			continue
		}
		existingValue, ok := matchingConfigHeader(existing, trimmedName)
		if !ok || existingValue != trimmedValue {
			return fmt.Errorf("custom header %q is restricted", trimmedName)
		}
	}
	return nil
}

func matchingConfigHeader(headers map[string]string, name string) (string, bool) {
	for candidateName, value := range headers {
		if strings.EqualFold(strings.TrimSpace(candidateName), name) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
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

func setCredentialWeight(raw json.RawMessage, dst **int) error {
	if len(raw) == 0 {
		return nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		*dst = nil
		return nil
	}
	var weight int
	if err := json.Unmarshal(raw, &weight); err != nil {
		return fmt.Errorf("weight must be an integer")
	}
	if err := config.ValidateCredentialWeight(&weight); err != nil {
		return err
	}
	*dst = &weight
	return nil
}

func patchClaudeCloak(raw json.RawMessage, dst **config.CloakConfig) error {
	if len(raw) == 0 {
		return nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		*dst = nil
		return nil
	}

	var patch struct {
		Mode           *string   `json:"mode"`
		StrictMode     *bool     `json:"strict-mode"`
		SensitiveWords *[]string `json:"sensitive-words"`
	}
	if err := json.Unmarshal(raw, &patch); err != nil {
		return err
	}

	next := config.CloakConfig{}
	if *dst != nil {
		next = **dst
		next.SensitiveWords = append([]string(nil), (*dst).SensitiveWords...)
	}
	if patch.Mode != nil {
		next.Mode = strings.ToLower(strings.TrimSpace(*patch.Mode))
	}
	if patch.StrictMode != nil {
		next.StrictMode = *patch.StrictMode
	}
	if patch.SensitiveWords != nil {
		next.SensitiveWords = append([]string(nil), *patch.SensitiveWords...)
	}
	*dst = &next
	return nil
}

// parseBody reads the request body and tries direct JSON unmarshal into T,
// falling back to a {"items": T} wrapper.
func parseBody[T any](c *gin.Context) (T, bool) {
	var zero T
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := handlers.ReadRequestBodyWithLimit(c, 10<<20)
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

func (h *Handler) putStringList(
	c *gin.Context,
	get func(*config.Config) []string,
	set func(*config.Config, []string),
	after func(*config.Config),
	validate func(string) error,
	transform func([]string, []string) ([]string, error),
) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := handlers.ReadRequestBodyWithLimit(c, 10<<20)
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var submitted []string
	if err = json.Unmarshal(data, &submitted); err != nil {
		var obj struct {
			Items []string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		submitted = obj.Items
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			arr := append([]string(nil), submitted...)
			if transform != nil {
				arr, err = transform(arr, get(next))
				if err != nil {
					return err
				}
			}
			if validate != nil {
				for _, value := range arr {
					if errValidate := validate(value); errValidate != nil {
						return errValidate
					}
				}
			}
			set(next, arr)
			if after != nil {
				after(next)
			}
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) patchStringList(
	c *gin.Context,
	get func(*config.Config) []string,
	set func(*config.Config, []string),
	after func(*config.Config),
	validate func(string) error,
) {
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
	if (body.Index == nil || body.Value == nil) && (body.Old == nil || body.New == nil) {
		c.JSON(400, gin.H{"error": "missing fields"})
		return
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			list := append([]string(nil), get(next)...)
			if body.Index != nil && body.Value != nil && *body.Index >= 0 && *body.Index < len(list) {
				resolvedValue, errResolve := resolveSubmittedAPIKey(*body.Value, list)
				if errResolve != nil {
					return errResolve
				}
				if validate != nil {
					if errValidate := validate(resolvedValue); errValidate != nil {
						return errValidate
					}
				}
				list[*body.Index] = resolvedValue
				set(next, list)
				if after != nil {
					after(next)
				}
				return nil
			}
			if body.Old == nil || body.New == nil {
				return newConfigMutationError(http.StatusBadRequest, "missing fields")
			}
			resolvedOld, errResolve := resolveSubmittedAPIKey(*body.Old, list)
			if errResolve != nil {
				return errResolve
			}
			resolvedNew, errResolve := resolveSubmittedAPIKey(*body.New, list)
			if errResolve != nil {
				return errResolve
			}
			if validate != nil {
				if errValidate := validate(resolvedNew); errValidate != nil {
					return errValidate
				}
			}
			replaced := false
			for i := range list {
				if list[i] == resolvedOld {
					list[i] = resolvedNew
					replaced = true
					break
				}
			}
			if !replaced {
				list = append(list, resolvedNew)
			}
			set(next, list)
			if after != nil {
				after(next)
			}
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) deleteFromStringList(
	c *gin.Context,
	get func(*config.Config) []string,
	set func(*config.Config, []string),
	after func(*config.Config),
) {
	idxStr := c.Query("index")
	value := strings.TrimSpace(c.Query("value"))
	if idxStr == "" && value == "" {
		c.JSON(400, gin.H{"error": "missing index or value"})
		return
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			list := append([]string(nil), get(next)...)
			if idxStr != "" {
				var index int
				if _, errScan := fmt.Sscanf(idxStr, "%d", &index); errScan == nil && index >= 0 && index < len(list) {
					list = append(list[:index], list[index+1:]...)
					set(next, list)
					if after != nil {
						after(next)
					}
					return nil
				}
			}
			if value == "" {
				return newConfigMutationError(http.StatusBadRequest, "missing index or value")
			}
			resolvedValue, errResolve := resolveSubmittedAPIKey(value, list)
			if errResolve != nil {
				return errResolve
			}
			out := make([]string, 0, len(list))
			for _, item := range list {
				if strings.TrimSpace(item) != resolvedValue {
					out = append(out, item)
				}
			}
			set(next, out)
			if after != nil {
				after(next)
			}
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// validateAPIKey checks that an API key meets minimum requirements.
// Only ASCII printable characters (0x21-0x7E) are allowed.
func validateAPIKey(key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return fmt.Errorf("api key must not be empty")
	}
	if err := validateResolvedAPIKeyPlaceholder(trimmed); err != nil {
		return err
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

func validateResolvedAPIKeyPlaceholder(key string) error {
	base, _, _ := splitMaskedAPIKeyOrdinal(strings.TrimSpace(key))
	if base == "***" {
		return fmt.Errorf("api key placeholder could not be resolved")
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

func (h *Handler) GetAPIKeys(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusOK, gin.H{"api-keys": []string{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"api-keys": maskAPIKeyList(cfg.APIKeys)})
}
func (h *Handler) PutAPIKeys(c *gin.Context) {
	h.putStringList(
		c,
		func(cfg *config.Config) []string { return cfg.APIKeys },
		func(cfg *config.Config, values []string) { cfg.APIKeys = values },
		nil,
		validateAPIKey,
		func(values, existing []string) ([]string, error) {
			return resolveStringListPlaceholders(values, existing)
		},
	)
}
func (h *Handler) PatchAPIKeys(c *gin.Context) {
	h.patchStringList(
		c,
		func(cfg *config.Config) []string { return cfg.APIKeys },
		func(cfg *config.Config, values []string) { cfg.APIKeys = values },
		nil,
		validateAPIKey,
	)
}
func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	h.deleteFromStringList(
		c,
		func(cfg *config.Config) []string { return cfg.APIKeys },
		func(cfg *config.Config, values []string) { cfg.APIKeys = values },
		nil,
	)
}

// api-key-aliases

func (h *Handler) GetAPIKeyAliases(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"api-key-aliases": h.apiKeyAliasesSnapshot()})
}

func configAPIKeyAliases(cfg *config.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	return cfg.APIKeyAliases
}

func copyAPIKeyAliases(aliases map[string]string) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(aliases))
	maps.Copy(out, aliases)
	return out
}

func (h *Handler) replaceAPIKeyAliasesSnapshot(aliases map[string]string) {
	snapshot := copyAPIKeyAliases(aliases)
	h.apiKeyAliasesMu.Lock()
	h.apiKeyAliases = snapshot
	h.apiKeyAliasesMu.Unlock()
}

func (h *Handler) apiKeyAliasesSnapshot() map[string]string {
	h.apiKeyAliasesMu.RLock()
	defer h.apiKeyAliasesMu.RUnlock()
	aliases := copyAPIKeyAliases(h.apiKeyAliases)
	if aliases == nil {
		return map[string]string{}
	}
	return aliases
}

func (h *Handler) PutAPIKeyAliases(c *gin.Context) {
	var body map[string]string
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			if len(body) == 0 {
				next.APIKeyAliases = nil
				return nil
			}
			resolved, errResolve := resolveAliasMapPlaceholders(body, next.APIKeyAliases)
			if errResolve != nil {
				return errResolve
			}
			next.APIKeyAliases = resolved
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
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
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			resolvedKey, errResolve := resolveSubmittedAPIKey(body.Key, aliasKeyCandidates(next.APIKeyAliases))
			if errResolve != nil {
				return errResolve
			}
			if errValidate := validateAPIKey(resolvedKey); errValidate != nil {
				return errValidate
			}
			if body.Alias == "" {
				delete(next.APIKeyAliases, resolvedKey)
			} else {
				if errValidate := validateAlias(body.Alias); errValidate != nil {
					return errValidate
				}
				if next.APIKeyAliases == nil {
					next.APIKeyAliases = map[string]string{}
				}
				next.APIKeyAliases[resolvedKey] = body.Alias
			}
			if len(next.APIKeyAliases) == 0 {
				next.APIKeyAliases = nil
			}
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) DeleteAPIKeyAlias(c *gin.Context) {
	key := c.Query("key")
	if key == "" && c.Request.Body != nil && c.Request.ContentLength != 0 {
		var body struct {
			Key string `json:"key"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		key = body.Key
	}
	if key == "" {
		c.JSON(400, gin.H{"error": "missing key parameter"})
		return
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			resolvedKey, errResolve := resolveSubmittedAPIKey(key, aliasKeyCandidates(next.APIKeyAliases))
			if errResolve != nil {
				return errResolve
			}
			delete(next.APIKeyAliases, resolvedKey)
			if len(next.APIKeyAliases) == 0 {
				next.APIKeyAliases = nil
			}
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// gemini-api-key: []GeminiKey

func (h *Handler) GetGeminiKeys(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"gemini-api-key": h.geminiKeysWithAuthIndex()})
}
func (h *Handler) PutGeminiKeys(c *gin.Context)   { putStructList(h, c, geminiKeyResource) }
func (h *Handler) PostGeminiKey(c *gin.Context)   { postStructEntry(h, c, geminiKeyResource) }
func (h *Handler) DeleteGeminiKey(c *gin.Context) { deleteStructEntry(h, c, geminiKeyResource) }
func (h *Handler) PatchGeminiKey(c *gin.Context) {
	patchGeminiKeyResource(h, c, geminiKeyResource)
}

// interactions-api-key: []GeminiKey

func (h *Handler) GetInteractionsKeys(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"interactions-api-key": h.interactionsKeysWithAuthIndex()})
}
func (h *Handler) PutInteractionsKeys(c *gin.Context) {
	putStructList(h, c, interactionsKeyResource)
}
func (h *Handler) PostInteractionsKey(c *gin.Context) {
	postStructEntry(h, c, interactionsKeyResource)
}
func (h *Handler) DeleteInteractionsKey(c *gin.Context) {
	deleteStructEntry(h, c, interactionsKeyResource)
}
func (h *Handler) PatchInteractionsKey(c *gin.Context) {
	patchGeminiKeyResource(h, c, interactionsKeyResource)
}

func patchGeminiKeyResource(h *Handler, c *gin.Context, resource structListResource[config.GeminiKey]) {
	patchStructEntry(
		h, c, resource, func(entry *config.GeminiKey, raw json.RawMessage) error {
			var p struct {
				APIKey         *string               `json:"api-key"`
				Priority       *int                  `json:"priority"`
				Weight         json.RawMessage       `json:"weight"`
				Prefix         *string               `json:"prefix"`
				BaseURL        *string               `json:"base-url"`
				ProxyURL       *string               `json:"proxy-url"`
				Models         *[]config.GeminiModel `json:"models"`
				Headers        *map[string]string    `json:"headers"`
				ExcludedModels *[]string             `json:"excluded-models"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if err := setRequiredTrimmed(p.APIKey, &entry.APIKey); err != nil {
				return err
			}
			if p.Priority != nil {
				entry.Priority = *p.Priority
			}
			if err := setCredentialWeight(p.Weight, &entry.Weight); err != nil {
				return err
			}
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setBaseURL(p.BaseURL, &entry.BaseURL); err != nil {
				return err
			}
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			if p.Models != nil {
				entry.Models = append([]config.GeminiModel(nil), *p.Models...)
			}
			setHeaders(p.Headers, &entry.Headers)
			setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
			return nil
		},
	)
}

// claude-api-key: []ClaudeKey

func (h *Handler) GetClaudeKeys(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"claude-api-key": h.claudeKeysWithAuthIndex()})
}
func (h *Handler) PutClaudeKeys(c *gin.Context)   { putStructList(h, c, claudeKeyResource) }
func (h *Handler) PostClaudeKey(c *gin.Context)   { postStructEntry(h, c, claudeKeyResource) }
func (h *Handler) DeleteClaudeKey(c *gin.Context) { deleteStructEntry(h, c, claudeKeyResource) }
func (h *Handler) PatchClaudeKey(c *gin.Context) {
	patchStructEntry(
		h, c, claudeKeyResource, func(entry *config.ClaudeKey, raw json.RawMessage) error {
			var p struct {
				APIKey                  *string               `json:"api-key"`
				Priority                *int                  `json:"priority"`
				Weight                  json.RawMessage       `json:"weight"`
				Prefix                  *string               `json:"prefix"`
				BaseURL                 *string               `json:"base-url"`
				ProxyURL                *string               `json:"proxy-url"`
				Models                  *[]config.ClaudeModel `json:"models"`
				Headers                 *map[string]string    `json:"headers"`
				ExcludedModels          *[]string             `json:"excluded-models"`
				RebuildMidSystemMessage *bool                 `json:"rebuild-mid-system-message"`
				Cloak                   json.RawMessage       `json:"cloak"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			setTrimmed(p.APIKey, &entry.APIKey)
			if p.Priority != nil {
				entry.Priority = *p.Priority
			}
			if err := setCredentialWeight(p.Weight, &entry.Weight); err != nil {
				return err
			}
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setBaseURL(p.BaseURL, &entry.BaseURL); err != nil {
				return err
			}
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			if p.Models != nil {
				entry.Models = append([]config.ClaudeModel(nil), *p.Models...)
			}
			setHeaders(p.Headers, &entry.Headers)
			setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
			if p.RebuildMidSystemMessage != nil {
				entry.RebuildMidSystemMessage = *p.RebuildMidSystemMessage
			}
			return patchClaudeCloak(p.Cloak, &entry.Cloak)
		},
	)
}

// openai-compatibility: []OpenAICompatibility

func (h *Handler) GetOpenAICompat(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"openai-compatibility": h.openAICompatibilityWithAuthIndex()})
}
func (h *Handler) PutOpenAICompat(c *gin.Context)    { putStructList(h, c, openAICompatResource) }
func (h *Handler) PostOpenAICompat(c *gin.Context)   { postStructEntry(h, c, openAICompatResource) }
func (h *Handler) DeleteOpenAICompat(c *gin.Context) { deleteStructEntry(h, c, openAICompatResource) }
func (h *Handler) PatchOpenAICompat(c *gin.Context) {
	patchStructEntry(
		h, c, openAICompatResource, func(entry *config.OpenAICompatibility, raw json.RawMessage) error {
			var p struct {
				Name                  *string                             `json:"name"`
				Priority              *int                                `json:"priority"`
				Prefix                *string                             `json:"prefix"`
				BaseURL               *string                             `json:"base-url"`
				Disabled              *bool                               `json:"disabled"`
				APIKeyEntries         *[]config.OpenAICompatibilityAPIKey `json:"api-key-entries"`
				Models                *[]config.OpenAICompatibilityModel  `json:"models"`
				Headers               *map[string]string                  `json:"headers"`
				SupportPromptCacheKey *bool                               `json:"support-prompt-cache-key"`
				DisableCooling        *bool                               `json:"disable-cooling"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			setTrimmed(p.Name, &entry.Name)
			if p.Priority != nil {
				entry.Priority = *p.Priority
			}
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setRequiredBaseURL(p.BaseURL, &entry.BaseURL); err != nil {
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
			if p.SupportPromptCacheKey != nil {
				entry.SupportPromptCacheKey = *p.SupportPromptCacheKey
			}
			if p.DisableCooling != nil {
				entry.DisableCooling = *p.DisableCooling
			}
			return nil
		},
	)
}

// vertex-api-key: []VertexCompatKey

func (h *Handler) GetVertexCompatKeys(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"vertex-api-key": h.vertexCompatKeysWithAuthIndex()})
}
func (h *Handler) PutVertexCompatKeys(c *gin.Context) { putStructList(h, c, vertexCompatKeyResource) }
func (h *Handler) PostVertexCompatKey(c *gin.Context) { postStructEntry(h, c, vertexCompatKeyResource) }
func (h *Handler) DeleteVertexCompatKey(c *gin.Context) {
	deleteStructEntry(h, c, vertexCompatKeyResource)
}
func (h *Handler) PatchVertexCompatKey(c *gin.Context) {
	patchStructEntry(
		h, c, vertexCompatKeyResource, func(entry *config.VertexCompatKey, raw json.RawMessage) error {
			var p struct {
				APIKey         *string                     `json:"api-key"`
				Priority       *int                        `json:"priority"`
				Weight         json.RawMessage             `json:"weight"`
				Prefix         *string                     `json:"prefix"`
				BaseURL        *string                     `json:"base-url"`
				ProxyURL       *string                     `json:"proxy-url"`
				Headers        *map[string]string          `json:"headers"`
				Models         *[]config.VertexCompatModel `json:"models"`
				ExcludedModels *[]string                   `json:"excluded-models"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if err := setRequiredTrimmed(p.APIKey, &entry.APIKey); err != nil {
				return err
			}
			if p.Priority != nil {
				entry.Priority = *p.Priority
			}
			if err := setCredentialWeight(p.Weight, &entry.Weight); err != nil {
				return err
			}
			setTrimmed(p.Prefix, &entry.Prefix)
			if err := setRequiredBaseURL(p.BaseURL, &entry.BaseURL); err != nil {
				return err
			}
			setTrimmed(p.ProxyURL, &entry.ProxyURL)
			setHeaders(p.Headers, &entry.Headers)
			if p.Models != nil {
				entry.Models = append([]config.VertexCompatModel(nil), *p.Models...)
			}
			setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
			return nil
		},
	)
}

// oauth-excluded-models: map[string][]string

func (h *Handler) GetOAuthExcludedModels(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"oauth-excluded-models": map[string][]string{}})
		return
	}
	c.JSON(200, gin.H{"oauth-excluded-models": config.NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)})
}

func (h *Handler) PutOAuthExcludedModels(c *gin.Context) {
	entries, ok := parseBody[map[string][]string](c)
	if !ok {
		return
	}
	if _, ok = h.mutateConfig(
		c, func(next *config.Config) error {
			next.OAuthExcludedModels = config.NormalizeOAuthExcludedModels(entries)
			next.PruneGloballyExcludedAPIKeyRules()
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
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
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			if len(normalized) == 0 {
				if next.OAuthExcludedModels == nil {
					return newConfigMutationError(http.StatusNotFound, "provider not found")
				}
				if _, exists := next.OAuthExcludedModels[provider]; !exists {
					return newConfigMutationError(http.StatusNotFound, "provider not found")
				}
				delete(next.OAuthExcludedModels, provider)
				if len(next.OAuthExcludedModels) == 0 {
					next.OAuthExcludedModels = nil
				}
				return nil
			}
			if next.OAuthExcludedModels == nil {
				next.OAuthExcludedModels = make(map[string][]string)
			}
			next.OAuthExcludedModels[provider] = normalized
			next.PruneGloballyExcludedAPIKeyRules()
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) DeleteOAuthExcludedModels(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		c.JSON(400, gin.H{"error": "missing provider"})
		return
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			if next.OAuthExcludedModels == nil {
				return newConfigMutationError(http.StatusNotFound, "provider not found")
			}
			if _, exists := next.OAuthExcludedModels[provider]; !exists {
				return newConfigMutationError(http.StatusNotFound, "provider not found")
			}
			delete(next.OAuthExcludedModels, provider)
			if len(next.OAuthExcludedModels) == 0 {
				next.OAuthExcludedModels = nil
			}
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// oauth-model-alias: map[string][]OAuthModelAlias

func (h *Handler) GetOAuthModelAlias(c *gin.Context) {
	doc, err := modelcatalog.LoadOrInit(h.configFilePath, h.configSnapshot())
	if err != nil {
		log.WithError(err).Error("failed to load model catalog")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load model catalog"})
		return
	}
	c.JSON(200, gin.H{"oauth-model-alias": doc.ToOAuthModelAlias()})
}

func (h *Handler) PutOAuthModelAlias(c *gin.Context) {
	entries, ok := parseBody[map[string][]config.OAuthModelAlias](c)
	if !ok {
		return
	}
	_, ok = h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			modelcatalog.ReplaceOAuthModelAlias(doc, sanitizedOAuthModelAlias(entries))
			return nil
		},
	)
	if ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
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
	_, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			if len(normalizedMap[channel]) == 0 {
				if !modelcatalog.DeleteOAuthModelAliasChannel(doc, channel) {
					return newConfigMutationError(http.StatusNotFound, "channel not found")
				}
				return nil
			}
			modelcatalog.ReplaceOAuthModelAliasChannel(doc, channel, normalizedMap[channel])
			return nil
		},
	)
	if ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
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
	_, ok := h.mutateModelCatalog(
		c,
		modelCatalogMutationOptions{},
		func(doc *modelcatalog.File) error {
			if !modelcatalog.DeleteOAuthModelAliasChannel(doc, channel) {
				return newConfigMutationError(http.StatusNotFound, "channel not found")
			}
			return nil
		},
	)
	if ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// codex-api-key: []CodexKey

func (h *Handler) GetCodexKeys(c *gin.Context) {
	writeRedactedJSON(c, 200, gin.H{"codex-api-key": h.codexKeysWithAuthIndex()})
}
func (h *Handler) PutCodexKeys(c *gin.Context)   { putStructList(h, c, codexKeyResource) }
func (h *Handler) PostCodexKey(c *gin.Context)   { postStructEntry(h, c, codexKeyResource) }
func (h *Handler) DeleteCodexKey(c *gin.Context) { deleteStructEntry(h, c, codexKeyResource) }
func (h *Handler) PatchCodexKey(c *gin.Context) {
	patchStructEntry(h, c, codexKeyResource, patchCodexStyleKey)
}

// xai-api-key: []XAIKey

func (h *Handler) GetXAIKeys(c *gin.Context) {
	writeRedactedJSON(c, http.StatusOK, gin.H{"xai-api-key": h.xaiKeysWithAuthIndex()})
}
func (h *Handler) PutXAIKeys(c *gin.Context)   { putStructList(h, c, xaiKeyResource) }
func (h *Handler) PostXAIKey(c *gin.Context)   { postStructEntry(h, c, xaiKeyResource) }
func (h *Handler) DeleteXAIKey(c *gin.Context) { deleteStructEntry(h, c, xaiKeyResource) }
func (h *Handler) PatchXAIKey(c *gin.Context) {
	patchStructEntry(h, c, xaiKeyResource, patchCodexStyleKey)
}

func patchCodexStyleKey(entry *config.CodexKey, raw json.RawMessage) error {
	var p struct {
		APIKey         *string              `json:"api-key"`
		Priority       *int                 `json:"priority"`
		Weight         json.RawMessage      `json:"weight"`
		Prefix         *string              `json:"prefix"`
		BaseURL        *string              `json:"base-url"`
		Websockets     *bool                `json:"websockets"`
		AlphaSearch    *bool                `json:"alpha-search"`
		ProxyURL       *string              `json:"proxy-url"`
		Models         *[]config.CodexModel `json:"models"`
		Headers        *map[string]string   `json:"headers"`
		ExcludedModels *[]string            `json:"excluded-models"`
		DisableCooling *bool                `json:"disable-cooling"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	setTrimmed(p.APIKey, &entry.APIKey)
	if p.Priority != nil {
		entry.Priority = *p.Priority
	}
	if err := setCredentialWeight(p.Weight, &entry.Weight); err != nil {
		return err
	}
	setTrimmed(p.Prefix, &entry.Prefix)
	if err := setRequiredBaseURL(p.BaseURL, &entry.BaseURL); err != nil {
		return err
	}
	if p.Websockets != nil {
		entry.Websockets = *p.Websockets
	}
	if p.AlphaSearch != nil {
		entry.AlphaSearch = *p.AlphaSearch
	}
	setTrimmed(p.ProxyURL, &entry.ProxyURL)
	if p.Models != nil {
		entry.Models = append([]config.CodexModel(nil), *p.Models...)
	}
	setHeaders(p.Headers, &entry.Headers)
	setExcludedModels(p.ExcludedModels, &entry.ExcludedModels)
	if p.DisableCooling != nil {
		entry.DisableCooling = *p.DisableCooling
	}
	return nil
}

func claudeKeyRouteEqual(left, right *config.ClaudeKey) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.APIKey == right.APIKey &&
		left.BaseURL == right.BaseURL &&
		left.Priority == right.Priority &&
		reflect.DeepEqual(left.Weight, right.Weight) &&
		left.Prefix == right.Prefix &&
		left.ProxyURL == right.ProxyURL &&
		diff.ComputeClaudeModelsHash(left.Models) == diff.ComputeClaudeModelsHash(right.Models) &&
		maps.Equal(left.Headers, right.Headers) &&
		diff.ComputeExcludedModelsHash(left.ExcludedModels) == diff.ComputeExcludedModelsHash(right.ExcludedModels) &&
		left.RebuildMidSystemMessage == right.RebuildMidSystemMessage &&
		reflect.DeepEqual(left.Cloak, right.Cloak) &&
		left.ExperimentalCCHSigning == right.ExperimentalCCHSigning
}

func codexKeyRouteEqual(left, right *config.CodexKey) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.APIKey == right.APIKey &&
		left.BaseURL == right.BaseURL &&
		left.Priority == right.Priority &&
		reflect.DeepEqual(left.Weight, right.Weight) &&
		left.Prefix == right.Prefix &&
		left.Websockets == right.Websockets &&
		left.AlphaSearch == right.AlphaSearch &&
		left.ProxyURL == right.ProxyURL &&
		diff.ComputeCodexModelsHash(left.Models) == diff.ComputeCodexModelsHash(right.Models) &&
		maps.Equal(left.Headers, right.Headers) &&
		diff.ComputeExcludedModelsHash(left.ExcludedModels) == diff.ComputeExcludedModelsHash(right.ExcludedModels) &&
		left.DisableCooling == right.DisableCooling
}

func xaiKeyRouteEqual(left, right *config.XAIKey) bool {
	return codexKeyRouteEqual(left, right)
}

func validateGeminiKey(entry *config.GeminiKey, existing *config.GeminiKey) error {
	if entry == nil {
		return nil
	}
	if err := validateResolvedAPIKeyPlaceholder(entry.APIKey); err != nil {
		return err
	}
	if err := validateBaseURL(entry.BaseURL); err != nil {
		return err
	}
	if err := config.ValidateCredentialWeight(entry.Weight); err != nil {
		return err
	}
	var existingHeaders map[string]string
	if existing != nil {
		existingHeaders = existing.Headers
	}
	return validateConfigHeaders(entry.Headers, existingHeaders)
}

func validateClaudeKey(entry *config.ClaudeKey, existing *config.ClaudeKey) error {
	if entry == nil {
		return nil
	}
	if err := validateResolvedAPIKeyPlaceholder(entry.APIKey); err != nil {
		return err
	}
	if err := validateBaseURL(entry.BaseURL); err != nil {
		return err
	}
	if err := config.ValidateCredentialWeight(entry.Weight); err != nil {
		return err
	}
	var existingHeaders map[string]string
	if existing != nil {
		existingHeaders = existing.Headers
	}
	return validateConfigHeaders(entry.Headers, existingHeaders)
}

func validateOpenAICompatibilityEntry(entry *config.OpenAICompatibility, existing *config.OpenAICompatibility) error {
	if entry == nil {
		return nil
	}
	if err := validateBaseURL(entry.BaseURL); err != nil {
		return err
	}
	for i := range entry.APIKeyEntries {
		if err := validateResolvedAPIKeyPlaceholder(entry.APIKeyEntries[i].APIKey); err != nil {
			return err
		}
		if err := config.ValidateCredentialWeight(entry.APIKeyEntries[i].Weight); err != nil {
			return err
		}
	}
	var existingHeaders map[string]string
	if existing != nil {
		existingHeaders = existing.Headers
	}
	return validateConfigHeaders(entry.Headers, existingHeaders)
}

func validateVertexCompatKey(entry *config.VertexCompatKey, existing *config.VertexCompatKey) error {
	if entry == nil {
		return nil
	}
	if err := validateResolvedAPIKeyPlaceholder(entry.APIKey); err != nil {
		return err
	}
	if err := validateBaseURL(entry.BaseURL); err != nil {
		return err
	}
	if err := config.ValidateCredentialWeight(entry.Weight); err != nil {
		return err
	}
	var existingHeaders map[string]string
	if existing != nil {
		existingHeaders = existing.Headers
	}
	return validateConfigHeaders(entry.Headers, existingHeaders)
}

func validateCodexKey(entry *config.CodexKey, existing *config.CodexKey) error {
	if entry == nil {
		return nil
	}
	if err := validateResolvedAPIKeyPlaceholder(entry.APIKey); err != nil {
		return err
	}
	if err := validateBaseURL(entry.BaseURL); err != nil {
		return err
	}
	if err := config.ValidateCredentialWeight(entry.Weight); err != nil {
		return err
	}
	var existingHeaders map[string]string
	if existing != nil {
		existingHeaders = existing.Headers
	}
	return validateConfigHeaders(entry.Headers, existingHeaders)
}

func validateXAIKey(entry *config.XAIKey, existing *config.XAIKey) error {
	if entry == nil {
		return nil
	}
	if err := validateResolvedAPIKeyPlaceholder(entry.APIKey); err != nil {
		return err
	}
	if strings.TrimSpace(entry.APIKey) == "" {
		return fmt.Errorf("api-key is required")
	}
	if strings.TrimSpace(entry.BaseURL) == "" {
		return fmt.Errorf("base-url is required")
	}
	return validateCodexKey(entry, existing)
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
	entry.Prefix = config.NormalizeModelPrefix(entry.Prefix)
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
	entry.Prefix = config.NormalizeModelPrefix(entry.Prefix)
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

func normalizeXAIKey(entry *config.XAIKey) {
	normalizeCodexKey(entry)
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

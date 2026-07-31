package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

var errEntryDeleted = errors.New("entry deleted")

// structListResource describes a []T configuration list for generic CRUD operations.
type structListResource[T any] struct {
	configKey                string // JSON response key (e.g., "gemini-api-key")
	getList                  func(cfg *config.Config) []T
	setList                  func(cfg *config.Config, list []T)
	sanitize                 func(cfg *config.Config)
	normalize                func(entry *T)                    // per-entry normalization (nil = skip)
	validate                 func(entry *T, existing *T) error // per-entry validation (nil = skip)
	matchKey                 func(entry *T) string             // extract match field value
	matchQueryParam          string                            // Delete query param name (e.g., "api-key")
	secondaryMatchKey        func(entry *T) string             // optional secondary delete match (e.g., base-url)
	secondaryMatchQueryParam string                            // optional secondary delete query param
	matchBodyField           string                            // Patch body field for match lookup ("match" or "name")
	stableMatchBodyField     string                            // Patch body field for stable runtime identity lookup
	stableMatchIndex         func(h *Handler, cfg *config.Config, value string) int
	trimMatch                bool                // whether to TrimSpace match values in Delete
	trimSecondaryMatch       bool                // whether to TrimSpace secondary match values in Delete
	filter                   func(entry *T) bool // optional PUT filter (nil = keep all)
	prepareGet               func(list []T) []T  // optional GET transform (nil = return as-is)
	strictDelete             bool                // true = return 404 when item not found in Delete
}

func putStructList[T any](h *Handler, c *gin.Context, r structListResource[T]) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := handlers.ReadRequestBodyWithLimit(c, 10<<20)
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var submitted []T
	if err = json.Unmarshal(data, &submitted); err != nil {
		var obj struct {
			Items []T `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		submitted = obj.Items
	}

	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		arr := append([]T(nil), submitted...)
		if r.normalize != nil {
			for i := range arr {
				r.normalize(&arr[i])
			}
		}
		existing := r.getList(next)
		var matches []*T
		arr, matches, err = resolveStructListAPIKeyPlaceholders(arr, existing, r)
		if err != nil {
			return err
		}
		if r.validate != nil {
			for i := range arr {
				if errValidate := r.validate(&arr[i], matches[i]); errValidate != nil {
					return errValidate
				}
			}
		}
		if r.filter != nil {
			filtered := make([]T, 0, len(arr))
			for i := range arr {
				if r.filter(&arr[i]) {
					filtered = append(filtered, arr[i])
				}
			}
			arr = filtered
		}
		r.setList(next, arr)
		r.sanitize(next)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func deleteStructEntry[T any](h *Handler, c *gin.Context, r structListResource[T]) {
	matchValue, hasMatch := deleteStructEntryMatch(c, r)
	secondaryValue, hasSecondary := deleteStructEntrySecondaryMatch(c, r)
	indexValue, hasIndex := deleteStructEntryIndex(c)

	if !hasMatch && !hasIndex && c.Request.Body != nil && c.Request.ContentLength != 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
		data, err := handlers.ReadRequestBodyWithLimit(c, 10<<20)
		if err != nil {
			c.JSON(400, gin.H{"error": "failed to read body"})
			return
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			var body map[string]json.RawMessage
			if err = json.Unmarshal(data, &body); err != nil {
				c.JSON(400, gin.H{"error": "invalid body"})
				return
			}
			if value, ok := stringFieldFromRaw(body, r.matchQueryParam); ok {
				matchValue = value
				hasMatch = true
			}
			if value, ok := stringFieldFromRaw(body, r.secondaryMatchQueryParam); ok {
				secondaryValue = value
				hasSecondary = true
			}
			if value, ok := intFieldFromRaw(body, "index"); ok {
				indexValue = value
				hasIndex = true
			}
		}
	}

	if !hasMatch && !hasIndex {
		c.JSON(400, gin.H{"error": fmt.Sprintf("missing %s or index", r.matchQueryParam)})
		return
	}
	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		list := r.getList(next)
		if hasMatch {
			resolvedMatch := matchValue
			resolvedSecondary := secondaryValue
			if r.trimMatch {
				resolvedMatch = strings.TrimSpace(resolvedMatch)
			}
			if r.trimSecondaryMatch {
				resolvedSecondary = strings.TrimSpace(resolvedSecondary)
			}
			if r.matchQueryParam == "api-key" {
				var errResolve error
				resolvedMatch, errResolve = resolveStructMatchPlaceholder(
					list, r, resolvedMatch, resolvedSecondary, hasSecondary,
				)
				if errResolve != nil {
					return errResolve
				}
			}
			out := make([]T, 0, len(list))
			for i := range list {
				matched := r.matchKey(&list[i]) == resolvedMatch
				if matched && hasSecondary && r.secondaryMatchKey != nil {
					matched = r.secondaryMatchKey(&list[i]) == resolvedSecondary
				}
				if !matched {
					out = append(out, list[i])
				}
			}
			if r.strictDelete && len(out) == len(list) {
				return newConfigMutationError(http.StatusNotFound, "item not found")
			}
			r.setList(next, out)
			r.sanitize(next)
			return nil
		}
		if indexValue < 0 || indexValue >= len(list) {
			return newConfigMutationError(
				http.StatusBadRequest,
				fmt.Sprintf("missing %s or index", r.matchQueryParam),
			)
		}
		list = append(list[:indexValue], list[indexValue+1:]...)
		r.setList(next, list)
		r.sanitize(next)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func deleteStructEntryMatch[T any](c *gin.Context, r structListResource[T]) (string, bool) {
	value := c.Query(r.matchQueryParam)
	if value == "" {
		return "", false
	}
	return value, true
}

func deleteStructEntrySecondaryMatch[T any](c *gin.Context, r structListResource[T]) (string, bool) {
	if r.secondaryMatchQueryParam == "" {
		return "", false
	}
	return c.GetQuery(r.secondaryMatchQueryParam)
}

func deleteStructEntryIndex(c *gin.Context) (int, bool) {
	idxStr := c.Query("index")
	if idxStr == "" {
		return 0, false
	}
	var idx int
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
		return 0, false
	}
	return idx, true
}

func stringFieldFromRaw(fields map[string]json.RawMessage, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	raw, ok := fields[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func intFieldFromRaw(fields map[string]json.RawMessage, name string) (int, bool) {
	raw, ok := fields[name]
	if !ok {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func patchStructEntry[T any](
	h *Handler, c *gin.Context, r structListResource[T],
	apply func(entry *T, rawValue json.RawMessage) error,
) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := handlers.ReadRequestBodyWithLimit(c, 10<<20)
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}

	var envelope struct {
		Index *int            `json:"index"`
		Value json.RawMessage `json:"value"`
	}
	if err = json.Unmarshal(data, &envelope); err != nil || len(envelope.Value) == 0 {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	var matchValue *string
	var stableMatchValue *string
	bodyField := r.matchBodyField
	if bodyField == "" {
		bodyField = "match"
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(data, &raw); err == nil {
		if matchRaw, ok := raw[bodyField]; ok {
			var s string
			if json.Unmarshal(matchRaw, &s) == nil {
				matchValue = &s
			}
		}
		if r.stableMatchBodyField != "" {
			if stableRaw, ok := raw[r.stableMatchBodyField]; ok {
				var s string
				if err = json.Unmarshal(stableRaw, &s); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid stable item identity"})
					return
				}
				stableMatchValue = &s
			}
		}
	}

	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		list := r.getList(next)
		targetIndex := -1
		if stableMatchValue != nil && r.stableMatchIndex != nil {
			stableMatch := strings.TrimSpace(*stableMatchValue)
			if stableMatch == "" {
				return newConfigMutationError(http.StatusBadRequest, "stable item identity is empty")
			}
			targetIndex = r.stableMatchIndex(h, next, stableMatch)
		} else if matchValue != nil {
			match := strings.TrimSpace(*matchValue)
			if match != "" {
				if r.matchQueryParam == "api-key" {
					var errResolve error
					match, errResolve = resolveStructMatchPlaceholder(list, r, match, "", false)
					if errResolve != nil {
						return errResolve
					}
				}
				for i := range list {
					if r.matchKey(&list[i]) == match {
						targetIndex = i
						break
					}
				}
			}
		} else if envelope.Index != nil && *envelope.Index >= 0 && *envelope.Index < len(list) {
			targetIndex = *envelope.Index
		}
		if targetIndex == -1 {
			return newConfigMutationError(http.StatusNotFound, "item not found")
		}

		entry := list[targetIndex]
		if errApply := apply(&entry, envelope.Value); errApply != nil {
			if errors.Is(errApply, errEntryDeleted) {
				list = append(list[:targetIndex], list[targetIndex+1:]...)
				r.setList(next, list)
				r.sanitize(next)
				return nil
			}
			return errApply
		}
		if r.normalize != nil {
			r.normalize(&entry)
		}
		if errResolve := resolveStructEntryAPIKeyPlaceholders(
			&entry,
			list[targetIndex],
			collectSensitiveStringsFromStructList(list),
		); errResolve != nil {
			return errResolve
		}
		if r.validate != nil {
			if errValidate := r.validate(&entry, &list[targetIndex]); errValidate != nil {
				return errValidate
			}
		}
		list[targetIndex] = entry
		r.setList(next, list)
		r.sanitize(next)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// --- Resource descriptors ---

var geminiKeyResource = structListResource[config.GeminiKey]{
	configKey:                "gemini-api-key",
	getList:                  func(cfg *config.Config) []config.GeminiKey { return cfg.GeminiKey },
	setList:                  func(cfg *config.Config, list []config.GeminiKey) { cfg.GeminiKey = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeGeminiKeys() },
	validate:                 validateGeminiKey,
	matchKey:                 func(e *config.GeminiKey) string { return e.APIKey },
	secondaryMatchKey:        func(e *config.GeminiKey) string { return e.BaseURL },
	matchQueryParam:          "api-key",
	secondaryMatchQueryParam: "base-url",
	trimMatch:                true,
	trimSecondaryMatch:       true,
	strictDelete:             true,
}

var claudeKeyResource = structListResource[config.ClaudeKey]{
	configKey:                "claude-api-key",
	getList:                  func(cfg *config.Config) []config.ClaudeKey { return cfg.ClaudeKey },
	setList:                  func(cfg *config.Config, list []config.ClaudeKey) { cfg.ClaudeKey = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeClaudeKeys() },
	normalize:                normalizeClaudeKey,
	validate:                 validateClaudeKey,
	matchKey:                 func(e *config.ClaudeKey) string { return e.APIKey },
	secondaryMatchKey:        func(e *config.ClaudeKey) string { return e.BaseURL },
	matchQueryParam:          "api-key",
	secondaryMatchQueryParam: "base-url",
	stableMatchBodyField:     "auth-index",
	stableMatchIndex: func(h *Handler, cfg *config.Config, value string) int {
		views := h.buildConfigAuthIndexViews(cfg)
		for i := range views.claude {
			if views.claude[i] == value {
				return i
			}
		}
		return -1
	},
	trimSecondaryMatch: true,
}

var openAICompatResource = structListResource[config.OpenAICompatibility]{
	configKey:                "openai-compatibility",
	getList:                  func(cfg *config.Config) []config.OpenAICompatibility { return cfg.OpenAICompatibility },
	setList:                  func(cfg *config.Config, list []config.OpenAICompatibility) { cfg.OpenAICompatibility = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeOpenAICompatibility() },
	normalize:                normalizeOpenAICompatibilityEntry,
	validate:                 validateOpenAICompatibilityEntry,
	matchKey:                 func(e *config.OpenAICompatibility) string { return e.Name },
	secondaryMatchKey:        func(e *config.OpenAICompatibility) string { return e.BaseURL },
	matchQueryParam:          "name",
	secondaryMatchQueryParam: "base-url",
	matchBodyField:           "name",
	filter:                   func(e *config.OpenAICompatibility) bool { return strings.TrimSpace(e.BaseURL) != "" },
	prepareGet:               normalizedOpenAICompatibilityEntries,
}

var vertexCompatKeyResource = structListResource[config.VertexCompatKey]{
	configKey:                "vertex-api-key",
	getList:                  func(cfg *config.Config) []config.VertexCompatKey { return cfg.VertexCompatAPIKey },
	setList:                  func(cfg *config.Config, list []config.VertexCompatKey) { cfg.VertexCompatAPIKey = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeVertexCompatKeys() },
	normalize:                normalizeVertexCompatKey,
	validate:                 validateVertexCompatKey,
	matchKey:                 func(e *config.VertexCompatKey) string { return e.APIKey },
	secondaryMatchKey:        func(e *config.VertexCompatKey) string { return e.BaseURL },
	matchQueryParam:          "api-key",
	secondaryMatchQueryParam: "base-url",
	trimMatch:                true,
	trimSecondaryMatch:       true,
}

var codexKeyResource = structListResource[config.CodexKey]{
	configKey:                "codex-api-key",
	getList:                  func(cfg *config.Config) []config.CodexKey { return cfg.CodexKey },
	setList:                  func(cfg *config.Config, list []config.CodexKey) { cfg.CodexKey = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeCodexKeys() },
	normalize:                normalizeCodexKey,
	validate:                 validateCodexKey,
	matchKey:                 func(e *config.CodexKey) string { return e.APIKey },
	secondaryMatchKey:        func(e *config.CodexKey) string { return e.BaseURL },
	matchQueryParam:          "api-key",
	secondaryMatchQueryParam: "base-url",
	trimSecondaryMatch:       true,
	filter:                   func(e *config.CodexKey) bool { return e.BaseURL != "" },
}

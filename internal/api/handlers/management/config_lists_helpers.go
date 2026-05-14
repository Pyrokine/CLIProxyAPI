package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

var errEntryDeleted = errors.New("entry deleted")

// structListResource describes a []T configuration list for generic CRUD operations.
type structListResource[T any] struct {
	configKey                string // JSON response key (e.g., "gemini-api-key")
	getList                  func(cfg *config.Config) []T
	setList                  func(cfg *config.Config, list []T)
	sanitize                 func(cfg *config.Config)
	normalize                func(entry *T)        // per-entry normalization (nil = skip)
	matchKey                 func(entry *T) string // extract match field value
	matchQueryParam          string                // Delete query param name (e.g., "api-key")
	secondaryMatchKey        func(entry *T) string // optional secondary delete match (e.g., base-url)
	secondaryMatchQueryParam string                // optional secondary delete query param
	matchBodyField           string                // Patch body field for match lookup ("match" or "name")
	trimMatch                bool                  // whether to TrimSpace match values in Delete
	trimSecondaryMatch       bool                  // whether to TrimSpace secondary match values in Delete
	filter                   func(entry *T) bool   // optional PUT filter (nil = keep all)
	prepareGet               func(list []T) []T    // optional GET transform (nil = return as-is)
	strictDelete             bool                  // true = return 404 when item not found in Delete
}

func getStructList[T any](h *Handler, c *gin.Context, r structListResource[T]) {
	list := r.getList(h.cfg)
	if r.prepareGet != nil {
		list = r.prepareGet(list)
	}
	c.JSON(200, gin.H{r.configKey: list})
}

func putStructList[T any](h *Handler, c *gin.Context, r structListResource[T]) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []T
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []T `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if r.normalize != nil {
		for i := range arr {
			r.normalize(&arr[i])
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
	r.setList(h.cfg, arr)
	r.sanitize(h.cfg)
	h.persist(c)
}

func deleteStructEntry[T any](h *Handler, c *gin.Context, r structListResource[T]) {
	list := r.getList(h.cfg)

	if val := c.Query(r.matchQueryParam); val != "" {
		if r.trimMatch {
			val = strings.TrimSpace(val)
		}
		secondaryVal, hasSecondary := "", false
		if r.secondaryMatchQueryParam != "" {
			secondaryVal, hasSecondary = c.GetQuery(r.secondaryMatchQueryParam)
			if hasSecondary && r.trimSecondaryMatch {
				secondaryVal = strings.TrimSpace(secondaryVal)
			}
		}
		out := make([]T, 0, len(list))
		for i := range list {
			matched := r.matchKey(&list[i]) == val
			if matched && hasSecondary && r.secondaryMatchKey != nil {
				matched = r.secondaryMatchKey(&list[i]) == secondaryVal
			}
			if !matched {
				out = append(out, list[i])
			}
		}
		if r.strictDelete && len(out) == len(list) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		r.setList(h.cfg, out)
		r.sanitize(h.cfg)
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && idx >= 0 && idx < len(list) {
			list = append(list[:idx], list[idx+1:]...)
			r.setList(h.cfg, list)
			r.sanitize(h.cfg)
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": fmt.Sprintf("missing %s or index", r.matchQueryParam)})
}

func patchStructEntry[T any](
	h *Handler, c *gin.Context, r structListResource[T],
	apply func(entry *T, rawValue json.RawMessage) error,
) {

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}

	// Extract common fields from raw JSON
	var envelope struct {
		Index *int            `json:"index"`
		Value json.RawMessage `json:"value"`
	}
	if err = json.Unmarshal(data, &envelope); err != nil || len(envelope.Value) == 0 {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	// Extract the match field (either "match" or "name" depending on resource)
	var matchValue *string
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
	}

	list := r.getList(h.cfg)
	targetIndex := -1
	if envelope.Index != nil && *envelope.Index >= 0 && *envelope.Index < len(list) {
		targetIndex = *envelope.Index
	}
	if targetIndex == -1 && matchValue != nil {
		match := strings.TrimSpace(*matchValue)
		if match != "" {
			for i := range list {
				if r.matchKey(&list[i]) == match {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := list[targetIndex]
	if err = apply(&entry, envelope.Value); err != nil {
		if errors.Is(err, errEntryDeleted) {
			list = append(list[:targetIndex], list[targetIndex+1:]...)
			r.setList(h.cfg, list)
			r.sanitize(h.cfg)
			h.persist(c)
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if r.normalize != nil {
		r.normalize(&entry)
	}
	list[targetIndex] = entry
	r.setList(h.cfg, list)
	r.sanitize(h.cfg)
	h.persist(c)
}

// --- Resource descriptors ---

var geminiKeyResource = structListResource[config.GeminiKey]{
	configKey:                "gemini-api-key",
	getList:                  func(cfg *config.Config) []config.GeminiKey { return cfg.GeminiKey },
	setList:                  func(cfg *config.Config, list []config.GeminiKey) { cfg.GeminiKey = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeGeminiKeys() },
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
	matchKey:                 func(e *config.ClaudeKey) string { return e.APIKey },
	secondaryMatchKey:        func(e *config.ClaudeKey) string { return e.BaseURL },
	matchQueryParam:          "api-key",
	secondaryMatchQueryParam: "base-url",
	trimSecondaryMatch:       true,
}

var openAICompatResource = structListResource[config.OpenAICompatibility]{
	configKey:       "openai-compatibility",
	getList:         func(cfg *config.Config) []config.OpenAICompatibility { return cfg.OpenAICompatibility },
	setList:         func(cfg *config.Config, list []config.OpenAICompatibility) { cfg.OpenAICompatibility = list },
	sanitize:        func(cfg *config.Config) { cfg.SanitizeOpenAICompatibility() },
	normalize:       normalizeOpenAICompatibilityEntry,
	matchKey:        func(e *config.OpenAICompatibility) string { return e.Name },
	matchQueryParam: "name",
	matchBodyField:  "name",
	filter:          func(e *config.OpenAICompatibility) bool { return strings.TrimSpace(e.BaseURL) != "" },
	prepareGet:      normalizedOpenAICompatibilityEntries,
}

var vertexCompatKeyResource = structListResource[config.VertexCompatKey]{
	configKey:                "vertex-api-key",
	getList:                  func(cfg *config.Config) []config.VertexCompatKey { return cfg.VertexCompatAPIKey },
	setList:                  func(cfg *config.Config, list []config.VertexCompatKey) { cfg.VertexCompatAPIKey = list },
	sanitize:                 func(cfg *config.Config) { cfg.SanitizeVertexCompatKeys() },
	normalize:                normalizeVertexCompatKey,
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
	matchKey:                 func(e *config.CodexKey) string { return e.APIKey },
	secondaryMatchKey:        func(e *config.CodexKey) string { return e.BaseURL },
	matchQueryParam:          "api-key",
	secondaryMatchQueryParam: "base-url",
	trimSecondaryMatch:       true,
	filter:                   func(e *config.CodexKey) bool { return e.BaseURL != "" },
}

package management

import (
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
)

type configAuthIndexViews struct {
	gemini         []string
	interactions   []string
	claude         []string
	codex          []string
	meta           []string
	xai            []string
	vertex         []string
	openAIEntries  [][]string
	openAIFallback []string
}

type configAuthIndexResponse struct {
	Gemini         []string   `json:"gemini"`
	Interactions   []string   `json:"interactions"`
	Claude         []string   `json:"claude"`
	Codex          []string   `json:"codex"`
	Meta           []string   `json:"meta"`
	XAI            []string   `json:"xai"`
	Vertex         []string   `json:"vertex"`
	OpenAIEntries  [][]string `json:"openai-entries"`
	OpenAIFallback []string   `json:"openai-fallback"`
}

func (views configAuthIndexViews) response() configAuthIndexResponse {
	return configAuthIndexResponse{
		Gemini:         views.gemini,
		Interactions:   views.interactions,
		Claude:         views.claude,
		Codex:          views.codex,
		Meta:           views.meta,
		XAI:            views.xai,
		Vertex:         views.vertex,
		OpenAIEntries:  views.openAIEntries,
		OpenAIFallback: views.openAIFallback,
	}
}

type geminiKeyWithAuthIndex struct {
	config.GeminiKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type claudeKeyWithAuthIndex struct {
	config.ClaudeKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type codexKeyWithAuthIndex struct {
	config.CodexKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type xaiKeyWithAuthIndex struct {
	config.XAIKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type vertexCompatKeyWithAuthIndex struct {
	config.VertexCompatKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityAPIKeyWithAuthIndex struct {
	config.OpenAICompatibilityAPIKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityWithAuthIndex struct {
	Name                  string                                   `json:"name"`
	Priority              int                                      `json:"priority,omitempty"`
	Disabled              bool                                     `json:"disabled,omitempty"`
	Prefix                string                                   `json:"prefix,omitempty"`
	BaseURL               string                                   `json:"base-url"`
	APIKeyEntries         []openAICompatibilityAPIKeyWithAuthIndex `json:"api-key-entries,omitempty"`
	Models                []config.OpenAICompatibilityModel        `json:"models,omitempty"`
	Headers               map[string]string                        `json:"headers,omitempty"`
	SupportPromptCacheKey bool                                     `json:"support-prompt-cache-key,omitempty"`
	DisableCooling        *bool                                    `json:"disable-cooling,omitempty"`
	RequestRetry          *int                                     `json:"request-retry,omitempty"`
	RequestScopedErrors   []config.RequestScopedErrorRule          `json:"request-scoped-errors,omitempty"`
	AuthIndex             string                                   `json:"auth-index,omitempty"`
}

func (h *Handler) buildConfigAuthIndexViews(cfg *config.Config) configAuthIndexViews {
	if h == nil || cfg == nil {
		return configAuthIndexViews{}
	}

	liveIndexByID := map[string]string{}
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil || strings.TrimSpace(auth.ID) == "" {
				continue
			}
			auth.EnsureIndex()
			if auth.Index == "" {
				continue
			}
			liveIndexByID[auth.ID] = auth.Index
		}
	}

	views := configAuthIndexViews{
		gemini:         make([]string, len(cfg.GeminiKey)),
		interactions:   make([]string, len(cfg.InteractionsKey)),
		claude:         make([]string, len(cfg.ClaudeKey)),
		codex:          make([]string, len(cfg.CodexKey)),
		meta:           make([]string, len(cfg.MetaKey)),
		xai:            make([]string, len(cfg.XAIKey)),
		vertex:         make([]string, len(cfg.VertexCompatAPIKey)),
		openAIEntries:  make([][]string, len(cfg.OpenAICompatibility)),
		openAIFallback: make([]string, len(cfg.OpenAICompatibility)),
	}

	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      cfg,
			Now:         time.Now(),
			IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if errSynthesize != nil {
		return views
	}

	type authIndexQueues struct {
		gemini       []string
		interactions []string
		claude       []string
		codex        []string
		meta         []string
		xai          []string
		vertex       []string
		openAI       []string
	}
	queues := authIndexQueues{}
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		index := liveIndexByID[auth.ID]
		switch {
		case strings.HasPrefix(auth.ID, "gemini:apikey:"):
			queues.gemini = append(queues.gemini, index)
		case strings.HasPrefix(auth.ID, "gemini-interactions:apikey:"):
			queues.interactions = append(queues.interactions, index)
		case strings.HasPrefix(auth.ID, "claude:apikey:"):
			queues.claude = append(queues.claude, index)
		case strings.HasPrefix(auth.ID, "codex:apikey:"):
			queues.codex = append(queues.codex, index)
		case strings.HasPrefix(auth.ID, "meta:apikey:"):
			queues.meta = append(queues.meta, index)
		case strings.HasPrefix(auth.ID, "xai:apikey:"):
			queues.xai = append(queues.xai, index)
		case strings.HasPrefix(auth.ID, "vertex:apikey:"):
			queues.vertex = append(queues.vertex, index)
		case strings.HasPrefix(auth.ID, "openai-compatibility:"):
			queues.openAI = append(queues.openAI, index)
		}
	}

	nextAuthIndex := func(indexes []string, cursor *int) string {
		if *cursor >= len(indexes) {
			return ""
		}
		index := indexes[*cursor]
		*cursor++
		return index
	}

	geminiCursor := 0
	for i := range cfg.GeminiKey {
		if strings.TrimSpace(cfg.GeminiKey[i].APIKey) == "" {
			continue
		}
		views.gemini[i] = nextAuthIndex(queues.gemini, &geminiCursor)
	}
	interactionsCursor := 0
	for i := range cfg.InteractionsKey {
		if strings.TrimSpace(cfg.InteractionsKey[i].APIKey) == "" {
			continue
		}
		views.interactions[i] = nextAuthIndex(queues.interactions, &interactionsCursor)
	}
	claudeCursor := 0
	for i := range cfg.ClaudeKey {
		if strings.TrimSpace(cfg.ClaudeKey[i].APIKey) == "" {
			continue
		}
		views.claude[i] = nextAuthIndex(queues.claude, &claudeCursor)
	}
	codexCursor := 0
	for i := range cfg.CodexKey {
		if strings.TrimSpace(cfg.CodexKey[i].APIKey) == "" {
			continue
		}
		views.codex[i] = nextAuthIndex(queues.codex, &codexCursor)
	}
	metaCursor := 0
	for i := range cfg.MetaKey {
		if strings.TrimSpace(cfg.MetaKey[i].APIKey) == "" {
			continue
		}
		views.meta[i] = nextAuthIndex(queues.meta, &metaCursor)
	}
	xaiCursor := 0
	for i := range cfg.XAIKey {
		if strings.TrimSpace(cfg.XAIKey[i].APIKey) == "" {
			continue
		}
		views.xai[i] = nextAuthIndex(queues.xai, &xaiCursor)
	}
	openAICursor := 0
	for i := range cfg.OpenAICompatibility {
		if cfg.OpenAICompatibility[i].Disabled {
			continue
		}
		entries := cfg.OpenAICompatibility[i].APIKeyEntries
		if len(entries) == 0 {
			views.openAIFallback[i] = nextAuthIndex(queues.openAI, &openAICursor)
			continue
		}

		views.openAIEntries[i] = make([]string, len(entries))
		for j := range entries {
			views.openAIEntries[i][j] = nextAuthIndex(queues.openAI, &openAICursor)
		}
	}
	vertexCursor := 0
	for i := range cfg.VertexCompatAPIKey {
		if strings.TrimSpace(cfg.VertexCompatAPIKey[i].APIKey) == "" {
			continue
		}
		views.vertex[i] = nextAuthIndex(queues.vertex, &vertexCursor)
	}

	return views
}

func (h *Handler) geminiKeysWithAuthIndex() []geminiKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]geminiKeyWithAuthIndex, len(cfg.GeminiKey))
	for i := range cfg.GeminiKey {
		out[i] = geminiKeyWithAuthIndex{
			GeminiKey: cfg.GeminiKey[i],
			AuthIndex: views.gemini[i],
		}
	}
	return out
}

func (h *Handler) interactionsKeysWithAuthIndex() []geminiKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]geminiKeyWithAuthIndex, len(cfg.InteractionsKey))
	for i := range cfg.InteractionsKey {
		out[i] = geminiKeyWithAuthIndex{
			GeminiKey: cfg.InteractionsKey[i],
			AuthIndex: views.interactions[i],
		}
	}
	return out
}

func (h *Handler) claudeKeysWithAuthIndex() []claudeKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]claudeKeyWithAuthIndex, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		out[i] = claudeKeyWithAuthIndex{
			ClaudeKey: cfg.ClaudeKey[i],
			AuthIndex: views.claude[i],
		}
	}
	return out
}

func (h *Handler) codexKeysWithAuthIndex() []codexKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]codexKeyWithAuthIndex, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		out[i] = codexKeyWithAuthIndex{
			CodexKey:  cfg.CodexKey[i],
			AuthIndex: views.codex[i],
		}
	}
	return out
}

func (h *Handler) metaKeysWithAuthIndex() []codexKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]codexKeyWithAuthIndex, len(cfg.MetaKey))
	for i := range cfg.MetaKey {
		out[i] = codexKeyWithAuthIndex{
			CodexKey:  cfg.MetaKey[i],
			AuthIndex: views.meta[i],
		}
	}
	return out
}

func (h *Handler) xaiKeysWithAuthIndex() []xaiKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]xaiKeyWithAuthIndex, len(cfg.XAIKey))
	for i := range cfg.XAIKey {
		out[i] = xaiKeyWithAuthIndex{
			XAIKey:    cfg.XAIKey[i],
			AuthIndex: views.xai[i],
		}
	}
	return out
}

func (h *Handler) vertexCompatKeysWithAuthIndex() []vertexCompatKeyWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	views := h.buildConfigAuthIndexViews(cfg)
	out := make([]vertexCompatKeyWithAuthIndex, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		out[i] = vertexCompatKeyWithAuthIndex{
			VertexCompatKey: cfg.VertexCompatAPIKey[i],
			AuthIndex:       views.vertex[i],
		}
	}
	return out
}

func (h *Handler) openAICompatibilityWithAuthIndex() []openAICompatibilityWithAuthIndex {
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}

	views := h.buildConfigAuthIndexViews(cfg)
	normalized := normalizedOpenAICompatibilityEntries(cfg.OpenAICompatibility)
	out := make([]openAICompatibilityWithAuthIndex, len(normalized))
	for i := range normalized {
		entry := normalized[i]
		response := openAICompatibilityWithAuthIndex{
			Name:                  entry.Name,
			Priority:              entry.Priority,
			Disabled:              entry.Disabled,
			Prefix:                entry.Prefix,
			BaseURL:               entry.BaseURL,
			Models:                entry.Models,
			Headers:               entry.Headers,
			SupportPromptCacheKey: entry.SupportPromptCacheKey,
			DisableCooling:        entry.DisableCooling,
			RequestRetry:          entry.RequestRetry,
			RequestScopedErrors:   entry.RequestScopedErrors,
			AuthIndex:             views.openAIFallback[i],
		}
		if len(entry.APIKeyEntries) > 0 {
			response.APIKeyEntries = make([]openAICompatibilityAPIKeyWithAuthIndex, len(entry.APIKeyEntries))
			for j := range entry.APIKeyEntries {
				authIndex := ""
				if i < len(views.openAIEntries) && j < len(views.openAIEntries[i]) {
					authIndex = views.openAIEntries[i][j]
				}
				response.APIKeyEntries[j] = openAICompatibilityAPIKeyWithAuthIndex{
					OpenAICompatibilityAPIKey: entry.APIKeyEntries[j],
					AuthIndex:                 authIndex,
				}
			}
		}
		out[i] = response
	}
	return out
}

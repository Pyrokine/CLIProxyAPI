package management

import (
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
)

type configAuthIndexViews struct {
	gemini         []string
	claude         []string
	codex          []string
	vertex         []string
	openAIEntries  [][]string
	openAIFallback []string
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

type vertexCompatKeyWithAuthIndex struct {
	config.VertexCompatKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityAPIKeyWithAuthIndex struct {
	config.OpenAICompatibilityAPIKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityWithAuthIndex struct {
	Name          string                                   `json:"name"`
	Priority      int                                      `json:"priority,omitempty"`
	Disabled      bool                                     `json:"disabled,omitempty"`
	Prefix        string                                   `json:"prefix,omitempty"`
	BaseURL       string                                   `json:"base-url"`
	APIKeyEntries []openAICompatibilityAPIKeyWithAuthIndex `json:"api-key-entries,omitempty"`
	Models        []config.OpenAICompatibilityModel        `json:"models,omitempty"`
	Headers       map[string]string                        `json:"headers,omitempty"`
	AuthIndex     string                                   `json:"auth-index,omitempty"`
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
		claude:         make([]string, len(cfg.ClaudeKey)),
		codex:          make([]string, len(cfg.CodexKey)),
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
		gemini []string
		claude []string
		codex  []string
		vertex []string
		openAI []string
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
			continue
		case strings.HasPrefix(auth.ID, "claude:apikey:"):
			queues.claude = append(queues.claude, index)
		case strings.HasPrefix(auth.ID, "codex:apikey:"):
			queues.codex = append(queues.codex, index)
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
		(*cursor)++
		return index
	}

	geminiCursor := 0
	for i := range cfg.GeminiKey {
		if strings.TrimSpace(cfg.GeminiKey[i].APIKey) == "" {
			continue
		}
		views.gemini[i] = nextAuthIndex(queues.gemini, &geminiCursor)
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
			Name:      entry.Name,
			Priority:  entry.Priority,
			Disabled:  entry.Disabled,
			Prefix:    entry.Prefix,
			BaseURL:   entry.BaseURL,
			Models:    entry.Models,
			Headers:   entry.Headers,
			AuthIndex: views.openAIFallback[i],
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

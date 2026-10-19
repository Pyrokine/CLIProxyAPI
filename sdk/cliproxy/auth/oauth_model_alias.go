package auth

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	log "github.com/sirupsen/logrus"
)

const oauthModelAliasesAttributeKey = "model_aliases"

type modelAliasEntry interface {
	GetName() string
	GetAlias() string
}

type modelForceMappingEntry interface {
	GetForceMapping() bool
}

type oauthModelAliasEntry struct {
	upstreamModel string
	configAlias   string
	forceMapping  bool
}

type oauthModelAliasTable struct {
	// reverse maps channel -> alias (lower) -> upstream model entry.
	reverse map[string]map[string]oauthModelAliasEntry
}

// OAuthModelAliasResult carries the upstream model and optional response rewrite target.
type OAuthModelAliasResult struct {
	UpstreamModel string
	ForceMapping  bool
	OriginalAlias string
}

func compileOAuthModelAliasTable(aliases map[string][]config.OAuthModelAlias) *oauthModelAliasTable {
	if len(aliases) == 0 {
		return &oauthModelAliasTable{}
	}
	out := &oauthModelAliasTable{
		reverse: make(map[string]map[string]oauthModelAliasEntry, len(aliases)),
	}
	for rawChannel, entries := range aliases {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" || len(entries) == 0 {
			continue
		}
		rev := make(map[string]oauthModelAliasEntry, len(entries))
		for _, entry := range entries {
			name := strings.TrimSpace(entry.Name)
			alias := strings.TrimSpace(entry.Alias)
			if name == "" || alias == "" {
				continue
			}
			if strings.EqualFold(name, alias) {
				continue
			}
			aliasKey := strings.ToLower(alias)
			if _, exists := rev[aliasKey]; exists {
				continue
			}
			rev[aliasKey] = oauthModelAliasEntry{
				upstreamModel: name,
				configAlias:   alias,
				forceMapping:  entry.ForceMapping,
			}
		}
		if len(rev) > 0 {
			out.reverse[channel] = rev
		}
	}
	if len(out.reverse) == 0 {
		out.reverse = nil
	}
	return out
}

// SetOAuthModelAlias updates the OAuth model name alias table used during execution.
// The alias is applied per-auth channel to resolve the upstream model name while keeping the
// client-visible model name unchanged for translation/response formatting.
func (m *Manager) SetOAuthModelAlias(aliases map[string][]config.OAuthModelAlias) {
	if m == nil {
		return
	}
	table := compileOAuthModelAliasTable(aliases)
	// atomic.Value requires non-nil store values.
	if table == nil {
		table = &oauthModelAliasTable{}
	}
	m.oauthModelAlias.Store(table)
}

// SetOAuthModelAliasesAttribute stores per-credential OAuth model aliases used by model routing.
func SetOAuthModelAliasesAttribute(auth *Auth, aliases []config.OAuthModelAlias) error {
	if auth == nil {
		return fmt.Errorf("credential is nil")
	}
	clean := config.SanitizeOAuthModelAliasMap(map[string][]config.OAuthModelAlias{"credential": aliases})["credential"]
	if len(clean) != len(aliases) {
		return fmt.Errorf("invalid or duplicate credential model_aliases entry")
	}
	if len(clean) == 0 {
		delete(auth.Attributes, oauthModelAliasesAttributeKey)
		return nil
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		return fmt.Errorf("encode credential model aliases: %w", err)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[oauthModelAliasesAttributeKey] = string(encoded)
	return nil
}

// OAuthModelAliasesFromMetadata reads and validates aliases from a credential file.
func OAuthModelAliasesFromMetadata(metadata map[string]any) ([]config.OAuthModelAlias, error) {
	raw, found := metadata[oauthModelAliasesAttributeKey]
	if !found || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode credential model_aliases: %w", err)
	}
	var aliases []config.OAuthModelAlias
	if err = json.Unmarshal(encoded, &aliases); err != nil {
		return nil, fmt.Errorf("invalid credential model_aliases: %w", err)
	}
	clean := config.SanitizeOAuthModelAliasMap(map[string][]config.OAuthModelAlias{"credential": aliases})["credential"]
	if len(clean) != len(aliases) {
		return nil, fmt.Errorf("invalid or duplicate credential model_aliases entry")
	}
	return clean, nil
}

// OAuthModelAliasesFromAttributes reads the per-credential aliases for model publication.
func OAuthModelAliasesFromAttributes(attributes map[string]string) []config.OAuthModelAlias {
	raw := strings.TrimSpace(attributes[oauthModelAliasesAttributeKey])
	if raw == "" {
		return nil
	}
	var aliases []config.OAuthModelAlias
	if err := json.Unmarshal([]byte(raw), &aliases); err != nil {
		log.Warn("invalid credential model aliases attribute")
		return nil
	}
	return config.SanitizeOAuthModelAliasMap(map[string][]config.OAuthModelAlias{"credential": aliases})["credential"]
}

// applyOAuthModelAlias resolves the upstream model from OAuth model alias.
// If an alias exists, the returned model is the upstream model.
func (m *Manager) applyOAuthModelAlias(auth *Auth, requestedModel string) string {
	result := m.applyOAuthModelAliasWithResult(auth, requestedModel)
	return result.UpstreamModel
}

func modelAliasLookupCandidates(requestedModel string) (string, []string) {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return "", nil
	}
	requestResult := thinking.ParseSuffix(requestedModel)
	base := strings.TrimSpace(requestResult.ModelName)
	if base == "" {
		base = requestedModel
	}
	candidates := []string{requestedModel}
	if base != requestedModel {
		candidates = append(candidates, base)
	}
	return base, candidates
}

func resolveModelAliasResultFromConfigModels(
	requestedModel string,
	models []modelAliasEntry,
) OAuthModelAliasResult {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" || len(models) == 0 {
		return OAuthModelAliasResult{}
	}

	baseModel, candidates := modelAliasLookupCandidates(requestedModel)
	if len(candidates) == 0 {
		return OAuthModelAliasResult{}
	}

	for _, candidate := range candidates {
		for i := range models {
			name := strings.TrimSpace(models[i].GetName())
			alias := strings.TrimSpace(models[i].GetAlias())
			if name == "" || alias == "" || !strings.EqualFold(alias, candidate) {
				continue
			}
			forceMapping := false
			if forceEntry, ok := models[i].(modelForceMappingEntry); ok {
				forceMapping = forceEntry.GetForceMapping()
			}
			if strings.EqualFold(name, baseModel) && !forceMapping {
				return OAuthModelAliasResult{}
			}
			originalAlias := requestedModel
			if forceMapping {
				originalAlias = oauthModelAliasForceMappingResponseModel(alias)
			}
			return OAuthModelAliasResult{
				UpstreamModel: thinking.PreserveRequestModelDecorations(name, requestedModel),
				ForceMapping:  forceMapping,
				OriginalAlias: originalAlias,
			}
		}
	}
	return OAuthModelAliasResult{}
}

// resolveOAuthUpstreamModel resolves the upstream model name from OAuth model alias.
// If an alias exists, returns the original (upstream) model name that corresponds
// to the requested alias.
//
// If the requested model contains a thinking suffix (e.g. "gemini-2.5-pro(8192)"),
// the suffix is preserved in the returned model name. However, if the alias's
// original name already contains a suffix, the config suffix takes priority.
func (m *Manager) resolveOAuthUpstreamModel(auth *Auth, requestedModel string) string {
	return m.resolveOAuthModelAliasWithResult(auth, requestedModel).UpstreamModel
}

func (m *Manager) resolveOAuthModelAliasWithResult(auth *Auth, requestedModel string) OAuthModelAliasResult {
	channel := modelAliasChannel(auth)
	if channel == "" {
		return OAuthModelAliasResult{}
	}
	if result := resolveUpstreamModelFromAliases(OAuthModelAliasesFromAttributes(auth.Attributes), requestedModel); result.UpstreamModel != "" {
		return result
	}
	return resolveUpstreamModelFromAliasTable(m, auth, requestedModel, channel)
}

func resolveUpstreamModelFromAliases(aliases []config.OAuthModelAlias, requestedModel string) OAuthModelAliasResult {
	baseModel, candidates := modelAliasLookupCandidates(requestedModel)
	for _, candidate := range candidates {
		for _, entry := range aliases {
			if !strings.EqualFold(entry.Alias, candidate) {
				continue
			}
			if strings.EqualFold(entry.Name, baseModel) && !entry.ForceMapping {
				return OAuthModelAliasResult{}
			}
			originalAlias := requestedModel
			if entry.ForceMapping {
				originalAlias = oauthModelAliasForceMappingResponseModel(entry.Alias)
			}
			return OAuthModelAliasResult{
				UpstreamModel: thinking.PreserveRequestModelDecorations(entry.Name, requestedModel),
				ForceMapping:  entry.ForceMapping,
				OriginalAlias: originalAlias,
			}
		}
	}
	return OAuthModelAliasResult{}
}

func (m *Manager) applyOAuthModelAliasWithResult(auth *Auth, requestedModel string) OAuthModelAliasResult {
	result := m.resolveOAuthModelAliasWithResult(auth, requestedModel)
	if result.UpstreamModel == "" {
		result.UpstreamModel = requestedModel
	}
	return result
}

func oauthModelAliasForceMappingResponseModel(configAlias string) string {
	return strings.TrimSpace(configAlias)
}

func resolveUpstreamModelFromAliasTable(m *Manager, auth *Auth, requestedModel, channel string) OAuthModelAliasResult {
	if m == nil || auth == nil {
		return OAuthModelAliasResult{}
	}
	if channel == "" {
		return OAuthModelAliasResult{}
	}

	baseModel, candidates := modelAliasLookupCandidates(requestedModel)
	if len(candidates) == 0 {
		return OAuthModelAliasResult{}
	}

	raw := m.oauthModelAlias.Load()
	table, _ := raw.(*oauthModelAliasTable)
	if table == nil || table.reverse == nil {
		return OAuthModelAliasResult{}
	}
	rev := table.reverse[channel]
	if rev == nil {
		return OAuthModelAliasResult{}
	}

	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		entry, exists := rev[key]
		if !exists {
			continue
		}

		original := strings.TrimSpace(entry.upstreamModel)
		if original == "" {
			continue
		}
		if strings.EqualFold(original, baseModel) && !entry.forceMapping {
			return OAuthModelAliasResult{}
		}

		originalAlias := requestedModel
		if entry.forceMapping {
			originalAlias = oauthModelAliasForceMappingResponseModel(entry.configAlias)
		}
		return OAuthModelAliasResult{
			UpstreamModel: thinking.PreserveRequestModelDecorations(original, requestedModel),
			ForceMapping:  entry.forceMapping,
			OriginalAlias: originalAlias,
		}
	}

	return OAuthModelAliasResult{}
}

// modelAliasChannel extracts the OAuth model alias channel from an Auth object.
// It determines the provider and auth kind from the Auth's attributes and delegates
// to OAuthModelAliasChannel for the actual channel resolution.
func modelAliasChannel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	authKind := auth.AuthKind()
	if authKind == "" {
		if kind, _ := auth.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = AuthKindAPIKey
		}
	}
	return OAuthModelAliasChannel(provider, authKind)
}

// OAuthModelAliasChannel returns the OAuth model alias channel name for a given provider
// and auth kind. Returns empty string if the provider/authKind combination doesn't support
// OAuth model alias (e.g., API key authentication).
//
// Built-in channels: gemini-cli, vertex, aistudio, antigravity, claude, codex, qwen, iflow, kimi.
// Plugin OAuth providers use their normalized provider key as the channel.
func OAuthModelAliasChannel(provider, authKind string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	authKind = normalizeOAuthModelAliasAuthKind(authKind)
	if authKind == "apikey" {
		return ""
	}
	switch provider {
	case "gemini":
		// gemini provider uses gemini-api-key config, not oauth-model-alias.
		// OAuth-based gemini auth is converted to "gemini-cli" by the synthesizer.
		return ""
	case "vertex":
		return "vertex"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "gemini-cli", "aistudio", "antigravity", "qwen", "iflow", "kimi":
		return provider
	default:
		return provider
	}
}

func normalizeOAuthModelAliasAuthKind(authKind string) string {
	authKind = strings.ToLower(strings.TrimSpace(authKind))
	switch authKind {
	case "api_key", "api-key":
		return "apikey"
	default:
		return authKind
	}
}

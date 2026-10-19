package synthesizer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/geminicli"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

// FileSynthesizer generates Auth entries from OAuth JSON files.
// It handles file-based authentication and Gemini virtual auth generation.
type FileSynthesizer struct{}

// NewFileSynthesizer creates a new FileSynthesizer instance.
func NewFileSynthesizer() *FileSynthesizer {
	return &FileSynthesizer{}
}

// Synthesize generates Auth entries from auth files in the auth directory.
func (s *FileSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 16)
	if ctx == nil || ctx.AuthDir == "" {
		return out, nil
	}

	entries, err := os.ReadDir(ctx.AuthDir)
	if err != nil {
		// Not an error if directory doesn't exist
		return out, nil
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		full := filepath.Join(ctx.AuthDir, name)
		data, errRead := os.ReadFile(full)
		if errRead != nil || len(data) == 0 {
			continue
		}
		auths, errSynthesize := synthesizeFileAuths(ctx, full, data)
		if errSynthesize != nil {
			return out, fmt.Errorf("synthesize auth file %s: %w", name, errSynthesize)
		}
		if len(auths) > 0 {
			out = append(out, auths...)
		}
	}
	return out, nil
}

// SynthesizeAuthFile generates Auth entries for one auth JSON file payload.
func SynthesizeAuthFile(ctx *SynthesisContext, fullPath string, data []byte) ([]*coreauth.Auth, error) {
	return synthesizeFileAuths(ctx, fullPath, data)
}

func synthesizeFileAuths(ctx *SynthesisContext, fullPath string, data []byte) ([]*coreauth.Auth, error) {
	auths, err := synthesizeFileAuthsRaw(ctx, fullPath, data)
	if err != nil || len(auths) == 0 {
		return auths, err
	}
	digest, errDigest := coreauth.DigestPersistedJSON(data)
	if errDigest != nil {
		return nil, errDigest
	}
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if err := coreauth.BindPersistedAuthDigest(auth, digest); err != nil {
			log.WithError(err).Warn("auth file: quota material proof unavailable")
		}
		auth.Attributes[coreauth.PersistedReadAtAttribute] = ctx.Now.Format(time.RFC3339Nano)
	}
	return auths, nil
}

func synthesizeFileAuthsRaw(ctx *SynthesisContext, fullPath string, data []byte) ([]*coreauth.Auth, error) {
	if ctx == nil || len(data) == 0 {
		return nil, nil
	}

	now := ctx.Now
	cfg := ctx.Config
	metadata, errUnmarshal := coreauth.DecodeCredentialMetadata(data)
	if errUnmarshal != nil {
		return nil, fmt.Errorf("parse auth JSON: %w", errUnmarshal)
	}
	coreauth.NormalizeCredentialMetadata(metadata)
	if errWeight := coreauth.ValidateAuthWeight(&coreauth.Auth{Metadata: metadata}); errWeight != nil {
		return nil, errWeight
	}
	if _, _, errProfile := coreauth.ClaudeFingerprintProfile(&coreauth.Auth{Metadata: metadata}); errProfile != nil {
		return nil, errProfile
	}
	perAccountModelAliases, errAliases := coreauth.OAuthModelAliasesFromMetadata(metadata)
	if errAliases != nil {
		return nil, errAliases
	}
	t, _ := metadata["type"].(string)
	provider := strings.ToLower(strings.TrimSpace(t))
	if ctx.PluginAuthParser != nil {
		auths, handled, errParse := parsePluginFileAuths(
			ctx.PluginAuthParser,
			pluginapi.AuthParseRequest{
				Provider: provider,
				Path:     fullPath,
				FileName: filepath.Base(fullPath),
				RawJSON:  data,
			},
		)
		if errParse != nil {
			return nil, fmt.Errorf("parse plugin auth: %w", errParse)
		}
		if handled {
			auths = compactPluginAuths(auths)
			if len(auths) == 0 {
				return nil, nil
			}
			perAccountExcluded := extractExcludedModelsFromMetadata(metadata)
			disabled, _ := metadata["disabled"].(bool)
			seenIDs := make(map[string]struct{}, len(auths))
			for index, auth := range auths {
				coreauth.NormalizeCredentialMetadata(auth.Metadata)
				if strings.TrimSpace(auth.ID) == "" {
					return nil, fmt.Errorf("plugin auth parser returned an auth without an id")
				}
				if _, exists := seenIDs[auth.ID]; exists {
					return nil, fmt.Errorf("plugin auth parser returned duplicate auth id %q", auth.ID)
				}
				seenIDs[auth.ID] = struct{}{}
				if len(auths) > 1 {
					coreauth.MarkPluginVirtualAuth(auth, fullPath, index)
				}
				auth.CreatedAt = now
				auth.UpdatedAt = now
				if auth.Attributes == nil {
					auth.Attributes = make(map[string]string)
				}
				auth.Attributes[coreauth.AttributePath] = fullPath
				auth.Attributes[coreauth.AttributeSource] = fullPath
				auth.Attributes[coreauth.AttributeSourceBackend] = coreauth.AuthSourceFile
				if disabled {
					auth.Disabled = true
					auth.Status = coreauth.StatusDisabled
					if auth.Metadata == nil {
						auth.Metadata = make(map[string]any)
					}
					auth.Metadata["disabled"] = true
				}
				if proxyURL, ok := metadata["proxy_url"].(string); ok && strings.TrimSpace(auth.ProxyURL) == "" {
					auth.ProxyURL = strings.TrimSpace(proxyURL)
				}
				if prefix, ok := metadata["prefix"].(string); ok && strings.TrimSpace(auth.Prefix) == "" {
					auth.Prefix = strings.Trim(strings.TrimSpace(prefix), "/")
				}
				ApplyAuthExcludedModelsMeta(auth, cfg, perAccountExcluded, "oauth")
				if rules, specified := metadata["request_scoped_errors"]; specified {
					if auth.Metadata == nil {
						auth.Metadata = make(map[string]any)
					}
					auth.Metadata["request_scoped_errors"] = rules
				}
				coreauth.ApplyAuthPriorityMetadata(auth, metadata)
				if _, inherited := auth.Attributes[coreauth.AttributeFilePriority]; inherited {
					if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
						setter.SetMetadata(auth.Metadata)
					}
				}
				if errHeaders := coreauth.ApplyCustomHeadersFromMetadata(auth); errHeaders != nil {
					return nil, errHeaders
				}
				if errWeight := coreauth.ApplyAuthWeightMetadata(auth, metadata); errWeight != nil {
					return nil, errWeight
				}
				if errProfile := coreauth.ApplyClaudeFingerprintProfileMetadata(auth, metadata); errProfile != nil {
					return nil, errProfile
				}
				if _, specified := metadata["model_aliases"]; specified {
					if errAliases := coreauth.SetOAuthModelAliasesAttribute(auth, perAccountModelAliases); errAliases != nil {
						return nil, errAliases
					}
				}
			}
			return auths, nil
		}
	}
	if provider == "" {
		return nil, nil
	}
	if provider == "gemini" {
		provider = "gemini-cli"
	}
	label := provider
	if email, _ := metadata["email"].(string); email != "" {
		label = email
	}
	// 使用 authDir 下的相对路径，并在大小写不敏感的平台统一文件标识
	id := authfile.FileAuthID(fullPath, ctx.AuthDir)

	proxyURL := ""
	if p, ok := metadata["proxy_url"].(string); ok {
		proxyURL = p
	}

	prefix := ""
	if rawPrefix, ok := metadata["prefix"].(string); ok {
		trimmed := strings.TrimSpace(rawPrefix)
		trimmed = strings.Trim(trimmed, "/")
		if trimmed != "" && !strings.Contains(trimmed, "/") {
			prefix = trimmed
		}
	}

	disabled, _ := metadata["disabled"].(bool)
	status := coreauth.StatusActive
	if disabled {
		status = coreauth.StatusDisabled
	}

	// Read per-account excluded models from the OAuth JSON file
	perAccountExcluded := extractExcludedModelsFromMetadata(metadata)

	a := &coreauth.Auth{
		ID:       id,
		Provider: provider,
		FileName: id,
		Label:    label,
		Prefix:   prefix,
		Status:   status,
		Disabled: disabled,
		Attributes: map[string]string{
			"source": fullPath,
			"path":   fullPath,
		},
		ProxyURL:  proxyURL,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if provider == "kimi" || provider == "kimi-ai" || provider == "kimi.ai" || provider == "kimi.com" {
		if baseURL, ok := metadata["base_url"].(string); ok {
			a.Attributes["base_url"] = strings.TrimSpace(baseURL)
		}
		if domain, ok := metadata["domain"].(string); ok {
			a.Attributes["domain"] = strings.TrimSpace(domain)
		}
		domain := kimiauth.ResolveKimiDomainFromAuth(a)
		a.Attributes["domain"] = domain
		if a.Attributes["base_url"] == "" {
			a.Attributes["base_url"] = kimiauth.ResolveKimiAPIBaseURL(domain)
		}
		if domain == kimiauth.KimiAIDomain {
			a.Provider = "kimi-ai"
		} else {
			a.Provider = "kimi"
		}
	}
	if err := coreauth.ApplyCustomHeadersFromMetadata(a); err != nil {
		return nil, err
	}
	if errWeight := coreauth.ApplyAuthWeightMetadata(a, metadata); errWeight != nil {
		return nil, errWeight
	}
	if errProfile := coreauth.ApplyClaudeFingerprintProfileMetadata(a, metadata); errProfile != nil {
		return nil, errProfile
	}
	coreauth.ApplyAuthPriorityMetadata(a, metadata)
	ApplyAuthExcludedModelsMeta(a, cfg, perAccountExcluded, "oauth")
	if errAliases := coreauth.SetOAuthModelAliasesAttribute(a, perAccountModelAliases); errAliases != nil {
		return nil, errAliases
	}
	if provider == "codex" {
		planType := codex.DefaultPlanType
		if savedPlan, ok := metadata["plan_type"].(string); ok && strings.TrimSpace(savedPlan) != "" {
			planType = strings.TrimSpace(savedPlan)
		} else if idToken, ok := metadata["id_token"].(string); ok && strings.TrimSpace(idToken) != "" {
			claims, errParse := codex.ParseJWTToken(idToken)
			if errParse == nil {
				planType = claims.GetPlanType()
			}
		}
		a.Attributes["plan_type"] = planType
	}
	if provider == "gemini-cli" {
		if virtualAuths := synthesizeGeminiVirtualAuths(a, metadata, now); len(virtualAuths) > 0 {
			for _, v := range virtualAuths {
				ApplyAuthExcludedModelsMeta(v, cfg, perAccountExcluded, "oauth")
			}
			out := make([]*coreauth.Auth, 0, 1+len(virtualAuths))
			out = append(out, a)
			out = append(out, virtualAuths...)
			return out, nil
		}
	}
	return []*coreauth.Auth{a}, nil
}

func parsePluginFileAuths(
	parser PluginAuthParser,
	req pluginapi.AuthParseRequest,
) ([]*coreauth.Auth, bool, error) {
	if parser == nil {
		return nil, false, nil
	}
	if multiParser, ok := parser.(PluginMultiAuthParser); ok {
		return multiParser.ParseAuths(context.Background(), req)
	}
	auth, handled, errParse := parser.ParseAuth(context.Background(), req)
	if errParse != nil || !handled || auth == nil {
		return nil, handled, errParse
	}
	return []*coreauth.Auth{auth}, true, nil
}

func compactPluginAuths(auths []*coreauth.Auth) []*coreauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := auths[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if errWeight := coreauth.ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// synthesizeGeminiVirtualAuths creates virtual Auth entries for multi-project Gemini credentials.
// It disables the primary auth and creates one virtual auth per project.
func synthesizeGeminiVirtualAuths(primary *coreauth.Auth, metadata map[string]any, _ time.Time) []*coreauth.Auth {
	if primary == nil || metadata == nil {
		return nil
	}
	projects := splitGeminiProjectIDs(metadata)
	if len(projects) <= 1 {
		return nil
	}
	email, _ := metadata["email"].(string)
	shared := geminicli.NewSharedCredential(primary.ID, email, metadata, projects)
	primary.Disabled = true
	primary.Status = coreauth.StatusDisabled
	primary.Runtime = shared
	if primary.Attributes == nil {
		primary.Attributes = make(map[string]string)
	}
	primary.Attributes["gemini_virtual_primary"] = "true"
	primary.Attributes["virtual_children"] = strings.Join(projects, ",")
	source := primary.Attributes["source"]
	authPath := primary.Attributes["path"]
	originalProvider := primary.Provider
	if originalProvider == "" {
		originalProvider = "gemini-cli"
	}
	label := primary.Label
	if label == "" {
		label = originalProvider
	}
	virtualAuths := make([]*coreauth.Auth, 0, len(projects))
	for _, projectID := range projects {
		attrs := map[string]string{
			"runtime_only":           "true",
			"gemini_virtual_parent":  primary.ID,
			"gemini_virtual_project": projectID,
		}
		if source != "" {
			attrs["source"] = source
		}
		if authPath != "" {
			attrs["path"] = authPath
		}
		// Propagate routing attributes from primary auth to virtual auths.
		if priorityVal, hasPriority := primary.Attributes["priority"]; hasPriority && priorityVal != "" {
			attrs["priority"] = priorityVal
		}
		if weight, hasWeight := primary.Attributes[coreauth.AttributeWeight]; hasWeight {
			attrs[coreauth.AttributeWeight] = weight
		}
		if aliases, hasAliases := primary.Attributes["model_aliases"]; hasAliases {
			attrs["model_aliases"] = aliases
		}
		metadataCopy := map[string]any{
			"email":             email,
			"project_id":        projectID,
			"virtual":           true,
			"virtual_parent_id": primary.ID,
			"type":              metadata["type"],
		}
		if v, ok := metadata["disable_cooling"]; ok {
			metadataCopy["disable_cooling"] = v
		} else if v, ok := metadata["disable-cooling"]; ok {
			metadataCopy["disable_cooling"] = v
		}
		if v, ok := metadata["request_retry"]; ok {
			metadataCopy["request_retry"] = v
		} else if v, ok := metadata["request-retry"]; ok {
			metadataCopy["request_retry"] = v
		}
		if rules, ok := metadata["request_scoped_errors"]; ok {
			metadataCopy["request_scoped_errors"] = rules
		}
		if note, ok := metadata["note"].(string); ok {
			if note = strings.TrimSpace(note); note != "" {
				metadataCopy["note"] = note
			}
		}
		proxy := strings.TrimSpace(primary.ProxyURL)
		if proxy != "" {
			metadataCopy["proxy_url"] = proxy
		}
		virtual := &coreauth.Auth{
			ID:         buildGeminiVirtualID(primary.ID, projectID),
			Provider:   originalProvider,
			Label:      fmt.Sprintf("%s [%s]", label, projectID),
			Status:     coreauth.StatusActive,
			Attributes: attrs,
			Metadata:   metadataCopy,
			ProxyURL:   primary.ProxyURL,
			Prefix:     primary.Prefix,
			CreatedAt:  primary.CreatedAt,
			UpdatedAt:  primary.UpdatedAt,
			Runtime:    geminicli.NewVirtualCredential(projectID, shared),
		}
		virtualAuths = append(virtualAuths, virtual)
	}
	return virtualAuths
}

// splitGeminiProjectIDs extracts and deduplicates project IDs from metadata.
func splitGeminiProjectIDs(metadata map[string]any) []string {
	raw, _ := metadata["project_id"].(string)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

// buildGeminiVirtualID constructs a virtual auth ID from base ID and project ID.
func buildGeminiVirtualID(baseID, projectID string) string {
	project := strings.TrimSpace(projectID)
	if project == "" {
		project = "project"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_")
	return fmt.Sprintf("%s::%s", baseID, replacer.Replace(project))
}

// extractExcludedModelsFromMetadata reads per-account excluded models from the OAuth JSON metadata.
// Supports both "excluded_models" and "excluded-models" keys, and accepts both []string and []interface{}.
func extractExcludedModelsFromMetadata(metadata map[string]any) []string {
	if metadata == nil {
		return nil
	}
	// Try both key formats
	raw, ok := metadata["excluded_models"]
	if !ok {
		raw, ok = metadata["excluded-models"]
	}
	if !ok || raw == nil {
		return nil
	}
	var stringSlice []string
	switch v := raw.(type) {
	case []string:
		stringSlice = v
	case []any:
		stringSlice = make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				stringSlice = append(stringSlice, s)
			}
		}
	default:
		return nil
	}
	result := make([]string, 0, len(stringSlice))
	for _, s := range stringSlice {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

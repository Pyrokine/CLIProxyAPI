package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"

	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

	geminiQuotaURL      = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	geminiCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:getCodeAssistSubscription"

	kimiUsageURL  = "https://api.kimi.com/coding/v1/usages"
	xaiBillingURL = "https://cli-chat-proxy.grok.com/v1/billing"
)

var claudeHeaders = map[string]string{
	"Authorization":  "Bearer $TOKEN$",
	"Content-Type":   "application/json",
	"anthropic-beta": "oauth-2025-04-20",
}

var codexHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
	"User-Agent":    "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
}

var geminiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
}

var kimiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
	"Content-Type":  "application/json",
}

var xaiHeaders = map[string]string{
	"Authorization": "Bearer $TOKEN$",
}

// fetchClaudeQuota queries Claude's quota API via the local api-call proxy.
func fetchClaudeQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	usageResp, err := apiCall(
		quota.APICallRequest{
			URL:       claudeUsageURL,
			Method:    "GET",
			Header:    claudeHeaders,
			AuthIndex: authIndex,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("claude usage request failed: %w", err)
	}
	if usageResp.StatusCode < 200 || usageResp.StatusCode >= 300 {
		return nil, fmt.Errorf("claude usage returned status %d", usageResp.StatusCode)
	}

	profileResp, profileErr := apiCall(
		quota.APICallRequest{
			URL:       claudeProfileURL,
			Method:    "GET",
			Header:    claudeHeaders,
			AuthIndex: authIndex,
		},
	)

	result := map[string]interface{}{
		"usage": usageResp.Body,
	}
	if profileErr == nil && profileResp.StatusCode >= 200 && profileResp.StatusCode < 300 {
		result["profile"] = profileResp.Body
	}

	return json.Marshal(result)
}

// fetchCodexQuota queries Codex/Copilot quota via api-call proxy.
func fetchCodexQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	resp, err := apiCall(
		quota.APICallRequest{
			URL:       codexUsageURL,
			Method:    "GET",
			Header:    codexHeaders,
			AuthIndex: authIndex,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("codex usage request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex usage returned status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// fetchGeminiQuota queries Gemini CLI quota via api-call proxy.
func fetchGeminiQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	quotaResp, err := apiCall(
		quota.APICallRequest{
			URL:       geminiQuotaURL,
			Method:    "POST",
			Header:    geminiHeaders,
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)
	if err != nil {
		return nil, fmt.Errorf("gemini quota request failed: %w", err)
	}
	if quotaResp.StatusCode < 200 || quotaResp.StatusCode >= 300 {
		return nil, fmt.Errorf("gemini quota returned status %d", quotaResp.StatusCode)
	}

	// Also fetch code assist subscription for tier info
	codeAssistResp, caErr := apiCall(
		quota.APICallRequest{
			URL:       geminiCodeAssistURL,
			Method:    "POST",
			Header:    geminiHeaders,
			AuthIndex: authIndex,
			Data:      "{}",
		},
	)

	result := map[string]interface{}{
		"quota": quotaResp.Body,
	}
	if caErr == nil && codeAssistResp.StatusCode >= 200 && codeAssistResp.StatusCode < 300 {
		result["codeAssist"] = codeAssistResp.Body
	}
	return json.Marshal(result)
}

// fetchAntigravityQuota queries Antigravity quota via api-call proxy.
// Antigravity shares the Gemini CLI cloudcode-pa endpoints, so this delegates to fetchGeminiQuota.
func fetchAntigravityQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	return fetchGeminiQuota(apiCall, authIndex)
}

// fetchKimiQuota queries Kimi quota via api-call proxy.
func fetchKimiQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	resp, err := apiCall(
		quota.APICallRequest{
			URL:       kimiUsageURL,
			Method:    "GET",
			Header:    kimiHeaders,
			AuthIndex: authIndex,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("kimi usage request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kimi usage returned status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func fetchXAIQuota(apiCall quota.InternalAPICallFunc, authIndex string) ([]byte, error) {
	resp, err := apiCall(
		quota.APICallRequest{
			URL:       xaiBillingURL,
			Method:    "GET",
			Header:    xaiHeaders,
			AuthIndex: authIndex,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("xai billing request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xai billing returned status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func registerAuthManagerForQuota(scheduler *quota.Scheduler, authManager *coreauth.Manager) int {
	if scheduler == nil || authManager == nil {
		return 0
	}
	registered := 0
	for _, auth := range authManager.List() {
		if auth == nil {
			continue
		}
		fileName := strings.TrimSpace(auth.FileName)
		if fileName == "" {
			continue
		}
		credType, ok := quotaCredentialTypeForProvider(auth.Provider)
		if !ok {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == "" {
			continue
		}
		scheduler.Register(fileName, credType, auth.Index)
		scheduler.SetDisabled(fileName, auth.Disabled)
		registered++
	}
	return registered
}

func registerAuthFilesForQuota(scheduler *quota.Scheduler, authDir string) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		log.Warnf("quota: cannot read auth dir %s: %v", authDir, err)
		return
	}

	registered := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		name := entry.Name()
		nameLower := strings.ToLower(name)

		credType, ok := quotaCredentialTypeForFileName(nameLower)
		if !ok {
			continue
		}

		// Compute auth_index the same way as sdk/cliproxy/auth: sha256("file:" + name)[:8]
		authIndex := computeAuthIndex(name)

		scheduler.Register(name, credType, authIndex)
		registered++
	}

	if registered > 0 {
		log.Infof("quota: registered %d auth files from %s", registered, authDir)
	}
}

func quotaCredentialTypeForProvider(provider string) (quota.CredentialType, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return quota.TypeClaude, true
	case "codex":
		return quota.TypeCodex, true
	case "gemini", "gemini-cli":
		return quota.TypeGeminiCli, true
	case "antigravity":
		return quota.TypeAntigravity, true
	case "kimi":
		return quota.TypeKimi, true
	case "xai", "x-ai", "grok":
		return quota.TypeXAI, true
	default:
		return "", false
	}
}

func quotaCredentialTypeForFileName(nameLower string) (quota.CredentialType, bool) {
	switch {
	case strings.HasPrefix(nameLower, "claude"):
		return quota.TypeClaude, true
	case strings.HasPrefix(nameLower, "codex"):
		return quota.TypeCodex, true
	case strings.HasPrefix(nameLower, "antigravity"):
		return quota.TypeAntigravity, true
	case strings.HasPrefix(nameLower, "gemini"):
		return quota.TypeGeminiCli, true
	case strings.HasPrefix(nameLower, "kimi"):
		return quota.TypeKimi, true
	case strings.HasPrefix(nameLower, "xai"), strings.HasPrefix(nameLower, "x-ai"), strings.HasPrefix(
		nameLower, "grok",
	):
		return quota.TypeXAI, true
	default:
		return "", false
	}
}

func computeAuthIndex(fileName string) string {
	seed := "file:" + strings.TrimSpace(fileName)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

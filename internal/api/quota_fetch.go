package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	log "github.com/sirupsen/logrus"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"

	codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

	geminiQuotaURL      = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	geminiCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:getCodeAssistSubscription"

	kimiUsageURL = "https://api.kimi.com/coding/v1/usages"
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
		"usage": json.RawMessage(usageResp.Body),
	}
	if profileErr == nil && profileResp.StatusCode >= 200 && profileResp.StatusCode < 300 {
		result["profile"] = json.RawMessage(profileResp.Body)
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
		"quota": json.RawMessage(quotaResp.Body),
	}
	if caErr == nil && codeAssistResp.StatusCode >= 200 && codeAssistResp.StatusCode < 300 {
		result["codeAssist"] = json.RawMessage(codeAssistResp.Body)
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

// registerAuthFilesForQuota registers auth files found in authDir with the quota
// scheduler. auth_index is derived from the filename hash matching the backend's
// auth indexing scheme.
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

		var credType quota.CredentialType
		switch {
		case strings.HasPrefix(nameLower, "claude"):
			credType = quota.TypeClaude
		case strings.HasPrefix(nameLower, "codex"):
			credType = quota.TypeCodex
		case strings.HasPrefix(nameLower, "antigravity"):
			credType = quota.TypeAntigravity
		case strings.HasPrefix(nameLower, "gemini"):
			credType = quota.TypeGeminiCli
		case strings.HasPrefix(nameLower, "kimi"):
			credType = quota.TypeKimi
		default:
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

// computeAuthIndex replicates the auth index computation from sdk/cliproxy/auth.
// It computes sha256("file:" + fileName) and takes the first 8 bytes as hex.
// computeAuthIndex replicates the auth index computation from sdk/cliproxy/auth.stableAuthIndex.
// Both use sha256(seed)[:8] as hex. If the SDK algorithm changes, this must be updated in sync.
func computeAuthIndex(fileName string) string {
	seed := "file:" + strings.TrimSpace(fileName)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

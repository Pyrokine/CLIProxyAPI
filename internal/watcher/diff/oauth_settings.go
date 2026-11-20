package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

// OAuthSettingsSummary 保存渠道设置的摘要和条目数
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
type OAuthSettingsSummary = modelsSummary

// SummarizeOAuthSettings 按渠道汇总 OAuth 模型设置，保留规则顺序
func SummarizeOAuthSettings(entries map[string][]config.OAuthModelSetting) map[string]OAuthSettingsSummary {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]OAuthSettingsSummary, len(entries))
	for rawChannel, settings := range entries {
		channel := strings.ToLower(strings.TrimSpace(rawChannel))
		if channel == "" {
			continue
		}
		seen := make(map[string]struct{}, len(settings))
		normalized := make([]string, 0, len(settings))
		for _, setting := range settings {
			name := strings.ToLower(strings.TrimSpace(setting.Name))
			if name == "" {
				continue
			}
			key := name + "->" + strings.ToLower(strings.TrimSpace(setting.Alias))
			if setting.MaxContextLength > 0 {
				key += fmt.Sprintf("|max-context-length=%d", setting.MaxContextLength)
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			normalized = append(normalized, key)
		}
		var summary OAuthSettingsSummary
		if len(normalized) > 0 {
			sum := sha256.Sum256([]byte(strings.Join(normalized, "|")))
			summary = OAuthSettingsSummary{hash: hex.EncodeToString(sum[:]), count: len(normalized)}
		}
		out[channel] = summary
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// OAuthSettingsChanges 返回逐渠道变化及按名称排序的受影响渠道
func OAuthSettingsChanges(oldMap, newMap map[string][]config.OAuthModelSetting) ([]string, []string) {
	return diffSummaryMapChanges(SummarizeOAuthSettings(oldMap), SummarizeOAuthSettings(newMap), "oauth-settings")
}

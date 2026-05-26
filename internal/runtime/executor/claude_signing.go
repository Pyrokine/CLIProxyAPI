package executor

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/pierrec/xxHash/xxHash64"
)

const claudeCCHSeed uint64 = 0x6E52736AC806831E

var claudeBillingHeaderPlaceholderPattern = regexp.MustCompile(`(x-anthropic-billing-header:[^"]*?\bcch=)(00000)(;)`)

func signAnthropicMessagesBody(body []byte) []byte {
	if !claudeBillingHeaderPlaceholderPattern.Match(body) {
		return body
	}

	cch := fmt.Sprintf("%05x", xxHash64.Checksum(body, claudeCCHSeed)&0xFFFFF)
	return claudeBillingHeaderPlaceholderPattern.ReplaceAll(body, []byte("${1}"+cch+"${3}"))
}

func resolveClaudeKeyConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.ClaudeKey {
	if cfg == nil || auth == nil {
		return nil
	}

	apiKey, baseURL := claudeCreds(auth)
	if apiKey == "" {
		return nil
	}

	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if !strings.EqualFold(cfgKey, apiKey) {
			continue
		}
		if baseURL != "" && cfgBase != "" && !strings.EqualFold(cfgBase, baseURL) {
			continue
		}
		return entry
	}

	return nil
}

func experimentalCCHSigningEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	entry := resolveClaudeKeyConfig(cfg, auth)
	return entry != nil && entry.ExperimentalCCHSigning
}

package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ClaudeFingerprintProfileDefault       = ""
	ClaudeFingerprintProfileClaudeCodeCLI = "claude-code-cli"
)

// NormalizeClaudeFingerprintProfile 规范化请求身份配置，并标明非法值
func NormalizeClaudeFingerprintProfile(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ClaudeFingerprintProfileDefault, true
	case "claude-code-cli", "oauth-cli":
		return ClaudeFingerprintProfileClaudeCodeCLI, true
	default:
		return raw, false
	}
}

func ValidateClaudeFingerprintProfile(raw string) error {
	if _, ok := NormalizeClaudeFingerprintProfile(raw); !ok {
		return fmt.Errorf("fingerprint-profile must be empty or %q", ClaudeFingerprintProfileClaudeCodeCLI)
	}
	return nil
}

// ParseClaudeFingerprintProfile 拒绝非字符串，避免 null 被解释为显式清空
func ParseClaudeFingerprintProfile(value any) (string, error) {
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("fingerprint-profile must be a string")
	}
	if err := ValidateClaudeFingerprintProfile(raw); err != nil {
		return "", err
	}
	profile, _ := NormalizeClaudeFingerprintProfile(raw)
	return profile, nil
}

// UnmarshalJSON 在完整配置和单项创建入口使用相同的字段类型校验
func (k *ClaudeKey) UnmarshalJSON(data []byte) error {
	type plain ClaudeKey
	candidate := plain(*k)
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name, raw := range fields {
		if strings.EqualFold(name, "fingerprint-profile") {
			if _, err := ParseClaudeFingerprintProfile(raw); err != nil {
				return err
			}
		}
	}
	if err := json.Unmarshal(data, &candidate); err != nil {
		return err
	}
	profile, err := ParseClaudeFingerprintProfile(candidate.FingerprintProfile)
	if err != nil {
		return err
	}
	candidate.FingerprintProfile = profile
	*k = ClaudeKey(candidate)
	return nil
}

// UnmarshalYAML 对合并键和 alias 解码后的值校验，不接受隐式字符串转换
func (k *ClaudeKey) UnmarshalYAML(node *yaml.Node) error {
	type plain ClaudeKey
	candidate := plain(*k)
	var fields map[string]any
	if err := node.Decode(&fields); err != nil {
		return err
	}
	if raw, exists := fields["fingerprint-profile"]; exists {
		if _, err := ParseClaudeFingerprintProfile(raw); err != nil {
			return err
		}
	}
	if err := node.Decode(&candidate); err != nil {
		return err
	}
	profile, err := ParseClaudeFingerprintProfile(candidate.FingerprintProfile)
	if err != nil {
		return err
	}
	candidate.FingerprintProfile = profile
	*k = ClaudeKey(candidate)
	return nil
}

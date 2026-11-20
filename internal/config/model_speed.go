package config

import (
	"slices"
	"strings"
)

// ModelSpeedOptions 按精确型号和渠道列出可配置的速度档位，账号权限由上游判断
func ModelSpeedOptions(model, channel string) []string {
	model = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(model, "models/")))
	channel = strings.ToLower(strings.TrimSpace(channel))
	switch channel {
	case "codex", "openai-compatibility":
		switch model {
		case "gpt-6.2-astra", "gpt-6.2-sol", "gpt-6-astra", "gpt-6.1-sol":
			return []string{"inherit", "standard", "fast", "ultrafast"}
		case "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
			"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.2", "gpt-5.1", "gpt-5", "gpt-5-mini",
			"gpt-4.1", "gpt-4.1-mini", "gpt-4o", "gpt-4o-mini", "o3":
			return []string{"inherit", "standard", "fast"}
		}
	case "claude":
		switch model {
		case "claude-fable-5-2", "claude-mythos-5-2", "claude-opus-5-5", "claude-opus-5", "claude-opus-4-8":
			return []string{"inherit", "standard", "fast"}
		}
	}
	return nil
}

// ModelSpeedAllowed 校验目录中显式设置的速度档位
func ModelSpeedAllowed(model, channel, speed string) bool {
	return speed == "" || speed == "inherit" || slices.Contains(ModelSpeedOptions(model, channel), speed)
}

// ModelCatalogSpeed 在所示渠道内优先采用客户端模型的设置，再查询转发模型
func (cfg *SDKConfig) ModelCatalogSpeed(channel string, models ...string) string {
	for _, model := range models {
		if entry, ok := cfg.modelCatalogEntry(model); ok {
			if !strings.EqualFold(strings.TrimSpace(entry.Channel), strings.TrimSpace(channel)) {
				return ""
			}
			return entry.Speed
		}
	}
	return ""
}

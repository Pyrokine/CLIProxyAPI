package modelcatalog

import (
	"regexp"
	"strings"
)

var (
	versionPrefixRE   = regexp.MustCompile(`^(?:gpt-|claude-|gemini-|grok-|qwen)(\d+(?:\.\d+)?)`)
	deepSeekVersionRE = regexp.MustCompile(`^deepseek-v(\d+(?:\.\d+)?)`)
	glmVersionRE      = regexp.MustCompile(`^(?:chat)?glm-(\d+)(?:\.\d+)?`)
	kimiVersionRE     = regexp.MustCompile(`^kimi-k(\d+(?:\.\d+)?)`)
	miniMaxVersionRE  = regexp.MustCompile(`^minimax[-_]?m(\d+(?:\.\d+)?)`)
)

func inferProvider(channel, name string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	name = strings.ToLower(normalizeModelName(name))
	switch channel {
	case "claude", "anthropic":
		return "Claude"
	case "codex", "openai", "openai-compatibility":
		return "GPT"
	case "gemini", "vertex", "gemini-cli", "aistudio", "antigravity", "google":
		return "Gemini"
	case "qwen":
		return "Qwen"
	case "kimi":
		return "Kimi"
	case "xai", "x-ai", "grok":
		return "Grok"
	}
	if strings.Contains(name, "claude") {
		return "Claude"
	}
	if strings.Contains(name, "gpt") || strings.HasPrefix(name, "codex-") || strings.HasPrefix(
		name, "o1",
	) || strings.HasPrefix(name, "o3") || strings.HasPrefix(name, "o4") {
		return "GPT"
	}
	if strings.Contains(name, "gemini") || strings.HasPrefix(name, "imagen-") {
		return "Gemini"
	}
	if strings.Contains(name, "qwen") || name == "coder-model" || name == "vision-model" {
		return "Qwen"
	}
	if strings.Contains(name, "glm") || strings.Contains(name, "chatglm") {
		return "GLM"
	}
	if strings.Contains(name, "grok") {
		return "Grok"
	}
	if strings.Contains(name, "deepseek") {
		return "DeepSeek"
	}
	if strings.Contains(name, "kimi") {
		return "Kimi"
	}
	if strings.Contains(name, "minimax") || strings.Contains(name, "abab") {
		return "MiniMax"
	}
	return "Other"
}

func inferChannel(channel, name string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != "" {
		return channel
	}
	switch inferProvider(channel, name) {
	case "Claude":
		return "claude"
	case "GPT":
		return "codex"
	case "Gemini":
		return "gemini"
	case "Qwen":
		return "qwen"
	case "Kimi":
		return "kimi"
	case "Grok":
		return "xai"
	case "DeepSeek":
		return "deepseek"
	case "GLM":
		return "glm"
	case "MiniMax":
		return "minimax"
	default:
		return "other"
	}
}

func inferGroup(provider, name string) string {
	provider = strings.TrimSpace(provider)
	lower := strings.ToLower(normalizeModelName(name))
	if provider == "" {
		provider = inferProvider("", lower)
	}
	switch provider {
	case "Claude":
		if strings.Contains(lower, "opus") {
			return "Opus"
		}
		if strings.Contains(lower, "sonnet") {
			return "Sonnet"
		}
		if strings.Contains(lower, "haiku") {
			return "Haiku"
		}
		return "Claude"
	case "GPT":
		if strings.HasPrefix(lower, "gpt-image-") {
			return "GPT Image"
		}
		if strings.HasPrefix(lower, "gpt-oss-") {
			return "GPT OSS"
		}
		if strings.HasPrefix(lower, "codex-") {
			return "Codex Tools"
		}
		if match := versionPrefixRE.FindStringSubmatch(lower); len(match) == 2 {
			return "GPT " + strings.Split(match[1], ".")[0]
		}
		if strings.HasPrefix(lower, "o") {
			return "OpenAI o-series"
		}
		return "GPT Other"
	case "Gemini":
		if strings.HasPrefix(lower, "imagen-") {
			return "Imagen"
		}
		if match := versionPrefixRE.FindStringSubmatch(lower); len(match) == 2 {
			return "Gemini " + strings.Split(match[1], ".")[0]
		}
		return "Gemini Other"
	case "Qwen":
		if strings.Contains(lower, "coder") || lower == "coder-model" {
			return "Coder"
		}
		if strings.Contains(lower, "vl") || lower == "vision-model" {
			return "Vision"
		}
		return "Qwen"
	case "DeepSeek":
		if lower == "deepseek-flash" {
			return "V4.1"
		}
		if strings.HasPrefix(lower, "deepseek-r1") {
			return "R1"
		}
		if match := deepSeekVersionRE.FindStringSubmatch(lower); len(match) == 2 {
			group := "V" + match[1]
			if strings.Contains(lower, "vision") {
				return group + " Vision"
			}
			return group
		}
		return "DeepSeek Other"
	case "Grok":
		if match := versionPrefixRE.FindStringSubmatch(lower); len(match) == 2 {
			return "Grok " + match[1]
		}
		return "Grok Other"
	case "Kimi":
		if match := kimiVersionRE.FindStringSubmatch(lower); len(match) == 2 {
			return "K" + match[1]
		}
		return "Kimi Other"
	case "GLM":
		if match := glmVersionRE.FindStringSubmatch(lower); len(match) == 2 {
			return "GLM " + match[1]
		}
		return "GLM Other"
	case "MiniMax":
		if match := miniMaxVersionRE.FindStringSubmatch(lower); len(match) == 2 {
			return "M" + match[1]
		}
		return "MiniMax Other"
	default:
		return provider
	}
}

func displayNameFromName(name string) string {
	name = normalizeModelName(name)
	if name == "" {
		return ""
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		switch lower {
		case "gpt", "glm", "vl", "oss", "r1", "v3", "v4", "api", "cli", "ai":
			parts[i] = strings.ToUpper(part)
		default:
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, " ")
}

func providerOrder(provider string) int {
	switch strings.TrimSpace(provider) {
	case "GPT":
		return 0
	case "Claude":
		return 1
	case "Gemini":
		return 2
	case "Kimi":
		return 3
	case "Qwen":
		return 4
	case "GLM":
		return 5
	case "Grok":
		return 6
	case "DeepSeek":
		return 7
	case "MiniMax":
		return 8
	default:
		return 100
	}
}

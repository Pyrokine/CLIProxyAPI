package executor

import (
	"slices"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

func applyModelCatalogSpeed(
	cfg *config.Config,
	model, protocol, root string,
	body []byte,
	requestedModel, targetExecutor, requestPath string,
) []byte {
	if strings.HasSuffix(requestPath, "/count_tokens") || strings.HasSuffix(requestPath, ":countTokens") ||
		strings.HasSuffix(requestPath, "/responses/compact") {
		return body
	}
	channel := strings.ToLower(strings.TrimSpace(targetExecutor))
	switch channel {
	case "codex", "codex-websockets", "codex_websockets":
		channel = "codex"
	case "claude":
	default:
		switch protocol {
		case "openai", "openai-response":
			channel = "openai-compatibility"
		}
	}
	speed := cfg.ModelCatalogSpeed(channel, requestedModel, model)
	if speed == "" || speed == "inherit" {
		return body
	}
	baseModel := thinking.ParseSuffix(model).ModelName
	if !slices.Contains(config.ModelSpeedOptions(baseModel, channel), speed) ||
		(speed == "ultrafast" && protocol != "codex" && protocol != "openai-response") {
		log.WithFields(log.Fields{"model": baseModel, "channel": channel, "speed": speed}).
			Warn("配置的速度档位不适用于当前模型或渠道，保留原请求")
		return body
	}
	field, value := "service_tier", "priority"
	if channel == "claude" {
		field, value = "speed", speed
	} else if speed == "ultrafast" {
		value = "ultrafast"
	} else if speed == "standard" {
		value = "default"
	}
	updated, _ := sjson.SetBytes(body, buildPayloadPath(root, field), value)
	return updated
}

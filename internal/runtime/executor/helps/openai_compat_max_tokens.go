package helps

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ShouldUseMaxCompletionTokensForModel resolves the upstream model before the client alias.
func ShouldUseMaxCompletionTokensForModel(
	compat *config.OpenAICompatibility,
	upstreamModel, requestedModel string,
) bool {
	if compat == nil {
		return false
	}
	if enabled, found := openAICompatibilityModelUsesMaxCompletionTokens(compat.Models, upstreamModel); found {
		return enabled
	}
	enabled, _ := openAICompatibilityModelUsesMaxCompletionTokens(compat.Models, requestedModel)
	return enabled
}

func openAICompatibilityModelUsesMaxCompletionTokens(
	models []config.OpenAICompatibilityModel,
	model string,
) (bool, bool) {
	model = normalizeOpenAICompatibilityModelName(model)
	if model == "" {
		return false, false
	}
	for i := range models {
		if strings.EqualFold(model, normalizeOpenAICompatibilityModelName(models[i].Name)) {
			return models[i].UseMaxCompletionTokens, true
		}
	}
	for i := range models {
		if strings.EqualFold(model, normalizeOpenAICompatibilityModelName(models[i].Alias)) {
			return models[i].UseMaxCompletionTokens, true
		}
	}
	return false, false
}

// NormalizeOpenAIMaxTokens selects the token limit field accepted by the upstream model.
func NormalizeOpenAIMaxTokens(payload []byte, useMaxCompletionTokens bool) ([]byte, error) {
	if len(payload) == 0 {
		return payload, nil
	}
	preferred, other := "max_tokens", "max_completion_tokens"
	if useMaxCompletionTokens {
		preferred, other = other, preferred
	}
	if gjson.GetBytes(payload, other).Exists() {
		if !gjson.GetBytes(payload, preferred).Exists() {
			value := gjson.GetBytes(payload, other)
			var err error
			payload, err = sjson.SetRawBytes(payload, preferred, []byte(value.Raw))
			if err != nil {
				return nil, fmt.Errorf("set %s: %w", preferred, err)
			}
		}
		var err error
		payload, err = sjson.DeleteBytes(payload, other)
		if err != nil {
			return nil, fmt.Errorf("remove %s: %w", other, err)
		}
	}
	return payload, nil
}

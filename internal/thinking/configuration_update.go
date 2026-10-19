package thinking

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripConfigurationUpdates removes Responses input items the selected model cannot accept.
func stripConfigurationUpdates(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	kept := make([]string, 0, len(input.Array()))
	removed := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "configuration_update" {
			removed = true
		} else {
			kept = append(kept, item.Raw)
		}
		return true
	})
	if !removed {
		return body, nil
	}
	result, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(kept, ",")+"]"))
	if err != nil {
		return body, fmt.Errorf("remove unsupported configuration updates: %w", err)
	}
	return result, nil
}

func stripResponsesEffort(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "reasoning.effort").Exists() {
		return body, nil
	}
	result, err := sjson.DeleteBytes(body, "reasoning.effort")
	if err != nil {
		return body, fmt.Errorf("remove unsupported reasoning effort: %w", err)
	}
	if reasoning := gjson.GetBytes(result, "reasoning"); reasoning.IsObject() && len(reasoning.Map()) == 0 {
		result, err = sjson.DeleteBytes(result, "reasoning")
		if err != nil {
			return body, fmt.Errorf("remove empty reasoning config: %w", err)
		}
	}
	return result, nil
}

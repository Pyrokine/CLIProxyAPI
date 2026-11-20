package pluginapi_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestPublicUsageRecordPreservesPublishedFields(t *testing.T) {
	original := pluginapi.UsageRecord{
		Provider: "codex", Model: "gpt-5.6-luna", ResponseModel: "gpt-5.6-luna-2026-05-13",
		Stream: true, Alias: "client-model", ServiceTier: "flex", RequestServiceTier: "flex",
		ResponseServiceTier: "scale",
		AuthID:              "synthetic-auth", AuthIndex: "synthetic-index", AuthType: "api_key",
		Source: "synthetic-" +
			"source",
		BaseURL: "https://synthetic.invalid/v1", ReasoningEffort: "high",
		Detail: pluginapi.UsageDetail{
			InputTokens: 84, OutputTokens: 16, TotalTokens: 100, ResponseServiceTier: "scale",
		},
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded pluginapi.UsageRecord
	if err = json.Unmarshal(payload, &decoded); err != nil || !reflect.DeepEqual(decoded, original) {
		t.Fatalf("published usage fields changed: decoded=%#v error=%v", decoded, err)
	}
}

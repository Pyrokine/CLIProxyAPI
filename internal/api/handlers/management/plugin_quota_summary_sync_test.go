package management

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestNormalizeQuotaProbeResponseAcceptsValidatedSummary(t *testing.T) {
	input := []byte(`{"summary":[
		{"key":"credits_used","label":"Credits used","value":1740.28,"unit":"credits","format":"currency","currency":"usd"},
		{"key":"missing_label","value":5},
		{"key":"string_value","label":"Invalid","value":"5"},
		{"key":"unknown_currency","label":"Unpriced","value":3,"format":"currency","currency":"XYZ"}
	]}`)
	for _, tc := range []struct {
		name string
		load func([]byte) (pluginapi.QuotaFetchResponse, error)
	}{
		{"normalized", normalizeQuotaProbeResponse},
		{"mapped", func(data []byte) (pluginapi.QuotaFetchResponse, error) { return mapQuotaProbeResponse(data, map[string]any{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.load(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Summary) != 2 || result.Summary[0].Value != 1740.28 ||
				result.Summary[0].Format != "currency" || result.Summary[0].Currency != "USD" ||
				result.Summary[1].Format != "" || result.Summary[1].Currency != "" {
				t.Fatalf("normalized summary = %#v", result.Summary)
			}
		})
	}
}

func TestValidateQuotaFetchResponseSummaryOnly(t *testing.T) {
	valid := pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{{Key: "credits", Label: "Credits", Value: 0}}}
	if err := validateQuotaFetchResponse(valid); err != nil {
		t.Fatalf("summary-only quota rejected: %v", err)
	}
	invalid := pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{{Key: "", Label: "Credits", Value: 10}}}
	if err := validateQuotaFetchResponse(invalid); err == nil {
		t.Fatal("malformed summary must not count as usable quota")
	}
}

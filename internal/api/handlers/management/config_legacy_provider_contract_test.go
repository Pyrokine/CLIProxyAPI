package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestLegacyProviderYAMLManagementContract(t *testing.T) {
	const original = "port: 8317\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(&config.Config{Port: 8317}, path, nil, withEnvSecret(""))
	t.Cleanup(func() { _ = handler.Close() })
	const legacy = original + `generative-language-api-key: [synthetic-gemini]
openai-compatibility:
  - name: Original
    base-url: https://original.synthetic.invalid
    api-keys: [synthetic-openai]
`
	validated := runConfigListRequest(handler.ValidateConfigYAML, http.MethodPost, "/config/validate", legacy)
	var validation struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(validated.Body.Bytes(), &validation); err != nil {
		t.Fatal(err)
	}
	if validated.Code != http.StatusOK || !validation.Valid {
		t.Fatalf("legacy validation status=%d body=%s", validated.Code, validated.Body.String())
	}
	loaded, _, err := handler.validateConfigBody([]byte(legacy))
	if err != nil || loaded == nil || len(loaded.GeminiKey) != 1 || len(loaded.OpenAICompatibility) != 1 ||
		len(loaded.OpenAICompatibility[0].APIKeyEntries) != 1 {
		t.Fatalf("management validation lost typed provider keys: %+v, %v", loaded, err)
	}
	reader := NewHandler(loaded, path, nil, withEnvSecret(""))
	t.Cleanup(func() { _ = reader.Close() })
	for _, sample := range []struct {
		name string
		get  func() (int, []byte)
	}{
		{
			"gemini-api-key", func() (int, []byte) {
				result := runConfigListRequest(reader.GetGeminiKeys, http.MethodGet, "/gemini-api-key", "")
				return result.Code, result.Body.Bytes()
			},
		},
		{
			"openai-compatibility", func() (int, []byte) {
				result := runConfigListRequest(reader.GetOpenAICompat, http.MethodGet, "/openai-compatibility", "")
				return result.Code, result.Body.Bytes()
			},
		},
	} {
		status, body := sample.get()
		var result map[string][]map[string]any
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || len(result[sample.name]) != 1 {
			t.Fatalf("typed management reader %s status=%d body=%s", sample.name, status, body)
		}
	}
	for _, sample := range []struct{ name, field, body string }{
		{
			"Gemini shape", "generative-language-api-key",
			"generative-language-api-key: {api-key: synthetic-private-value}\n",
		},
		{
			"OpenAI shape", "api-keys",
			"openai-compatibility: [{name: Original, base-url: 'https://original.sy" +
				"nthetic.invalid', api-keys: {api-key: synthetic-private-value}}]\n",
		},
		{"retired Amp", "amp-upstream-api-key", "amp-upstream-api-key: synthetic-private-value\n"},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				body := original + sample.body
				validation := runConfigListRequest(
					handler.ValidateConfigYAML, http.MethodPost, "/config/validate", body,
				)
				var result struct {
					Valid  bool                    `json:"valid"`
					Errors []configValidationError `json:"errors"`
				}
				if err := json.Unmarshal(validation.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if validation.Code != http.StatusOK || result.Valid || len(result.Errors) != 1 || !strings.Contains(
					result.Errors[0].Message, sample.field,
				) || strings.Contains(validation.Body.String(), "synthetic-private-value") {
					t.Fatalf(
						"legacy management diagnostic status=%d body=%s", validation.Code, validation.Body.String(),
					)
				}
				saved := runConfigListRequest(handler.PutConfigYAML, http.MethodPut, "/config.yaml", body)
				if saved.Code != http.StatusUnprocessableEntity || !strings.Contains(
					saved.Body.String(), sample.field,
				) || strings.Contains(saved.Body.String(), "synthetic-private-value") {
					t.Fatalf("legacy invalid save status=%d body=%s", saved.Code, saved.Body.String())
				}
				content, err := os.ReadFile(path)
				if err != nil || string(content) != original {
					t.Fatalf("failed legacy save changed config: %q, %v", content, err)
				}
			},
		)
	}
}

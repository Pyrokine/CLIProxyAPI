package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestCredentialPartValidationIdentifiesActualField(t *testing.T) {
	for _, test := range []struct{ field, value string }{{"max-part-bytes", "512"}, {"max-part-count", "65"}} {
		t.Run(
			test.field, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				const original = "port: 8317\n"
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				h := NewHandler(&config.Config{Port: 8317}, path, nil, withEnvSecret(""))
				t.Cleanup(func() { _ = h.Close() })
				body := original + "credential-in-flight:\n  " + test.field + ": " + test.value + "\n"
				want := "credential-in-flight." + test.field
				validation := runConfigListRequest(h.ValidateConfigYAML, http.MethodPost, "/config/validate", body)
				var result struct {
					Valid  bool                    `json:"valid"`
					Errors []configValidationError `json:"errors"`
				}
				if err := json.Unmarshal(validation.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if validation.Code != http.StatusOK || result.Valid || len(result.Errors) != 1 ||
					result.Errors[0].Field != want {
					t.Fatalf("validation status=%d body=%s", validation.Code, validation.Body.String())
				}
				saved := runConfigListRequest(h.PutConfigYAML, http.MethodPut, "/config.yaml", body)
				var failure struct {
					Field string `json:"field"`
				}
				if err := json.Unmarshal(saved.Body.Bytes(), &failure); err != nil {
					t.Fatal(err)
				}
				if saved.Code != http.StatusUnprocessableEntity || failure.Field != want {
					t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != original {
					t.Fatalf("config=%q error=%v", data, err)
				}
			},
		)
	}
}

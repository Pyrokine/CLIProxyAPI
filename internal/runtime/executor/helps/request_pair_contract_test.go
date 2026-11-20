package helps_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAttachmentRequestPairUsesOnlyWorkingTranslationError(t *testing.T) {
	const invalid = `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":` +
		`[{"type":"container_upload","file_id":` +
		`"file-1"}]}]}`
	const valid = `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":` +
		`[{"type":"text","text":"keep ` +
		`me"}]}]}`
	for _, target := range []sdktranslator.Format{
		sdktranslator.FormatAntigravity, sdktranslator.FormatOpenAI, sdktranslator.FormatGemini,
		sdktranslator.FormatCodex, sdktranslator.FormatInteractions,
	} {
		for _, compat := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, sample := range []struct {
					name, baseline, working string
					reject                  bool
					shared                  bool
				}{
					{name: "invalid-baseline", baseline: invalid, working: valid},
					{name: "invalid-working", baseline: valid, working: invalid, reject: true},
					{name: "shared-invalid", working: invalid, reject: true, shared: true},
					{name: "shared-valid", working: valid, shared: true},
				} {
					t.Run(
						fmt.Sprintf("%s/compat=%t/stream=%t/%s", target, compat, stream, sample.name),
						func(t *testing.T) {
							working := []byte(sample.working)
							baseline := []byte(sample.baseline)
							if sample.shared {
								baseline = working
							}
							original, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
								t.Context(),
								nil,
								nil,
								sdktranslator.FormatClaude,
								target,
								"m",
								baseline,
								working,
								stream,
								compat,
							)
							if sample.reject {
								var part *translatorcommon.UnsupportedPartError
								if !errors.As(err, &part) || part.Type != "container_upload" ||
									part.StatusCode() != 400 || !part.IsRequestScoped() || body != nil {
									t.Fatalf("working attachment error=%v body=%s", err, body)
								}
								return
							}
							if err != nil || !gjson.ValidBytes(body) ||
								!strings.Contains(string(body), "keep me") {
								t.Fatalf(
									"valid working payload rejected or changed: error=%v body=%s",
									err,
									body,
								)
							}
							if sample.shared {
								original[0] = '['
								if body[0] != '{' || string(working) != valid {
									t.Fatal(
										"baseline, working translation or caller payload share mutable bytes",
									)
								}
							}
						},
					)
				}
			}
		}
	}
}

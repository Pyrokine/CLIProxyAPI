package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiExecutorWarnsOnlyForUnexpectedUpstreamModel(t *testing.T) {
	for _, test := range []struct {
		name, alias, served string
		warnings            int
	}{
		{"mapped model", "kimi-mapped-no-warn-test", "kimi-for-coding", 0},
		{"unexpected model", "kimi-unexpected-warn-test", "unexpected-model-v2", 1},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				hook := captureResponseModelWarnings(t)
				ctx, finish := xaiOutcomeContext(t)
				ctx = usage.WithRequestedModelAlias(ctx, test.alias)
				ctx = util.WithRoundTripper(
					ctx, kimiTokenRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							body, err := io.ReadAll(req.Body)
							if err != nil {
								return nil, err
							}
							if model := gjson.GetBytes(body, "model").String(); model != "kimi-for-coding" {
								t.Fatalf("upstream request model = %q, want kimi-for-coding", model)
							}
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": {"application/json"}},
								Body: io.NopCloser(
									strings.NewReader(
										(`{"id":"chatcmpl-123","object":"chat.completion","model":` +
											`"`) +
											test.served +
											(`","choices":` +
												`[{"message":` +
												`{"role":` +
												`"assistant",` +
												`"content":` +
												`"hello"}}],` +
												`"usage":` +
												`{"total_tokens` +
												`":10}}`),
									),
								),
							}, nil
						},
					),
				)
				exec := NewKimiExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Provider: "kimi", Attributes: map[string]string{},
					Metadata: map[string]any{"access_token": "test-key"},
				}
				response, err := exec.Execute(
					ctx, auth, cliproxyexecutor.Request{
						Model:   "kimi-k2.8",
						Payload: []byte(`{"model":"kimi-k2.8","messages":[{"role":"user","content":"hello"}]}`),
					}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
				)
				if err != nil || len(response.Payload) == 0 {
					t.Fatalf("Execute() error = %v, payload = %s", err, response.Payload)
				}
				records := finish()
				if len(records) != 1 {
					t.Fatalf("usage records = %d, want 1", len(records))
				}
				if record := records[0]; record.Model != "kimi-k2.8" || record.ResponseModel != test.served ||
					record.Alias != test.alias {
					t.Fatalf("logical and served model or alias changed: %+v", record)
				}
				warnings := responseModelWarningMessages(hook)
				if len(warnings) != test.warnings {
					t.Fatalf("model warnings = %#v, want %d", warnings, test.warnings)
				}
				if len(warnings) > 0 && !strings.Contains(warnings[0], test.served) {
					t.Fatalf("model warning lost the served model: %s", warnings[0])
				}
			},
		)
	}
}

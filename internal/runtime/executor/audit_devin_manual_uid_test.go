package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditDevinManualSlowUIDRemainsCallableWithoutCatalog(t *testing.T) {
	const uid = "manual.Synthetic-slow"
	for _, model := range []string{uid, "devin/" + uid, "devin/" + uid + ":high", "devin/" + uid + "(low)"} {
		for _, streaming := range []bool{false, true} {
			name := model + "/execute"
			if streaming {
				name = model + "/stream"
			}
			t.Run(
				name, func(t *testing.T) {
					resolved, info, err := helps.ResolveDevinChatModel(model, "high", 32768)
					if err != nil || resolved != uid || info != nil {
						t.Fatalf("uid=%q info=%+v error=%v", resolved, info, err)
					}
					if advertised := registry.LookupDevinModel("manual-Synthetic-slow"); advertised != nil {
						t.Fatalf("manual UID appeared in static catalog: %+v", advertised)
					}
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(request.Body)
								if err != nil || len(body) < 5 {
									t.Errorf("Connect frame length=%d error=%v", len(body), err)
								} else {
									_, _, err = helps.FinalizeDevinPayload(
										body[5:], func(view []byte) ([]byte, error) {
											if gjson.GetBytes(view, "model").String() != uid {
												t.Errorf("manual UID changed in protobuf: %s", view)
											}
											return view, nil
										},
									)
									if err != nil {
										t.Error(err)
									}
								}
								wire := helps.WrapConnectEnvelope(
									testDevinBytesField(
										nil, 3, []byte("synthetic answer"),
									),
								)
								wire = append(
									wire,
									helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`))...,
								)
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"application/connect+proto"}},
									Body:       io.NopCloser(strings.NewReader(string(wire))), Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "devin", Attributes: map[string]string{
							"api_key": "synthetic-session", "base_url": "https://devin.synthetic.invalid",
						},
					}
					err = auditFinalUsageCall(
						ctx, NewDevinExecutor(&config.Config{}), credential, core.Request{
							Model: model, Payload: []byte(`{"input":[{"type":"user_input","content":"synthetic"}]}`),
						}, core.Options{
							SourceFormat: translator.FormatInteractions, ResponseFormat: translator.FormatInteractions,
						}, streaming,
					)
					if err != nil || calls != 1 {
						t.Fatalf("error=%v calls=%d", err, calls)
					}
				},
			)
		}
	}
	if _, _, err := helps.ResolveDevinChatModel("devin/not-a-model-high", "", 0); err == nil {
		t.Fatal("unknown non-manual model remained callable")
	}
}

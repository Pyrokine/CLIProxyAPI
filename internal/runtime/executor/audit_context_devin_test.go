package executor

import (
	"fmt"
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

func TestAuditDevinContextChecksFinalWireUID(t *testing.T) {
	originalUID, _, err := helps.ResolveDevinChatModel("devin/swe-2", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, streaming := range []bool{false, true} {
		for _, target := range []string{"same", "small", "other-capable"} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", streaming, target), func(t *testing.T) {
					final := originalUID
					if target == "small" {
						final = "MODEL_SYNTHETIC_SMALL"
					} else if target == "other-capable" {
						final = "MODEL_SYNTHETIC_HIGH"
					}
					cfg := &config.Config{}
					cfg.Payload.Override = []config.PayloadRule{
						{
							Models: []config.PayloadModelRule{
								{
									Name: "devin/swe-2", Protocol: "devin",
								},
							}, Params: map[string]any{"model": final},
						},
					}
					manager := auth.NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
					manager.SetConfig(cfg)
					manager.RegisterExecutor(NewDevinExecutor(cfg))
					credential, err := manager.Register(
						t.Context(), &auth.Auth{
							ID: t.Name(), Provider: "devin", Attributes: map[string]string{
								"api_key": "synthetic-session", "base_url": "https://devin.synthetic.invalid",
							},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(
						credential.ID, "devin", []*registry.ModelInfo{
							{ID: "devin/swe-2", ContextLength: 1_000_000},
							{ID: "devin/MODEL_SYNTHETIC_SMALL", ContextLength: 128_000},
							{ID: "devin/MODEL_SYNTHETIC_HIGH", MaxContextLength: 1_000_000},
						},
					)
					t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
					manager.RefreshSchedulerEntry(credential.ID)
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(request.Body)
								if err != nil || len(body) < 5 {
									t.Errorf("invalid Connect envelope: length=%d error=%v", len(body), err)
								} else {
									_, _, err = helps.FinalizeDevinPayload(
										body[5:], func(view []byte) ([]byte, error) {
											if gjson.GetBytes(view, "model").String() != final {
												t.Errorf("wire model=%s", view)
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
					request := core.Request{
						Model:   "devin/swe-2[1m]",
						Payload: []byte(`{"input":[{"type":"user_input","content":"synthetic"}]}`),
					}
					opts := core.Options{
						SourceFormat: translator.FormatInteractions, ResponseFormat: translator.FormatInteractions,
					}
					if streaming {
						var result *core.StreamResult
						result, err = manager.ExecuteStream(ctx, []string{"devin"}, request, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = manager.Execute(ctx, []string{"devin"}, request, opts)
					}
					if target == "small" {
						status, ok := err.(interface{ StatusCode() int })
						if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 {
							t.Fatalf("error=%v calls=%d", err, calls)
						}
					} else if err != nil || calls != 1 {
						t.Fatalf("error=%v calls=%d", err, calls)
					}
				},
			)
		}
	}
}

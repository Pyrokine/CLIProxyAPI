package auth_test

import (
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestPublicSessionIdentityReachesStableCredentialIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headers  http.Header
		payload  string
		metadata map[string]any
		want     string
	}{
		{
			name: "nested child", payload: `{"request":{"sessionId":"root","metadata":{"agent_id":"worker"}}}`,
			want: "session:root:agent:worker",
		},
		{name: "nested parent", payload: `{"request":{"sessionId":"root"}}`, want: "session:root"},
		{
			name:     "canonical metadata",
			metadata: map[string]any{core.CanonicalSessionIDMetadataKey: "ctx:v1:canonical"}, want: "ctx:v1:canonical",
		},
		{
			name: "canonical before content hash", payload: `{"messages":[{"role":"user","content":"hello"}]}`,
			metadata: map[string]any{core.CanonicalSessionIDMetadataKey: "ctx:v1:canonical"}, want: "ctx:v1:canonical",
		},
		{
			name: "canonical before LCP", metadata: map[string]any{
				core.CanonicalSessionIDMetadataKey:   "ctx:v1:canonical",
				core.LCPAffinitySessionIDMetadataKey: "lcp:v1:other",
			}, want: "ctx:v1:canonical",
		},
		{
			name: "LCP metadata", metadata: map[string]any{core.LCPAffinitySessionIDMetadataKey: "lcp:v1:bound"},
			want: "lcp:v1:bound",
		},
		{
			name: "header before canonical", headers: http.Header{"X-Session-ID": {"explicit"}},
			metadata: map[string]any{core.CanonicalSessionIDMetadataKey: "ctx:v1:canonical"}, want: "header:explicit",
		},
		{
			name: "execution remains explicit", metadata: map[string]any{
				core.ExecutionSessionMetadataKey: "explicit", core.CanonicalSessionIDMetadataKey: "ctx:v1:canonical",
			}, want: "execution:explicit",
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				payload := []byte(tc.payload)
				if got := auth.CanonicalSessionID(
					tc.headers, payload, tc.metadata,
				); got != session.BoundSessionIdentity(tc.want) {
					t.Errorf("reporting identity=%q, want %q", got, tc.want)
				}
				first := helps.ClaudeAgentSessionUUIDForRequest(tc.headers, payload, nil, true, tc.metadata)
				second := helps.ClaudeAgentSessionUUIDForRequest(tc.headers, payload, nil, true, tc.metadata)
				if first == "" || first != second {
					t.Fatalf("credential session is not stable: %q/%q", first, second)
				}
				if tc.want == "session:root:agent:worker" {
					parent := helps.ClaudeAgentSessionUUIDForRequest(
						nil, []byte(`{"request":{"sessionId":"root"}}`), nil, true,
					)
					if first == parent {
						t.Fatal("child credential session merged with parent")
					}
				}
			},
		)
	}
}

func TestPublicSessionIdentityReachesEveryManagerEntry(t *testing.T) {
	for _, identity := range []string{"nested", "canonical", "header"} {
		for _, mode := range []string{"execute", "count", "stream"} {
			t.Run(
				identity+"/"+mode, func(t *testing.T) {
					const provider, model = "session-identity-fixture", "fixture-model"
					manager := auth.NewManager(nil, nil, nil)
					manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
					capture := &auditConfigIdentityExecutor{provider: provider}
					manager.RegisterExecutor(capture)
					credential, err := manager.Register(
						t.Context(), &auth.Auth{
							ID: t.Name(), Provider: provider,
							Attributes: map[string]string{"api_key": "synthetic"},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(credential.ID, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
					manager.RefreshSchedulerEntry(credential.ID)
					req := core.Request{
						Model:   model,
						Payload: []byte(`{"request":{"sessionId":"root","metadata":{"agent_id":"worker"}}}`),
					}
					opts := core.Options{SourceFormat: translator.FormatOpenAI}
					want := "root:agent:worker"
					if identity == "canonical" {
						req.Payload = []byte(`{}`)
						opts.Metadata = map[string]any{core.CanonicalSessionIDMetadataKey: "ctx:v1:canonical"}
						want = "ctx:v1:canonical"
					} else if identity == "header" {
						req.Payload = []byte(`{}`)
						opts.Headers = http.Header{"X-Session-Id": {"caller-session"}}
						want = "caller-session"
					}
					switch mode {
					case "execute":
						_, err = manager.Execute(t.Context(), []string{provider}, req, opts)
					case "count":
						_, err = manager.ExecuteCount(t.Context(), []string{provider}, req, opts)
					case "stream":
						var result *core.StreamResult
						result, err = manager.ExecuteStream(t.Context(), []string{provider}, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
						}
					}
					if err != nil || capture.calls != 1 || capture.seenSession != want {
						t.Fatalf(
							"Manager %s: session=%q want=%q calls=%d error=%v", mode, capture.seenSession, want,
							capture.calls, err,
						)
					}
					wantCanonical := want
					if identity == "nested" {
						wantCanonical = "session:" + want
					} else if identity == "header" {
						wantCanonical = "header:" + want
					}
					if got, _ := capture.seen.Metadata[core.CanonicalSessionIDMetadataKey].(string); got !=
						wantCanonical {
						t.Fatalf("executor canonical identity=%q, want %q", got, wantCanonical)
					}
				},
			)
		}
	}
}

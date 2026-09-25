package executor

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiFingerprintCountInterceptorMatchesWire(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		t.Run(format.String(), func(t *testing.T) {
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(NewKimiExecutor(&config.Config{}))
			a := &cliproxyauth.Auth{ID: "synthetic-kimi-count-" + format.String(), Provider: "kimi", Status: cliproxyauth.StatusActive, Metadata: map[string]any{"access_token": "synthetic-kimi"}}
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(a.ID, "kimi", []*registry.ModelInfo{{ID: "kimi-k2.5"}})
			t.Cleanup(func() { reg.UnregisterClient(a.ID) })
			manager.RefreshSchedulerEntry(a.ID)
			wireCalls, interceptorCalls := 0, 0
			ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				wireCalls++
				if r.URL.Hostname() != "api.kimi.com" || r.URL.Path != "/coding/v1/messages/count_tokens" {
					t.Fatalf("unexpected Provider/control request: %s", r.URL)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !gjson.GetBytes(body, "messages").IsArray() || !strings.Contains(gjson.GetBytes(body, "messages.0.content").String(), "synthetic count") {
					t.Fatalf("count request was not translated to Claude: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"input_tokens":17}`)), Request: r}, nil
			}))
			opts := cliproxyexecutor.Options{SourceFormat: format, RequestAfterAuthInterceptor: func(_ context.Context, req cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
				interceptorCalls++
				if req.SourceFormat != format || req.ToFormat != sdktranslator.FormatClaude || req.AuthID != a.ID {
					t.Errorf("interceptor source=%s target=%s auth=%s", req.SourceFormat, req.ToFormat, req.AuthID)
				}
				return cliproxyexecutor.RequestAfterAuthInterceptResponse{}
			}}
			payload := `{"model":"kimi-k2.5","messages":[{"role":"user","content":"synthetic count"}]}`
			if format == sdktranslator.FormatOpenAIResponse {
				payload = `{"model":"kimi-k2.5","input":[{"role":"user","content":[{"type":"input_text","text":"synthetic count"}]}]}`
			}
			_, err := manager.ExecuteCount(ctx, []string{"kimi"}, cliproxyexecutor.Request{Model: "kimi-k2.5", Payload: []byte(payload)}, opts)
			if err != nil || wireCalls != 1 || interceptorCalls != 1 {
				t.Fatalf("wire=%d interceptor=%d err=%v", wireCalls, interceptorCalls, err)
			}
		})
	}
}

func TestKimiFingerprintMessagesKeepProviderAndStableIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		auth := fingerprintAuth("claude-code-cli", "")
		auth.Provider = "kimi"
		delete(auth.Attributes, "api_key")
		auth.Metadata = map[string]any{"access_token": "synthetic-first-token"}
		var identities []string
		calls := 0
		ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Hostname() != "api.kimi.com" || r.URL.Path != "/coding/v1/messages" {
				t.Fatalf("unexpected Provider/control request: %s", r.URL)
			}
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(gjson.GetBytes(body, "system.0.text").String(), "cch=") {
				t.Fatal("Kimi CLI signed billing")
			}
			if r.Header.Get("Authorization") != "Bearer "+auth.Metadata["access_token"].(string) {
				t.Fatal("CLI profile changed Kimi auth")
			}
			identities = append(identities, gjson.GetBytes(body, "metadata.user_id").String())
			name := gjson.GetBytes(body, "tools.0.name").String()
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fingerprintResponse(name, stream))), Request: r}, nil
		}))
		e := NewKimiExecutor(&config.Config{})
		for _, token := range []string{"synthetic-first-token", "synthetic-next-token"} {
			auth.Metadata["access_token"] = token
			before := auth.Clone()
			if stream {
				result, err := e.ExecuteStream(ctx, auth, fingerprintRequest(fingerprintPayload), fingerprintOptions())
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := e.Execute(ctx, auth, fingerprintRequest(fingerprintPayload), fingerprintOptions()); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(auth.Attributes, before.Attributes) || !reflect.DeepEqual(auth.Metadata, before.Metadata) {
				t.Fatal("Kimi changed shared credential")
			}
		}
		if calls != 2 || identities[0] == "" || identities[0] != identities[1] {
			t.Fatalf("identity changed across token rotation: %v", identities)
		}
	}
}

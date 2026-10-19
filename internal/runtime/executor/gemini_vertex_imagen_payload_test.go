package executor

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func googleImagenServiceAccount(t *testing.T) map[string]any {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"type": "service_account", "project_id": "synthetic-project", "private_key_id": "synthetic-key-id",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "client_email": "synthetic@synthetic.invalid", "token_uri": "https://imagen-token.invalid/token"}
}

func TestVertexImagenPayloadRules(t *testing.T) {
	for _, kind := range []string{"override", "default", "filter", "none"} {
		t.Run(kind, func(t *testing.T) {
			models := []config.PayloadModelRule{{Name: "imagen-contract", Protocol: "gemini", FromProtocol: "gemini", Headers: map[string]string{"X-Payload-Test": "match"}}}
			cfg := &config.Config{}
			wantPrompt, wantAspect := "original", "4:3"
			switch kind {
			case "override":
				cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"instances.0.prompt": "configured", "parameters.sampleCount": 2}}}
				wantPrompt = "configured"
			case "default":
				cfg.Payload.Default = []config.PayloadRule{{Models: models, Params: map[string]any{"instances.0.prompt": "unused", "parameters.aspectRatio": "unused", "parameters.personGeneration": "allow_adult"}}}
			case "filter":
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"parameters.aspectRatio", "instances.0.negativePrompt"}}}
				wantAspect = ""
			}
			tokenCalls, modelCalls := 0, 0
			var sent []byte
			ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host == "imagen-token.invalid" && req.URL.Path == "/token" {
					tokenCalls++
					resp := googlePayloadResponse(req, `{"access_token":"synthetic-token","expires_in":3600,"token_type":"Bearer"}`)
					resp.Header.Set("Content-Type", "application/json")
					return resp, nil
				}
				if req.URL.Host != "aiplatform.googleapis.com" || !strings.HasSuffix(req.URL.Path, ":predict") {
					return nil, fmt.Errorf("unexpected request %s", req.URL)
				}
				modelCalls++
				if req.Header.Get("Authorization") != "Bearer synthetic-token" {
					return nil, fmt.Errorf("missing synthetic token")
				}
				var err error
				sent, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				return googlePayloadResponse(req, `{"predictions":[{"bytesBase64Encoded":"AQ==","mimeType":"image/png"}]}`), nil
			}))
			auth := &cliproxyauth.Auth{ID: "imagen-contract-" + kind, Provider: "vertex", Metadata: map[string]any{"project_id": "synthetic-project", "location": "global", "service_account": googleImagenServiceAccount(t)}}
			response, err := NewGeminiVertexExecutor(cfg).Execute(ctx, auth, cliproxyexecutor.Request{Model: "imagen-contract", Payload: []byte(`{"prompt":"original","aspectRatio":"4:3","negativePrompt":"avoid"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, Headers: http.Header{"X-Payload-Test": {"match"}}})
			if err != nil {
				t.Fatal(err)
			}
			if tokenCalls != 1 || modelCalls != 1 {
				t.Fatalf("token=%d model=%d", tokenCalls, modelCalls)
			}
			if gjson.GetBytes(sent, "instances.0.prompt").String() != wantPrompt || gjson.GetBytes(sent, "parameters.aspectRatio").String() != wantAspect {
				t.Fatalf("sent=%s", sent)
			}
			if kind == "override" && gjson.GetBytes(sent, "parameters.sampleCount").Int() != 2 {
				t.Fatalf("sample count missing: %s", sent)
			}
			if kind == "default" && gjson.GetBytes(sent, "parameters.personGeneration").String() != "allow_adult" {
				t.Fatalf("default missing: %s", sent)
			}
			if kind == "filter" && gjson.GetBytes(sent, "instances.0.negativePrompt").Exists() {
				t.Fatalf("filter missing: %s", sent)
			}
			var decoded map[string]any
			if err := json.Unmarshal(response.Payload, &decoded); err != nil || !gjson.GetBytes(response.Payload, "candidates.0.content.parts.0.inlineData").IsObject() {
				t.Fatalf("image response=%s error=%v", response.Payload, err)
			}
		})
	}
}

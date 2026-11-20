package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestGeminiExecutorRecordsSuccessfulZeroUsageInQueue(t *testing.T) {
	model := fmt.Sprintf("gemini-2.5-flash-zero-usage-%d", time.Now().UnixNano())
	source := fmt.Sprintf("zero-usage-%d@example.com", time.Now().UnixNano())
	const requestBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/v1beta/models/" + model + ":generateContent"
				if r.URL.Path != wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(body), `"hi"`) {
					t.Errorf("Gemini request body = %s, error = %v", body, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write(
					[]byte((`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"fi` +
						`nishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"candidates` +
						`TokenCount":0,"totalTokenCount":0}}`)),
				); err != nil {
					t.Error(err)
				}
			},
		),
	)
	defer server.Close()

	build := func() *Service {
		cfg := &config.Config{AuthDir: t.TempDir(), UsageStatisticsEnabled: true}
		cfg.Plugins.Dir = t.TempDir()
		service, err := NewBuilder().WithConfig(cfg).
			WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
			WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).Build()
		if err != nil {
			t.Fatal(err)
		}
		service.usageQueue.SetEnabled(true)
		service.RegisterUsagePlugin(redisqueue.NewUsagePluginForQueue(service.usageQueue))
		t.Cleanup(
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := service.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			},
		)
		return service
	}
	service, peer := build(), build()
	if service.usageManager == peer.usageManager || service.usageQueue == peer.usageQueue {
		t.Fatal("Service instances share usage delivery state")
	}
	ctx := coreusage.WithManager(t.Context(), service.usageManager)
	executor := runtimeexecutor.NewGeminiExecutor(service.cfg)
	auth := &coreauth.Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "test-upstream-key", "base_url": server.URL,
		},
		Metadata: map[string]any{"email": source},
	}
	response, err := executor.Execute(
		ctx, auth, coreexecutor.Request{
			Model: model, Payload: []byte(requestBody),
		}, coreexecutor.Options{SourceFormat: translator.FormatGemini, OriginalRequest: []byte(requestBody)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Payload), `"ok"`) {
		t.Fatalf("Gemini Execute payload=%s error=%v", response.Payload, err)
	}
	drainCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := service.usageManager.StopContext(drainCtx); err != nil {
		t.Fatal(err)
	}
	items := service.usageQueue.PopOldest(10)
	if len(items) != 1 {
		t.Fatalf("queued Gemini records = %d, want one", len(items))
	}
	var record struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Source   string `json:"source"`
		Failed   bool   `json:"failed"`
		Tokens   struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(items[0], &record); err != nil {
		t.Fatal(err)
	}
	if record.Provider != "gemini" || record.Model != model || record.Failed ||
		record.Tokens.InputTokens != 0 || record.Tokens.OutputTokens != 0 || record.Tokens.TotalTokens != 0 {
		t.Fatalf("queued Gemini zero-usage result = %+v", record)
	}
	if record.Source != util.HideAPIKey("test-upstream-key") || auth.Metadata["email"] != source {
		t.Fatalf("credential source changed: queued=%q metadata=%v", record.Source, auth.Metadata)
	}
	if items := peer.usageQueue.PopOldest(10); len(items) != 0 {
		t.Fatalf("Gemini usage reached the peer Service queue: %d records", len(items))
	}
}

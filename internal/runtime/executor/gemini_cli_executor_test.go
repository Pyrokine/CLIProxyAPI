package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/geminicli"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

func TestGeminiCLIExecutorRefreshBuildsPrimaryPersistenceForVirtualCredential(t *testing.T) {
	tokenServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if errParse := r.ParseForm(); errParse != nil {
					t.Errorf("ParseForm() error = %v", errParse)
					http.Error(w, "invalid form", http.StatusBadRequest)
					return
				}
				if got := r.Form.Get("refresh_token"); got != "old-refresh" {
					t.Errorf("refresh_token = %q, want old-refresh", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(
					map[string]any{
						"access_token":  "new-access",
						"refresh_token": "new-refresh",
						"token_type":    "Bearer",
						"expires_in":    3600,
					},
				)
			},
		),
	)
	defer tokenServer.Close()

	previousTokenParser := geminiTokenAndConfigFromMetadata
	geminiTokenAndConfigFromMetadata = func(metadata map[string]any) (map[string]any, *oauth2.Token, *oauth2.Config) {
		return map[string]any{"refresh_token": "old-refresh"}, &oauth2.Token{
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour),
		}, &oauth2.Config{
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint: oauth2.Endpoint{
				TokenURL: tokenServer.URL,
			},
		}
	}
	t.Cleanup(func() { geminiTokenAndConfigFromMetadata = previousTokenParser })

	shared := geminicli.NewSharedCredential(
		"primary.json",
		"user@example.com",
		map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"project_id":    "project-a,project-b",
			"type":          "gemini-cli",
		},
		[]string{"project-a", "project-b"},
	)
	virtualRuntime := geminicli.NewVirtualCredential("project-a", shared)
	auth := &cliproxyauth.Auth{
		ID:       "primary.json::project-a",
		Provider: "gemini-cli",
		Attributes: map[string]string{
			"runtime_only":          "true",
			"gemini_virtual_parent": "primary.json",
			"path":                  "/tmp/primary.json",
		},
		Metadata: map[string]any{"project_id": "project-a", "virtual": true},
		Runtime:  virtualRuntime,
	}

	updated, errRefresh := NewGeminiCLIExecutor(nil).Refresh(context.Background(), auth)
	if errRefresh != nil {
		t.Fatalf("Refresh() error = %v", errRefresh)
	}
	if updated == nil || updated.Runtime != virtualRuntime {
		t.Fatalf("updated runtime = %#v, want original virtual runtime", updated)
	}
	if updated.Metadata["access_token"] != "new-access" || updated.Metadata["refresh_token"] != "new-refresh" {
		t.Fatalf("updated metadata = %#v", updated.Metadata)
	}
	primary := updated.RefreshPersistence
	if primary == nil || primary.ID != "primary.json" || primary.Runtime != shared {
		t.Fatalf("primary persistence update = %#v", primary)
	}
	if primary.Metadata["access_token"] != "new-access" || primary.Metadata["refresh_token"] != "new-refresh" ||
		primary.Metadata["project_id"] != "project-a,project-b" {
		t.Fatalf("primary metadata = %#v", primary.Metadata)
	}
	if primary.Attributes["runtime_only"] != "" || primary.Attributes["path"] != "/tmp/primary.json" {
		t.Fatalf("primary attributes = %#v", primary.Attributes)
	}
	if shared.MetadataSnapshot()["access_token"] != "old-access" {
		t.Fatalf("shared metadata published before manager commit: %#v", shared.MetadataSnapshot())
	}
	if updated.RefreshCommit == nil {
		t.Fatal("Refresh() did not return a shared metadata commit guard")
	}
	if !updated.RefreshCommit.CommitRefresh(func() bool { return true }) {
		t.Fatal("shared metadata commit was rejected")
	}
	if shared.MetadataSnapshot()["access_token"] != "new-access" {
		t.Fatalf("shared metadata after commit = %#v", shared.MetadataSnapshot())
	}
}

func TestPrepareGeminiCLITokenSourceDoesNotRefreshOutsideManager(t *testing.T) {
	var refreshRequests atomic.Int32
	tokenServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				refreshRequests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"unexpected","token_type":"Bearer","expires_in":3600}`))
			},
		),
	)
	defer tokenServer.Close()

	previousTokenParser := geminiTokenAndConfigFromMetadata
	geminiTokenAndConfigFromMetadata = func(map[string]any) (map[string]any, *oauth2.Token, *oauth2.Config) {
		return map[string]any{"refresh_token": "refresh"}, &oauth2.Token{
			AccessToken:  "expired-access",
			RefreshToken: "refresh",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(-time.Minute),
		}, &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: tokenServer.URL}}
	}
	t.Cleanup(func() { geminiTokenAndConfigFromMetadata = previousTokenParser })

	auth := &cliproxyauth.Auth{
		ID:       "gemini.json",
		Provider: "gemini-cli",
		Metadata: map[string]any{"type": "gemini-cli"},
	}
	if !NewGeminiCLIExecutor(nil).ShouldRefresh(time.Now(), auth) {
		t.Fatal("expired Gemini CLI token did not request managed refresh")
	}
	_, _, errSource := prepareGeminiCLITokenSource(context.Background(), nil, auth)
	if errSource == nil || !strings.Contains(errSource.Error(), "managed refresh required") {
		t.Fatalf("prepareGeminiCLITokenSource() error = %v", errSource)
	}
	if refreshRequests.Load() != 0 {
		t.Fatalf("request-time token source sent %d refresh requests", refreshRequests.Load())
	}
}

func TestCleanGeminiCLIRequestSchemas(t *testing.T) {
	input := []byte(`{
		"request": {
			"tools": [{
				"functionDeclarations": [{
					"name": "lookup",
					"parameters": {
						"$schema": "http://json-schema.org/draft-07/schema#",
						"type": "object",
						"properties": {
							"mode": {
								"type": "string",
								"enum": ["fast", "deep"],
								"enumTitles": ["Fast", "Deep"]
							}
						}
					}
				}]
			}],
			"generationConfig": {
				"responseSchema": {
					"type": "object",
					"properties": {
						"payload": {
							"type": "object",
							"prefill": "remove me",
							"properties": {"value": {"type": "string"}}
						}
					}
				}
			}
		}
	}`)

	output := cleanGeminiCLIRequestSchemas(input)

	if gjson.GetBytes(output, "request.tools.0.functionDeclarations.0.parameters.$schema").Exists() {
		t.Fatalf("tool parameters still contain $schema: %s", output)
	}
	if gjson.GetBytes(output, "request.tools.0.functionDeclarations.0.parameters.properties.mode.enumTitles").Exists() {
		t.Fatalf("tool parameters still contain enumTitles: %s", output)
	}
	if gjson.GetBytes(output, "request.generationConfig.responseSchema.properties.payload.prefill").Exists() {
		t.Fatalf("response schema still contains prefill: %s", output)
	}
	if !gjson.GetBytes(
		output, "request.tools.0.functionDeclarations.0.parameters.properties.mode.description",
	).Exists() {
		t.Fatalf("enum description was not preserved after cleanup: %s", output)
	}
}

func TestParseRetryDelayHumanDuration(t *testing.T) {
	for _, test := range []struct {
		text  string
		want  time.Duration
		valid bool
	}{
		{"1h43m56s", time.Hour + 43*time.Minute + 56*time.Second, true},
		{"20s", 20 * time.Second, true}, {"0.25s", 250 * time.Millisecond, true},
		{"1ms", time.Millisecond, true}, {"373.801628ms", 373801628 * time.Nanosecond, true},
		{".25s", 250 * time.Millisecond, true}, {"0s", 0, true},
		{"1month", 0, false}, {"1snope", 0, false}, {"-1s", 0, false},
	} {
		t.Run(test.text, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"error": map[string]any{"message": "Your quota will reset after " + test.text + "."}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseRetryDelay(body)
			if !test.valid {
				if err == nil || got != nil {
					t.Fatalf("invalid reset duration accepted: %v, %v", got, err)
				}
				return
			}
			if err != nil || got == nil || *got != test.want {
				t.Fatalf("retry delay = %v, %v, want %v", got, err, test.want)
			}
		})
	}
	got, err := parseRetryDelay([]byte(`{"error":{"message":"Your quota will reset after 1h.","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]}}`))
	if err != nil || got == nil || *got != 2*time.Second {
		t.Fatalf("structured retry priority = %v, %v", got, err)
	}
}

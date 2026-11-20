package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestApplyClaudeHeaders_LegacyModeFallsBackToRuntimeOSArchWhenMissing(t *testing.T) {
	checkClaudeLegacyRuntimePlatform(t, true, "auth-legacy-runtime-os-arch", "key-legacy-runtime-os-arch")
}

func TestApplyClaudeHeaders_UnsetStabilizationAlsoUsesLegacyRuntimeOSArchFallback(t *testing.T) {
	checkClaudeLegacyRuntimePlatform(t, false, "auth-unset-runtime-os-arch", "key-unset-runtime-os-arch")
}

func checkClaudeLegacyRuntimePlatform(t *testing.T, configured bool, id, key string) {
	t.Helper()
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent: "claude-cli/2.1.60 (external, cli)", PackageVersion: "0.70.0", RuntimeVersion: "v22.0.0",
			OS: "MacOS", Arch: "arm64",
		},
	}
	if configured {
		cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile = new(false)
	}
	credential := &cliproxyauth.Auth{ID: id, Attributes: map[string]string{"api_key": key}}
	request := newClaudeHeaderTestRequest(t, http.Header{"User-Agent": {"curl/8.7.1"}})
	if err := applyClaudeHeaders(request, credential, key, false, nil, nil, cfg, nil, false); err != nil {
		t.Fatal(err)
	}
	wantOS, wantArch := claudeRuntimePlatformForTest()
	if request.Header.Get("X-Stainless-Os") != wantOS || request.Header.Get("X-Stainless-Arch") != wantArch {
		t.Fatalf(
			"platform=%s/%s, want runtime %s/%s", request.Header.Get("X-Stainless-Os"),
			request.Header.Get("X-Stainless-Arch"), wantOS, wantArch,
		)
	}
	if request.Header.Get("User-Agent") != "curl/8.7.1" || request.Header.Get("X-App") != "" ||
		strings.Contains(
			request.Header.Get("Anthropic-Beta"), claudeOAuthBeta,
		) {
		t.Fatalf("legacy platform setting changed caller-owned identity: %v", request.Header)
	}
}

func claudeRuntimePlatformForTest() (string, string) {
	os := map[string]string{
		"darwin": "MacOS", "windows": "Windows", "linux": "Linux", "freebsd": "FreeBSD",
	}[runtime.GOOS]
	if os == "" {
		os = "Other::" + runtime.GOOS
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "386": "x86"}[runtime.GOARCH]
	if arch == "" {
		arch = "other::" + runtime.GOARCH
	}
	return os, arch
}

func TestClaudeLegacyPlatformReachesExecutionEntries(t *testing.T) {
	const model = "claude-synthetic-platform-contract"
	registry.GetGlobalRegistry().RegisterClient(t.Name(), "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(t.Name()) })
	for _, profile := range []string{"default", "caller-owned", "claude-code-cli", "oauth"} {
		for _, stabilization := range []string{"unset", "false", "true"} {
			for _, suppliedPlatform := range []bool{false, true} {
				for _, entry := range []string{"execute", "stream", "count"} {
					t.Run(
						fmt.Sprintf(
							"%s/stabilize=%s/supplied=%t/%s",
							profile,
							stabilization,
							suppliedPlatform,
							entry,
						),
						func(t *testing.T) {
							cfg := &config.Config{
								ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
									UserAgent: "claude-cli/2.1.60 (external, cli)", PackageVersion: "0.70.0",
									RuntimeVersion: "v22.0.0", OS: "MacOS", Arch: "arm64",
								},
							}
							if stabilization != "unset" {
								cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile = new(stabilization == "true")
							}
							key := "synthetic-platform-api-key"
							credential := &cliproxyauth.Auth{
								ID: t.Name(), Provider: "claude", Attributes: map[string]string{
									"api_key": key, "base_url": "https://platform-contract.invalid",
								},
							}
							if entry == "count" {
								credential.Attributes["base_url"] = "https://api.anthropic.com"
							}
							if profile == "caller-owned" {
								credential.Attributes["fingerprint_profile"] = ""
							} else if profile == "claude-code-cli" {
								credential.Attributes["fingerprint_profile"] = profile
							} else if profile == "oauth" {
								key = "sk-ant-oat-synthetic-platform"
								credential.Attributes["api_key"] = key
								credential.Metadata = claudeOAuthTestMetadata()
							}
							incoming := http.Header{"User-Agent": {"curl/8.7.1"}}
							if suppliedPlatform {
								incoming["X-Stainless-OS"], incoming["X-Stainless-Arch"] = []string{
									"Windows",
								}, []string{
									"x64",
								}
							}
							var captured http.Header
							calls := 0
							ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
							defer cancel()
							ctx = util.WithRoundTripper(
								ctx, protectedUtlsRoundTripFunc(
									func(request *http.Request) (*http.Response, error) {
										calls++
										captured = request.Header.Clone()
										body := claudeReplayTextResponse("synthetic-platform")
										if entry == "stream" {
											body = claudeReplayTextStream("synthetic-platform")
										} else if entry == "count" {
											body = `{"input_tokens":1}`
										}
										return &http.Response{
											StatusCode: http.StatusOK,
											Header:     http.Header{"Content-Type": {"application/json"}},
											Body:       io.NopCloser(strings.NewReader(body)), Request: request,
										}, nil
									},
								),
							)
							payload := []byte(
								(`{"model":"claude-synthetic-platform-contract","max_tokens":32,` +
									`"messages":[{"role":"user","content":` +
									`"synthetic"}]}`),
							)
							original := bytes.Clone(payload)
							request := cliproxyexecutor.Request{Model: model, Payload: payload}
							opts := cliproxyexecutor.Options{
								SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude,
								Headers: incoming,
								Metadata: map[string]any{
									cliproxyexecutor.ExecutionSessionMetadataKey: t.Name(),
								},
							}
							executor := NewClaudeExecutor(cfg)
							var err error
							if entry == "stream" {
								var stream *cliproxyexecutor.StreamResult
								stream, err = executor.ExecuteStream(ctx, credential, request, opts)
								if err == nil {
									for chunk := range stream.Chunks {
										if chunk.Err != nil {
											err = chunk.Err
										}
										if chunk.Acknowledge != nil {
											chunk.Acknowledge(nil)
										}
									}
								}
							} else if entry == "count" {
								_, err = executor.CountTokens(ctx, credential, request, opts)
							} else {
								_, err = executor.Execute(ctx, credential, request, opts)
							}
							if err != nil {
								t.Fatal(err)
							}
							wantOS, wantArch := claudeRuntimePlatformForTest()
							cli := profile == "claude-code-cli" || profile == "oauth"
							if stabilization == "true" && cli {
								wantOS, wantArch = "MacOS", "arm64"
							} else if suppliedPlatform {
								wantOS, wantArch = "Windows", "x64"
							} else if stabilization == "true" {
								wantOS, wantArch = "", ""
							}
							gotOS, gotArch := helps.HeaderValueCaseInsensitive(
								captured, "X-Stainless-Os",
							), helps.HeaderValueCaseInsensitive(captured, "X-Stainless-Arch")
							if gotOS != wantOS || gotArch != wantArch || calls != 1 ||
								!bytes.Equal(payload, original) {
								t.Fatalf(
									"platform=%s/%s want=%s/%s calls=%d payload unchanged=%t",
									gotOS,
									gotArch,
									wantOS,
									wantArch,
									calls,
									bytes.Equal(payload, original),
								)
							}
							if !cli &&
								(captured.Get("User-Agent") != "curl/8.7.1" || captured.Get("X-App") != "") {
								t.Fatalf("platform configuration changed caller-owned identity: %v", captured)
							}
							if cli && captured.Get("User-Agent") != cfg.ClaudeHeaderDefaults.UserAgent {
								t.Fatalf("CLI software profile changed: %v", captured)
							}
							if entry == "count" && profile != "oauth" {
								if captured.Get("X-Api-Key") != key || captured.Get("Authorization") != "" {
									t.Fatalf("direct API-key authentication changed: %v", captured)
								}
							} else if captured.Get("Authorization") !=
								"Bearer "+
									key ||
								captured.Get("X-Api-Key") !=
									"" {
								t.Fatalf("bearer authentication changed: %v", captured)
							}
						},
					)
				}
			}
		}
	}
}

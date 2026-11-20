package executor

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// noinspection GoDfaErrorMayBeNotNil
func executeClaudeCompactionTestRequest(
	t *testing.T, executor *ClaudeExecutor, auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool,
) ([]byte, error) {
	t.Helper()
	if !stream {
		response, err := executor.Execute(t.Context(), auth, req, opts)
		return response.Payload, err
	}
	result, err := executor.ExecuteStream(t.Context(), auth, req, opts)
	if err != nil {
		return nil, err
	}
	var output []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			return nil, chunk.Err
		}
		output = append(output, chunk.Payload...)
	}
	for line := range bytes.SplitSeq(output, []byte("\n")) {
		if payload, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			if gjson.GetBytes(payload, "type").String() == "response.completed" {
				return []byte(gjson.GetBytes(payload, "response").Raw), nil
			}
		}
	}
	return nil, fmt.Errorf("compaction stream has no completed response: %s", output)
}

func TestClaudeCompactionCapsuleUsesFinalPayloadModel(t *testing.T) {
	const rewrittenModel = "claude-sonnet-4-6"
	for _, stream := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("stream=%t/override=%t", stream, override), func(t *testing.T) {
					var seen [][]byte
					server := claudeCompactionTestServer(t, &seen)
					defer server.Close()
					auth := &cliproxyauth.Auth{
						ID: "compaction-final-model", Provider: "claude",
						Attributes: map[string]string{"api_key": "key-compaction-final", "base_url": server.URL},
					}
					cfg := &config.Config{}
					wantModel := claudeCompactionTestModel
					wrongModel := rewrittenModel
					if override {
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: []config.PayloadModelRule{
									{
										Name: claudeCompactionTestModel, Protocol: "claude",
									},
								},
								Params: map[string]any{"model": rewrittenModel},
							},
						}
						wantModel, wrongModel = rewrittenModel, claudeCompactionTestModel
					}
					executor := NewClaudeExecutor(cfg)
					payload := []byte(`{"model":"` + claudeCompactionTestModel + `","input":[` +
						`{"role":"user","content":"Summarize this conversation"},{"type":"compaction_trigger"}]}`)
					req, opts := claudeCompactionTestRequest(payload)
					response, err := executeClaudeCompactionTestRequest(t, executor, auth, req, opts, stream)
					if err != nil {
						t.Fatal(err)
					}
					if len(seen) != 1 || gjson.GetBytes(seen[0], "model").String() != wantModel {
						t.Fatalf("summary model = %q, want %q", seen, wantModel)
					}
					if got := gjson.GetBytes(response, "model").String(); got != wantModel {
						t.Errorf("compaction response model = %q, want %q", got, wantModel)
					}
					capsule := gjson.GetBytes(response, "output.0.encrypted_content").String()
					scope, err := executor.claudeCompactionScope(auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					scope.Model = wantModel
					const wantSummary = "Summary of the previous turns."
					if summary, err := helps.UnsealAntigravityCompaction(
						capsule, scope,
					); err != nil || summary != wantSummary {
						t.Fatalf("final model capsule summary = %q, error = %v", summary, err)
					}
					scope.Model = wrongModel
					if _, err := helps.UnsealAntigravityCompaction(capsule, scope); err == nil {
						t.Fatal("capsule accepted the other model scope")
					}

					replay := []byte(`{"input":[{"type":"compaction","encrypted_content":"` + capsule +
						`"},{"role":"user","content":"Continue"}]}`)
					replayReq, replayOpts := claudeCompactionTestRequest(replay)
					replayReq.Model = wantModel
					if _, err := executor.Execute(t.Context(), auth, replayReq, replayOpts); err != nil {
						t.Fatalf("replay with final model: %v", err)
					}
					if len(seen) != 2 || !bytes.Contains(seen[1], []byte("Summary of the previous turns.")) {
						t.Fatalf("final-model replay request = %q", seen)
					}
				},
			)
		}
	}
}

func TestClaudeCompactionReplayRejectsFinalModelRewrite(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, trigger := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("stream=%t/trigger=%t", stream, trigger), func(t *testing.T) {
					var seen [][]byte
					server := claudeCompactionTestServer(t, &seen)
					defer server.Close()
					auth := &cliproxyauth.Auth{
						ID: "compaction-replay-model", Provider: "claude",
						Attributes: map[string]string{"api_key": "key-compaction-replay", "base_url": server.URL},
					}
					executor := NewClaudeExecutor(
						&config.Config{
							Payload: config.PayloadConfig{
								Override: []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{
											{
												Name: claudeCompactionTestModel, Protocol: "claude",
											},
										},
										Params: map[string]any{"model": "claude-sonnet-4-6"},
									},
								},
							},
						},
					)
					req, opts := claudeCompactionTestRequest(nil)
					scope, err := executor.claudeCompactionScope(auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					capsule, err := helps.SealAntigravityCompaction("Original model summary", scope)
					if err != nil {
						t.Fatal(err)
					}
					tail := ""
					if trigger {
						tail = `,{"type":"compaction_trigger"}`
					}
					payload := []byte(`{"input":[{"type":"compaction","encrypted_content":"` + capsule +
						`"},{"role":"user","content":"Continue"}` + tail + `]}`)
					req, opts = claudeCompactionTestRequest(payload)
					_, err = executeClaudeCompactionTestRequest(t, executor, auth, req, opts, stream)
					if err == nil || statusErrCode(err) != 400 || len(seen) != 0 {
						t.Fatalf(
							"cross-model replay error = %v, upstream calls = %d, want 400 before sending",
							err, len(seen),
						)
					}
				},
			)
		}
	}
}

func TestClaudeCompactionScopeUsesSharedMetadataLock(t *testing.T) {
	for _, token := range []any{" refresh-compaction-key ", []byte(" refresh-compaction-key ")} {
		t.Run(
			fmt.Sprintf("%T", token), func(t *testing.T) {
				auth := &cliproxyauth.Auth{
					ID: "compaction-shared-metadata", Metadata: map[string]any{
						"access_token": "sk-ant-oat-compaction-test", "refresh_token": token,
					},
				}
				executor := NewClaudeExecutor(&config.Config{})
				req, opts := claudeCompactionTestRequest(nil)
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i := range 64 {
					wg.Go(
						func() {
							<-start
							if i%2 == 0 {
								claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", "compaction-account")
								return
							}
							scope, err := executor.claudeCompactionScope(auth, req, opts)
							if err != nil || string(scope.KeyMaterial) != "refresh-compaction-key" {
								t.Errorf("compaction metadata key was not preserved: %v", err)
							}
						},
					)
				}
				close(start)
				wg.Wait()
			},
		)
	}
}

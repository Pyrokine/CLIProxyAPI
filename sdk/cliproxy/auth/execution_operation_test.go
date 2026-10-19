package auth

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type operationTestExecutor struct {
	schedulerTestExecutor
	countTokens bool
}

func (e *operationTestExecutor) RequestToFormat(_ cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	if opts.CountTokens {
		return sdktranslator.FormatClaude
	}
	return sdktranslator.FormatOpenAI
}

func (e *operationTestExecutor) Execute(_ context.Context, _ *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.countTokens = opts.CountTokens
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *operationTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *operationTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_, _ = e.Execute(ctx, auth, req, opts)
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestRequestToFormatMatchesExecutionTranslation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		opts     cliproxyexecutor.Options
		want     sdktranslator.Format
	}{
		{name: "meta", provider: "meta", want: sdktranslator.FormatCodex},
		{name: "codex", provider: "codex", want: sdktranslator.FormatCodex},
		{name: "gemini-cli", provider: "gemini-cli", want: sdktranslator.FormatGeminiCLI},
		{name: "image", provider: "codex", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image")}, want: sdktranslator.FromString("openai-image")},
		{name: "video", provider: "xai", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-video")}, want: sdktranslator.FromString("openai-video")},
		{name: "speech", provider: "claude", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-speech")}, want: sdktranslator.FromString("openai-speech")},
		{name: "compact", provider: "xai", opts: cliproxyexecutor.Options{Alt: "responses/compact"}, want: sdktranslator.FormatOpenAIResponse},
		{name: "compact stream", provider: "xai", opts: cliproxyexecutor.Options{Alt: "responses/compact", Stream: true}, want: sdktranslator.FormatCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestToFormat(nil, tc.provider, nil, cliproxyexecutor.Request{}, tc.opts); got != tc.want {
				t.Fatalf("requestToFormat() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := requestToFormat(&operationTestExecutor{}, "meta", nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{Alt: "responses/compact"}); got != sdktranslator.FormatOpenAI {
		t.Fatalf("executor format override = %q, want openai", got)
	}
}

func TestManagerOwnsCountOperationAcrossInterceptor(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			executor := &operationTestExecutor{}
			manager.RegisterExecutor(executor)
			auth := &Auth{ID: "synthetic-operation-" + mode, Provider: "test", Status: StatusActive}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, "test", []*registry.ModelInfo{{ID: "test-model"}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			manager.RefreshSchedulerEntry(auth.ID)
			want := mode == "count"
			intercepts := 0
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, CountTokens: !want}
			opts.RequestAfterAuthInterceptor = func(_ context.Context, in cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
				intercepts++
				if in.Options.CountTokens != want || (in.ToFormat == sdktranslator.FormatClaude) != want {
					t.Errorf("operation=%v format=%s want count=%v", in.Options.CountTokens, in.ToFormat, want)
				}
				in.Options.CountTokens = !want
				return cliproxyexecutor.RequestAfterAuthInterceptResponse{Handled: true, Request: in.Request, Options: in.Options}
			}
			req := cliproxyexecutor.Request{Model: "test-model", Payload: []byte(`{}`)}
			var err error
			switch mode {
			case "execute":
				_, err = manager.Execute(context.Background(), []string{"test"}, req, opts)
			case "count":
				_, err = manager.ExecuteCount(context.Background(), []string{"test"}, req, opts)
			case "stream":
				var result *cliproxyexecutor.StreamResult
				result, err = manager.ExecuteStream(context.Background(), []string{"test"}, req, opts)
				if err == nil {
					for range result.Chunks {
					}
				}
			}
			if err != nil || intercepts != 1 || executor.countTokens != want {
				t.Fatalf("count=%v intercepts=%d err=%v", executor.countTokens, intercepts, err)
			}
			if opts.CountTokens == want {
				t.Fatal("manager changed caller options")
			}
		})
	}
}

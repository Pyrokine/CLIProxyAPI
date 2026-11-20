package executor

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/sjson"
)

func TestAntigravitySignatureLengthStopsEveryExecutionEntry(t *testing.T) {
	value := "cpa-gemini-carrier-v1:next:text:" + base64.RawStdEncoding.EncodeToString(
		[]byte(strings.Repeat(
			"R", 32*1024*1024+1,
		)),
	)
	body := []byte((`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"` +
		`thinking","thinking":"","signature":"`) + value + `"},{"type":"text","text":"visible"}]}]}`)
	for _, entry := range []string{"generate", "claude-aggregate", "stream", "count"} {
		t.Run(
			entry, func(t *testing.T) {
				model := "gemini-2.5-flash"
				if entry == "claude-aggregate" {
					model = "claude-sonnet-4-6"
				}
				var calls atomic.Int32
				ctx := util.WithRoundTripper(
					t.Context(), antigravityCountTokensRoundTripFunc(
						func(*http.Request) (*http.Response, error) {
							calls.Add(1)
							return nil, errors.New("offline transport reached after oversized signature")
						},
					),
				)
				executor := NewAntigravityExecutor(&config.Config{})
				auth := antigravityCountTokensAuth("https://signature-fixture.invalid", t.Name())
				req := cliproxyexecutor.Request{Model: model, Payload: body}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				var err error
				switch entry {
				case "stream":
					_, err = executor.ExecuteStream(ctx, auth, req, opts)
				case "count":
					_, err = executor.CountTokens(ctx, auth, req, opts)
				default:
					_, err = executor.Execute(ctx, auth, req, opts)
				}
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest ||
					calls.Load() != 0 {
					t.Fatalf(
						"oversized input: error=%v calls=%d, want HTTP 400 before transport",
						err,
						calls.Load(),
					)
				}
			},
		)
	}
}

func TestAntigravitySignatureLengthChecksRestoredCacheValue(t *testing.T) {
	const model = "gemini-2.5-flash"
	text := t.Name()
	ctx := t.Context()
	if !cache.CacheSignatureBestEffort(ctx, model, text, strings.Repeat("R", 32*1024*1024+1)) {
		t.Fatal("failed to seed synthetic signature cache")
	}
	t.Cleanup(
		func() {
			if err := cache.DeleteCachedSignatureRequired(ctx, model, text); err != nil {
				t.Error(err)
			}
		},
	)
	body, err := sjson.SetBytes(
		[]byte(
			(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
				`"signature":""},{"type":"text","text":` +
				`"visible"}]}]}`),
		),
		"messages.0.content.0.thinking",
		text,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"executor-validation", "checked-translation"} {
		t.Run(
			entry, func(t *testing.T) {
				var err error
				if entry == "executor-validation" {
					_, err = validateAntigravityRequestSignatures(ctx, model, sdktranslator.FormatClaude, body)
				} else {
					_, err = sdktranslator.TranslateRequestChecked(
						ctx, sdktranslator.FormatClaude, sdktranslator.FormatAntigravity, model, body, false,
					)
				}
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
					t.Fatalf("cached oversized signature: %v, want HTTP 400", err)
				}
			},
		)
	}
}

package executor

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestOpenAICompatOriginalConfigIndexRequiresConfigSource(t *testing.T) {
	for _, source := range []string{
		auth.AuthSourceConfig, auth.AuthSourceFile, auth.AuthSourceMemory, auth.AuthSourceGit, "",
	} {
		for _, home := range []bool{false, true} {
			name := source
			if name == "" {
				name = "unspecified"
			}
			if home {
				name += "/Home"
			}
			t.Run(
				name, func(t *testing.T) {
					cfg := &config.Config{
						Home: config.HomeConfig{Enabled: home}, OpenAICompatibility: []config.OpenAICompatibility{
							{Name: "matching-name", BaseURL: "https://first.invalid/v1"},
							{Name: "indexed-name", BaseURL: "https://second.invalid/v1", SupportPromptCacheKey: true},
						},
					}
					credential := &auth.Auth{
						Provider: "matching-name", Attributes: map[string]string{
							"source": source, "config_index": "1", "compat_name": "matching-name",
							"provider_key": "matching-name",
						},
					}
					selected := NewOpenAICompatExecutor("matching-name", cfg).resolveCompatConfig(
						credential, core.Request{Model: "synthetic-model"},
					)
					if home {
						if selected == nil || selected.Name != "matching-name" || selected.BaseURL != "" ||
							selected.SupportPromptCacheKey {
							t.Fatalf("Home used global route options: %+v", selected)
						}
						return
					}
					index := 0
					if source == auth.AuthSourceConfig {
						index = 1
					}
					if selected != &cfg.OpenAICompatibility[index] {
						t.Fatalf("source=%q selected=%+v want=%+v", source, selected, cfg.OpenAICompatibility[index])
					}
				},
			)
		}
	}
}

func TestOpenAICompatOriginalTPMRetryAfterPreservesResponse(t *testing.T) {
	for _, sample := range []struct {
		name, body, retryAfter string
		status                 int
		delay                  time.Duration
		present                bool
	}{
		{
			name: "TPM code", status: http.StatusTooManyRequests,
			body: `{"error":{"code":"TPMRateLimitExceeded","message":"limited"}}`, present: true, delay: time.Minute,
		},
		{
			name: "TPM message", status: http.StatusTooManyRequests,
			body: `{"error":{"message":"Tokens per minute limit exceeded"}}`, present: true, delay: time.Minute,
		},
		{
			name: "TPM invalid header", status: http.StatusTooManyRequests,
			body: `{"error":{"code":"TPMRateLimitExceeded"}}`, retryAfter: "invalid", present: true, delay: time.Minute,
		},
		{
			name: "TPM header", status: http.StatusTooManyRequests, body: `{"error":{"code":"TPMRateLimitExceeded"}}`,
			retryAfter: "7", present: true, delay: 7 * time.Second,
		},
		{
			name: "ordinary rate limit", status: http.StatusTooManyRequests,
			body: `{"error":{"message":"requests per minute limit exceeded"}}`,
		},
		{
			name: "503 header", status: http.StatusServiceUnavailable, body: `{"error":{"message":"unavailable"}}`,
			retryAfter: "30", present: true, delay: 30 * time.Second,
		},
		{
			name: "original TPM code", status: http.StatusTooManyRequests,
			body: `{"error":{"code":"ModelAccountTpmRateLimitExceeded","message":"TPM limit exceeded"}}`, present: true,
			delay: time.Minute,
		},
		{
			name: "original TPM message", status: http.StatusTooManyRequests,
			body: `{"error":{"message":"TPM (Tokens Per Minute) limit of this model is exceeded"}}`, present: true,
			delay: time.Minute,
		},
		{
			name: "original header precedence", status: http.StatusTooManyRequests,
			body: `{"error":{"code":"ModelAccountTpmRateLimitExceeded"}}`, retryAfter: "5", present: true,
			delay: 5 * time.Second,
		},
		{name: "original generic limit", status: http.StatusTooManyRequests, body: `{"error":{"code":"rate_limit"}}`},
		{
			name: "original propagated header", status: http.StatusTooManyRequests,
			body: `{"error":{"code":"rate_limit","message":"try later"}}`, retryAfter: "7", present: true,
			delay: 7 * time.Second,
		},
	} {
		for _, mode := range []string{"execute", "stream"} {
			t.Run(
				sample.name+"/"+mode, func(t *testing.T) {
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), claudeContractTransport(
							func(request *http.Request) (*http.Response, error) {
								calls++
								headers := http.Header{"Content-Type": {"application/json"}}
								if sample.retryAfter != "" {
									headers.Set("Retry-After", sample.retryAfter)
								}
								return &http.Response{
									StatusCode: sample.status, Header: headers,
									Body: io.NopCloser(strings.NewReader(sample.body)), Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						Attributes: map[string]string{
							"base_url": "http://127.0.0.1:1/v1", "api_key": "test",
						},
					}
					request := core.Request{
						Model:   "compatible-model",
						Payload: []byte(`{"model":"compatible-model","messages":[{"role":"user","content":"hi"}]}`),
					}
					opts := core.Options{SourceFormat: translator.FormatOpenAI, Stream: mode == "stream"}
					execute := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
					var err error
					if mode == "execute" {
						_, err = execute.Execute(ctx, credential, request, opts)
					} else {
						_, err = execute.ExecuteStream(ctx, credential, request, opts)
					}
					var upstream interface {
						error
						StatusCode() int
						RetryAfter() *time.Duration
					}
					if !errors.As(
						err, &upstream,
					) || upstream.StatusCode() != sample.status || upstream.Error() != sample.body || calls != 1 {
						t.Fatalf("response status/body changed: error=%v calls=%d", err, calls)
					}
					delay := upstream.RetryAfter()
					if (delay != nil) != sample.present || delay != nil && *delay != sample.delay {
						t.Fatalf("retry after=%v want=%s present=%t", delay, sample.delay, sample.present)
					}
				},
			)
		}
	}
}

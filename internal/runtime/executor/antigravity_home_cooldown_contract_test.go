package executor

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAntigravityHomeShortCooldownOriginalInputsAtManagerEntries(t *testing.T) {
	failure := errors.New("synthetic Home cooldown unavailable")
	for _, sample := range []struct {
		name      string
		status    int
		provider  int
		calls     int32
		getErr    error
		setErr    error
		delErr    error
		rejectSet bool
		globalOff bool
		authOff   bool
		credits   bool
		write     bool
		expired   bool
		malformed bool
		active    bool
		delay     string
		wait      time.Duration
	}{
		{name: "active", active: true, status: 429},
		{name: "expired", expired: true, status: 200, calls: 1},
		{name: "get-error", getErr: failure, status: 503},
		{name: "invalid-original-value", malformed: true, status: 503},
		{name: "delete-error", expired: true, delErr: failure, status: 503},
		{name: "write", write: true, provider: 429, status: 429, calls: 1},
		{name: "write-error", write: true, provider: 429, setErr: failure, status: 503, calls: 1},
		{name: "write-not-applied", write: true, provider: 429, rejectSet: true, status: 503, calls: 1},
		{name: "bounded-retry-edge", delay: "0.5s", provider: 429, status: 429, calls: 1},
		{
			name: "above-bounded-retry", delay: "0.501s", wait: 501 * time.Millisecond, write: true, provider: 429,
			status: 429, calls: 1,
		},
		{
			name: "below-original-instant-edge", delay: "2.999s", wait: 2999 * time.Millisecond, write: true,
			provider: 429, status: 429, calls: 1,
		},
		{
			name: "original-instant-edge", delay: "3s", wait: 3 * time.Second, write: true, provider: 429, status: 429,
			calls: 1,
		},
		{
			name: "below-original-quota-edge", delay: "299.999s", wait: 5*time.Minute - time.Millisecond, write: true,
			provider: 429, status: 429, calls: 1,
		},
		{name: "original-quota-edge", delay: "300s", provider: 429, status: 429, calls: 1},
		{name: "above-original-quota-edge", delay: "300.001s", provider: 429, status: 429, calls: 1},
		{name: "global-disabled", malformed: true, globalOff: true, status: 200, calls: 1},
		{name: "auth-disabled", malformed: true, authOff: true, status: 200, calls: 1},
		{name: "credits-bypass", malformed: true, credits: true, status: 200, calls: 1},
	} {
		for _, entry := range []string{"execute", "count", "stream"} {
			t.Run(
				sample.name+"/"+entry, func(t *testing.T) {
					const model = "claude-sonnet-4-5"
					cfg := &config.Config{
						Home: config.HomeConfig{Enabled: true}, DisableCooling: sample.globalOff,
						QuotaExceeded: config.QuotaExceeded{AntigravityCredits: sample.credits},
					}
					credential := antigravityCountTokensAuth("https://cooldown.synthetic.invalid", t.Name())
					if sample.authOff {
						credential.Metadata["disable_cooling"] = true
					}
					key := "cpa:antigravity:short-cooldown:" + credential.ID + ":" + home.HashKeyPart(model)
					client := &antigravityCreditsHomeClient{
						values: make(map[string][]byte), getErr: sample.getErr,
						setErr: sample.setErr, delErr: sample.delErr, rejectSet: sample.rejectSet,
					}
					switch {
					case sample.active:
						client.values[key] = []byte(
							strconv.FormatInt(time.Now().Add(30*time.Second).UnixNano(), 10),
						)
					case sample.expired:
						client.values[key] = []byte("1")
					case sample.malformed:
						client.values[key] = []byte("9999999999999999999")
					}
					var err error
					client.payload, err = json.Marshal(
						map[string]any{
							"auth": credential, "model": model, "request_retry": 0,
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					manager := cliproxyauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					manager.RegisterExecutor(NewAntigravityExecutor(cfg))
					manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return client })
					manager.PublishHomeDispatch(client, executionregistry.New(), 1)
					var calls atomic.Int32
					ctx := t.Context()
					if sample.credits {
						ctx = cliproxyauth.WithAntigravityCredits(ctx)
					}
					ctx = util.WithRoundTripper(
						ctx, antigravityCountTokensRoundTripFunc(
							func(req *http.Request) (*http.Response, error) {
								calls.Add(1)
								status := sample.provider
								if status == 0 {
									status = http.StatusOK
								}
								body := `{"response":{"candidates":[{"content":{"role":"model","parts":` +
									`[{"text":"ok"}]},"finishReason":` +
									`"STOP"}]}}`
								if entry == "count" {
									body = `{"totalTokens":7}`
								}
								if status == 429 {
									delay := sample.delay
									if delay == "" {
										delay = "30s"
									}
									body = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type"` +
										`:"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEE` +
										`DED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay"` +
										`:"` + delay + `"}]}}`
								}
								return &http.Response{
									StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
									Request: req, Body: io.NopCloser(strings.NewReader(body)),
								}, nil
							},
						),
					)
					request := core.Request{
						Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
					}
					options := core.Options{SourceFormat: translator.FormatAntigravity}
					started := time.Now()
					switch entry {
					case "execute":
						_, err = manager.Execute(ctx, []string{"antigravity"}, request, options)
					case "count":
						_, err = manager.ExecuteCount(ctx, []string{"antigravity"}, request, options)
					case "stream":
						var stream *core.StreamResult
						stream, err = manager.ExecuteStream(ctx, []string{"antigravity"}, request, options)
						if err == nil {
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					}
					status := http.StatusOK
					if err != nil {
						status = testExecutorStatusCode(err)
					}
					if entry == "count" {
						wantStatus := sample.provider
						if wantStatus == 0 {
							wantStatus = http.StatusOK
						}
						if status != wantStatus || calls.Load() != 1 || client.cooldownReads != 0 ||
							client.deletes != 0 || client.ttls[key] != 0 {
							t.Fatalf(
								"count-only request used generation cooldown: status=%d error=%v "+
									"requests=%d reads=%d deletes=%d "+
									"TTL=%s",
								status,
								err,
								calls.Load(),
								client.cooldownReads,
								client.deletes,
								client.ttls[key],
							)
						}
						return
					}
					if status != sample.status || calls.Load() != sample.calls {
						t.Fatalf(
							"status=%d error=%v requests=%d, want %d/%d",
							status,
							err,
							calls.Load(),
							sample.status,
							sample.calls,
						)
					}
					if (sample.getErr != nil || sample.setErr != nil || sample.delErr != nil) && !errors.Is(
						err, failure,
					) {
						t.Fatalf("Home cooldown error lost original cause: %v", err)
					}
					if sample.status == 503 {
						scoped, ok := errors.AsType[interface {
							error
							IsRequestScoped() bool
						}](err)
						if !ok || !scoped.IsRequestScoped() {
							t.Fatalf("Home KV failure lost request ownership: %v", err)
						}
					}
					if sample.expired && sample.delErr == nil && client.deletes != 1 {
						t.Fatalf("expired Home cooldown deletes=%d, want 1", client.deletes)
					}
					if sample.provider == 429 && !sample.write && client.ttls[key] != 0 {
						t.Fatalf(
							"delay %q wrote a generation short cooldown: TTL=%s",
							sample.delay,
							client.ttls[key],
						)
					}
					if sample.write && sample.setErr == nil && !sample.rejectSet {
						wait := sample.wait
						if wait == 0 {
							wait = 30 * time.Second
						}
						deadline, parseErr := strconv.ParseInt(string(client.values[key]), 10, 64)
						if parseErr != nil || deadline < started.Add(wait).UnixNano() ||
							deadline > time.Now().Add(wait).UnixNano() ||
							client.ttls[key] != wait+5*time.Second {
							t.Fatalf(
								"Home cooldown value=%s wait=%s TTL=%s error=%v", client.values[key], wait,
								client.ttls[key], parseErr,
							)
						}
					}
				},
			)
		}
	}
}

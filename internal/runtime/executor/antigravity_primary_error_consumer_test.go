package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// primaryErrorHome keeps the actual scoped cache consumer and CAS boundary in use.
type primaryErrorHome struct {
	t        *testing.T
	mu       sync.Mutex
	value    []byte
	readKey  string
	casCalls int
	casErr   error
	advance  bool
}

func (h *primaryErrorHome) Enabled() bool     { return true }
func (h *primaryErrorHome) HeartbeatOK() bool { return true }
func (h *primaryErrorHome) GetRefreshAuth(context.Context, string, string) ([]byte, error) {
	h.t.Error("unexpected Home credential refresh")
	return nil, errors.New("unexpected refresh")
}
func (h *primaryErrorHome) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	h.t.Error("unexpected Home credential dispatch")
	return nil, errors.New("unexpected dispatch")
}
func (h *primaryErrorHome) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	if !strings.HasPrefix(key, "cpa:antigravity:reasoning-replay:") {
		return nil, false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.readKey = key
	return bytes.Clone(h.value), true, nil
}
func (h *primaryErrorHome) KVSet(context.Context, string, []byte, homekv.KVSetOptions) (bool, error) {
	h.t.Error("unexpected unguarded replay write")
	return false, errors.New("unexpected write")
}
func (h *primaryErrorHome) KVDel(context.Context, ...string) (int64, error) {
	h.t.Error("unexpected unguarded replay delete")
	return 0, errors.New("unexpected delete")
}
func (h *primaryErrorHome) KVExpire(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (h *primaryErrorHome) KVCompareAndSwap(
	_ context.Context, key string, expected []byte, exists bool, value []byte, ttl time.Duration,
) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.casCalls++
	if key != h.readKey || !exists || ttl != time.Hour {
		h.t.Error("cleanup changed the cache key, expected-exists flag or TTL")
	}
	if h.casErr != nil {
		return false, h.casErr
	}
	if !bytes.Equal(expected, h.value) {
		return false, nil
	}
	h.value = bytes.Clone(value)
	return true, nil
}

type primaryErrorResults struct {
	coreauth.NoopHook
	results []coreauth.Result
}

func (h *primaryErrorResults) OnResult(_ context.Context, result coreauth.Result) {
	h.results = append(h.results, result)
}

func TestAntigravityPrimaryErrorSurvivesReplayCleanupAtManager(t *testing.T) {
	for _, entry := range []struct {
		name, model, path string
		stream            bool
	}{
		{"generate", "gemini-2.5-flash", antigravityGeneratePath, false},
		{"claude_nonstream_route", "gemini-3-pro-preview", antigravityStreamPath, false},
		{"stream", "gemini-2.5-flash", antigravityStreamPath, true},
	} {
		for _, upstream := range []string{"http400", "wrapped400"} {
			for _, cleanup := range []string{"failed", "deleted", "new_generation"} {
				t.Run(
					entry.name+"/"+upstream+"/"+cleanup, func(t *testing.T) {
						const body = `{"error":{"code":400,"message":"invalid thoughtSignature: ` +
							`synthetic-upstream-only"}}`
						const sensitive = "synthetic-secret-session-token-payload"
						oldValue, err := json.Marshal(
							[][]byte{
								[]byte(`{"type":"thought_signature",` +
									(`"thoughtSignature":"synthetic-signature-123456789","contentIndex":0,` +
										`"partIndex":` +
										`0}`)),
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						newValue, err := json.Marshal(
							[][]byte{
								[]byte(`{"type":"thought_signature",` +
									(`"thoughtSignature":"synthetic-new-signature-123456789","contentIndex":` +
										`0,"partIndex":` +
										`0}`)),
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						home := &primaryErrorHome{
							t: t, value: bytes.Clone(oldValue), advance: cleanup == "new_generation",
						}
						if cleanup == "failed" {
							home.casErr = fmt.Errorf("CAS %s: %w", sensitive, homekv.ErrNotConnected)
						}
						hook := &primaryErrorResults{}
						manager := coreauth.NewManager(nil, nil, hook)
						t.Cleanup(
							func() {
								if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
									t.Error(err)
								}
							},
						)
						manager.SetConfig(&config.Config{})
						manager.SetHomeClientProvider(func() coreauth.HomeClient { return home })
						manager.SetRetryConfig(2, 0, 2)
						manager.RegisterExecutor(NewAntigravityExecutor(&config.Config{}))
						credential := antigravityCountTokensAuth("https://synthetic.invalid", t.Name())
						credential.Status = coreauth.StatusActive
						credential.Metadata["access_token"] = sensitive
						ctx := coreauth.WithSkipPersist(t.Context())
						if _, err := manager.Register(ctx, credential); err != nil {
							t.Fatal(err)
						}
						models := registry.GetGlobalRegistry()
						models.RegisterClient(
							credential.ID, "antigravity", []*registry.ModelInfo{
								{ID: entry.model, Object: "model", OwnedBy: "antigravity"},
							},
						)
						t.Cleanup(func() { models.UnregisterClient(credential.ID) })
						before, _ := manager.GetByID(credential.ID)
						primary := newUpstreamStatusErr(http.StatusBadRequest, []byte(body), nil)
						wrapped := fmt.Errorf("synthetic upstream wrapper: %w", primary)
						calls := 0
						transport := antigravityCountTokensRoundTripFunc(
							func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Host != "synthetic.invalid" || request.URL.Path != entry.path {
									t.Error("unexpected upstream endpoint")
								}
								if request.Body != nil {
									_, _ = io.Copy(io.Discard, request.Body)
									_ = request.Body.Close()
								}
								if home.advance {
									home.mu.Lock()
									home.value = bytes.Clone(newValue)
									home.mu.Unlock()
								}
								if upstream == "wrapped400" {
									return nil, wrapped
								}
								return &http.Response{
									StatusCode: http.StatusBadRequest,
									Header:     http.Header{"Content-Type": {"application/json"}},
									Body:       io.NopCloser(strings.NewReader(body)), Request: request,
								}, nil
							},
						)
						ctx = util.WithRoundTripper(ctx, transport)
						logger := log.StandardLogger()
						oldLevel := logger.GetLevel()
						logger.SetLevel(log.WarnLevel)
						capture := &logtest.Hook{}
						oldHooks := logger.ReplaceHooks(log.LevelHooks{})
						logger.AddHook(capture)
						t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })
						request := core.Request{
							Model: entry.model,
							Payload: []byte(
								`{"request":{"contents":[{"role":"user","parts":[{"text":"synthetic input"}]}]}}`,
							),
						}
						opts := core.Options{
							Stream: entry.stream, SourceFormat: translator.FormatAntigravity,
							Headers: http.Header{"Session-Id": {sensitive}},
						}
						if entry.stream {
							var result *core.StreamResult
							result, err = manager.ExecuteStream(ctx, []string{"antigravity"}, request, opts)
							if result != nil {
								t.Fatal("failed HTTP request returned a successful stream")
							}
						} else {
							var response core.Response
							response, err = manager.Execute(ctx, []string{"antigravity"}, request, opts)
							if len(response.Payload) != 0 {
								t.Fatal("failed HTTP request returned a successful payload")
							}
						}
						var status statusErr
						if !errors.As(err, &status) || status.code != 400 || status.msg != body ||
							!errors.Is(err, primary) || errors.Is(err, homekv.ErrNotConnected) {
							t.Errorf("primary status/body/cause replaced: %v", err)
						}
						if upstream == "wrapped400" && !errors.Is(err, wrapped) {
							t.Error("upstream wrapper was discarded")
						}
						if calls != 1 || home.casCalls != 1 || home.readKey == "" {
							t.Errorf(
								"requests=%d cleanup CAS=%d scoped read=%t",
								calls,
								home.casCalls,
								home.readKey != "",
							)
						}
						if cleanup == "failed" && !bytes.Equal(home.value, oldValue) {
							t.Error("failed CAS changed cache state")
						}
						if cleanup == "deleted" && bytes.Equal(home.value, oldValue) {
							t.Error("successful cleanup did not replace the snapshot")
						}
						if cleanup == "new_generation" && !bytes.Equal(home.value, newValue) {
							t.Error("cleanup deleted a newer generation")
						}
						if len(hook.results) != 1 || hook.results[0].Success || hook.results[0].Error == nil ||
							hook.results[0].Error.HTTPStatus != 400 || hook.results[0].Error.Message != err.Error() ||
							hook.results[0].Disposition.RetryDirective != core.RetryDirectiveNoRetry ||
							hook.results[0].Disposition.EffectScope != core.StateEffectScopeRequest {
							t.Errorf("Manager final result = %+v", hook.results)
						}
						after, _ := manager.GetByID(credential.ID)
						if after == nil || after.Status != before.Status || after.Unavailable || after.Disabled ||
							after.LastError != nil || !after.NextRetryAfter.IsZero() || after.Quota.Exceeded ||
							len(after.ModelStates) != len(before.ModelStates) ||
							after.Failed != before.Failed+1 ||
							after.Success != before.Success {
							t.Error(
								"request failure changed credential availability or lost failure accounting",
							)
						}
						warnings := 0
						for _, message := range capture.AllEntries() {
							if message.Message == "antigravity reasoning replay cache delete failed" {
								warnings++
								if message.Level != log.WarnLevel || len(message.Data) != 0 {
									t.Error("cleanup diagnostic contains untrusted fields")
								}
							}
							if strings.Contains(
								message.Message, sensitive,
							) || strings.Contains(fmt.Sprint(message.Data), sensitive) {
								t.Error("cleanup error exposed a session, token or payload")
							}
						}
						wantWarnings := 0
						if cleanup == "failed" {
							wantWarnings = 1
						}
						if warnings != wantWarnings {
							t.Errorf("cleanup diagnostics=%d, want %d", warnings, wantWarnings)
						}
					},
				)
			}
		}
	}
}

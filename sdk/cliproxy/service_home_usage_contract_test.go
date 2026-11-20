package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type homeCountUsageDispatcher struct{ calls atomic.Int32 }

func (*homeCountUsageDispatcher) HeartbeatOK() bool { return true }
func (d *homeCountUsageDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	d.calls.Add(1)
	return []byte((`{"model":"upstream-model","auth_index":"home-count-index","auth":{"id"` +
		`:"home-count-auth","provider":"codex","attributes":{"auth_kind":"oauth` +
		`"},"metadata":{"access_token":"dispatch-access-token"}}}`)), nil
}

type homeCountUsageError struct{ body string }

func (e homeCountUsageError) Error() string        { return "count rejected: " + e.body }
func (homeCountUsageError) StatusCode() int        { return http.StatusUnauthorized }
func (homeCountUsageError) DirectResponse() bool   { return true }
func (e homeCountUsageError) ResponseBody() []byte { return []byte(e.body) }

type homeCountUsageExecutor struct {
	customOwnedExecutor
	body           string
	calls, refresh atomic.Int32
}

func (e *homeCountUsageExecutor) CountTokens(
	ctx context.Context,
	auth *coreauth.Auth,
	_ executor.Request,
	_ executor.Options,
) (executor.Response, error) {
	e.calls.Add(1)
	effective := auth.Clone()
	effective.Metadata["access_token"] = "executor-internal-token"
	coreauth.NotifyAccessTokenFingerprint(ctx, effective)
	return executor.Response{}, homeCountUsageError{body: e.body}
}

func (e *homeCountUsageExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.refresh.Add(1)
	return auth.Clone(), nil
}

func TestHomeCountFailureReachesServiceUsageQueue(t *testing.T) {
	for _, display := range []bool{false, true} {
		for _, body := range []string{"", "  {\"error\":\"access token expired\",\"detail\":17}\n"} {
			t.Run(
				fmt.Sprintf("display=%t/empty=%t", display, body == ""), func(t *testing.T) {
					cfg := &config.Config{AuthDir: t.TempDir(), UsageStatisticsEnabled: display}
					cfg.Home.Enabled = true
					cfg.Plugins.Dir = t.TempDir()
					manager := coreauth.NewManager(nil, nil, nil)
					service, err := NewBuilder().WithConfig(cfg).
						WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).WithCoreAuthManager(manager).Build()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(
						func() {
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							if err := service.Shutdown(ctx); err != nil {
								t.Error(err)
							}
						},
					)
					service.usageQueue.SetEnabled(true)
					service.RegisterUsagePlugin(redisqueue.NewUsagePluginForQueue(service.usageQueue))
					dispatcher := &homeCountUsageDispatcher{}
					registry := executionregistry.New()
					t.Cleanup(func() { _ = registry.Close() })
					manager.PublishHomeDispatch(dispatcher, registry, 1)
					provider := &homeCountUsageExecutor{
						id: "codex", body: body,
					}
					manager.RegisterExecutor(provider)
					ctx := coreusage.WithManager(t.Context(), service.usageManager)
					opts := executor.Options{
						Metadata: map[string]any{
							executor.RequestedModelMetadataKey:     "client-model",
							executor.CanonicalSessionIDMetadataKey: "slot:worker",
							executor.ParentSessionIDMetadataKey:    "slot:root",
						},
					}
					_, err = manager.ExecuteCount(
						ctx, []string{"codex"}, executor.Request{Model: "upstream-model"}, opts,
					)
					if err == nil || err.Error() != (homeCountUsageError{body: body}).Error() ||
						provider.calls.Load() != 1 ||
						provider.refresh.Load() != 0 ||
						dispatcher.calls.Load() != 1 {
						t.Fatalf(
							"count error=%v attempts=%d refresh=%d dispatch=%d", err, provider.calls.Load(),
							provider.refresh.Load(), dispatcher.calls.Load(),
						)
					}
					if err := service.usageManager.StopContext(t.Context()); err != nil {
						t.Fatal(err)
					}
					items := service.usageQueue.PopOldest(2)
					if len(items) != 1 {
						t.Fatalf("count failure queue records=%d, want one", len(items))
					}
					var wire struct {
						Model, Alias     string
						AuthIndex        string `json:"auth_index"`
						Fingerprint      string `json:"access_token_sha256"`
						SessionID        string `json:"session_id"`
						ParentSessionID  string `json:"parent_session_id"`
						Failed, Generate bool
						Tokens           struct {
							InputTokens  *int64 `json:"input_tokens"`
							OutputTokens *int64 `json:"output_tokens"`
							TotalTokens  *int64 `json:"total_tokens"`
						}
						Fail struct {
							StatusCode int `json:"status_code"`
							Body       string
						}
					}
					if err := json.Unmarshal(items[0], &wire); err != nil {
						t.Fatal(err)
					}
					wantFingerprint := coreauth.AccessTokenSHA256(
						&coreauth.Auth{Metadata: map[string]any{"access_token": "executor-internal-token"}},
					)
					if wire.AuthIndex != "home-count-index" || wire.Model != "upstream-model" ||
						wire.Alias != "client-model" ||
						wire.Fingerprint != wantFingerprint ||
						!wire.Failed ||
						wire.Generate ||
						wire.Fail.StatusCode != 401 ||
						wire.Fail.Body != body {
						t.Fatalf("count failure fields changed before queue consumption: %s", items[0])
					}
					for _, value := range []*int64{
						wire.Tokens.InputTokens, wire.Tokens.OutputTokens, wire.Tokens.TotalTokens,
					} {
						if value == nil || *value != 0 {
							t.Fatalf("count failure lost explicit zero token totals: %s", items[0])
						}
					}
					if wire.SessionID != coresession.NormalizeToCanonicalUUID("slot:worker") ||
						wire.ParentSessionID != coresession.NormalizeToCanonicalUUID("slot:root") ||
						service.usageStatisticsEnabled.Load() != display {
						t.Fatalf("count session hierarchy or display switch changed: %s", items[0])
					}
				},
			)
		}
	}
}

func TestHomeUsageReportingIndependentFromDisplay(t *testing.T) {
	for _, homeEnabled := range []bool{false, true} {
		for _, display := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("home=%t/display=%t", homeEnabled, display), func(t *testing.T) {
					cfg := &config.Config{AuthDir: t.TempDir(), UsageStatisticsEnabled: display}
					cfg.Home.Enabled = homeEnabled
					cfg.Plugins.Dir = t.TempDir()
					service, err := NewBuilder().WithConfig(cfg).
						WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).
						WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).Build()
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(
						func() {
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							if err := service.Shutdown(ctx); err != nil {
								t.Error(err)
							}
						},
					)
					service.usageQueue.SetEnabled(true)
					service.RegisterUsagePlugin(redisqueue.NewUsagePluginForQueue(service.usageQueue))
					ctx := coreusage.WithManager(t.Context(), service.usageManager)
					if err := coreusage.PublishRecord(
						ctx, coreusage.Record{
							Provider: "codex", Model: "gpt-5.4", AuthIndex: "home-count-index",
							Failed:   true,
							Fail:     coreusage.Failure{StatusCode: 429, Body: "synthetic upstream rejection"},
							Generate: new(false),
						},
					); err != nil {
						t.Fatal(err)
					}
					if err := service.usageManager.StopContext(t.Context()); err != nil {
						t.Fatal(err)
					}
					items := service.usageQueue.PopOldest(10)
					want := 0
					if homeEnabled || display {
						want = 1
					}
					if len(items) != want {
						t.Errorf("report queue records=%d, want %d", len(items), want)
					}
					if len(items) == 1 {
						var wire struct {
							AuthIndex string `json:"auth_index"`
							Failed    bool   `json:"failed"`
							Generate  bool   `json:"generate"`
							Fail      struct {
								StatusCode int    `json:"status_code"`
								Body       string `json:"body"`
							} `json:"fail"`
						}
						if err := json.Unmarshal(items[0], &wire); err != nil {
							t.Fatal(err)
						}
						if wire.AuthIndex != "home-count-index" || !wire.Failed || wire.Generate ||
							wire.Fail.StatusCode != 429 ||
							wire.Fail.Body != "synthetic upstream rejection" {
							t.Errorf("external queue lost attempt fields: %s", items[0])
						}
					}
					if service.usageStatisticsEnabled.Load() != display {
						t.Error("Home reporting changed the local display switch")
					}
					if homeEnabled {
						runtime := *cfg
						config.ApplyHomeRuntimeConfig(&runtime)
						if runtime.UsageStatisticsEnabled != display {
							t.Error("Home overlay changed the local display switch")
						}
					}
				},
			)
		}
	}
}

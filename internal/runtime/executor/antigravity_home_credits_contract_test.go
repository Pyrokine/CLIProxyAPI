package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type antigravityCreditsHomeClient struct {
	home.KVClient
	mu            sync.Mutex
	values        map[string][]byte
	payload       []byte
	ttls          map[string]time.Duration
	getErr        error
	setErr        error
	delErr        error
	deletes       int
	cooldownReads int
	rejectSet     bool
	acquired      bool
	locks         int
	lockKey       string
	lockTTL       time.Duration
}

func (*antigravityCreditsHomeClient) Enabled() bool     { return true }
func (*antigravityCreditsHomeClient) HeartbeatOK() bool { return true }

func (c *antigravityCreditsHomeClient) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	return c.payload, nil
}
func (*antigravityCreditsHomeClient) GetRefreshAuth(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("unexpected Home credential refresh")
}
func (c *antigravityCreditsHomeClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.HasPrefix(key, "cpa:antigravity:short-cooldown:") {
		c.cooldownReads++
	}
	value, found := c.values[key]
	return append([]byte(nil), value...), found, c.getErr
}
func (c *antigravityCreditsHomeClient) KVSet(
	_ context.Context, key string, value []byte, opts home.KVSetOptions,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if opts.NX {
		c.locks++
		c.lockKey, c.lockTTL = key, opts.EX
		return c.acquired, c.setErr
	}
	return c.setValue(key, value, opts.EX)
}

func (c *antigravityCreditsHomeClient) KVDel(_ context.Context, keys ...string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	if c.delErr != nil {
		return 0, c.delErr
	}
	var deleted int64
	for _, key := range keys {
		if _, found := c.values[key]; found {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *antigravityCreditsHomeClient) KVCompareAndSwap(
	_ context.Context, key string, expected []byte, exists bool, value []byte, ttl time.Duration,
) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, found := c.values[key]
	if found != exists || !bytes.Equal(previous, expected) {
		return false, nil
	}
	return c.setValue(key, value, ttl)
}

func (c *antigravityCreditsHomeClient) setValue(key string, value []byte, ttl time.Duration) (bool, error) {
	if c.setErr != nil {
		return false, c.setErr
	}
	if c.rejectSet {
		return false, nil
	}
	if c.values == nil {
		c.values = make(map[string][]byte)
	}
	if c.ttls == nil {
		c.ttls = make(map[string]time.Duration)
	}
	c.values[key], c.ttls[key] = append([]byte(nil), value...), ttl
	return true, nil
}

func TestAntigravityHomeCreditsRefreshSeparatesHomesWithTheSameAuth(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			credential := &cliproxyauth.Auth{
				ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"access_token": "token"},
			}
			exec := NewAntigravityExecutor(
				&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
			)
			var calls atomic.Int32
			for _, scopeID := range []string{"first-home", "second-home"} {
				client := &antigravityCreditsHomeClient{acquired: true}
				ctx := home.WithClientScope(
					t.Context(),
					home.ClientScope{ID: scopeID, Client: client, Enabled: true},
				)
				ctx = util.WithRoundTripper(
					ctx, antigravityCountTokensRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls.Add(1)
							return &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
								Body: io.NopCloser(
									strings.NewReader(
										`{"paidTier":{"availableCredits":[{"creditType":"GOOGLE_ONE_AI","credit` +
											`Amount":"25000","minimumCreditAmountForUsage":"50"}]}}`,
									),
								),
							}, nil
						},
					),
				)
				exec.maybeRefreshAntigravityCreditsHint(ctx, credential, "token", false)
				synctest.Wait()
				if _, found := client.values["cpa:antigravity:credits-hint:"+credential.ID]; !found {
					t.Fatalf("%s lost its refresh to another Home; requests=%d", scopeID, calls.Load())
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("independent Home refresh requests=%d, want 2", calls.Load())
			}
		},
	)
}

func TestAntigravityHomeCreditsExplicitFlagKeepsManagerEntries(t *testing.T) {
	for _, entry := range []string{"execute", "stream", "count"} {
		t.Run(
			entry, func(t *testing.T) {
				const model = "claude-sonnet-4-5"
				cfg := &config.Config{
					Home:          config.HomeConfig{Enabled: true},
					QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
				}
				credential := antigravityCountTokensAuth("https://credits.synthetic.invalid", t.Name())
				credential.Metadata["disable_cooling"] = true
				client := &antigravityCreditsHomeClient{getErr: errors.New("synthetic Home KV unavailable")}
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
				ctx := util.WithRoundTripper(
					cliproxyauth.WithAntigravityCredits(t.Context()),
					antigravityCountTokensRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							calls.Add(1)
							body, err := io.ReadAll(req.Body)
							if err != nil {
								return nil, err
							}
							credits := strings.Contains(string(body), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`)
							if credits != (entry != "count") {
								t.Errorf("entry=%s credits=%t body=%s", entry, credits, body)
							}
							return &http.Response{
								StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Request: req,
								Body: io.NopCloser(
									strings.NewReader(`{"error":{"code":429,"message":"synthetic limit"}}`),
								),
							}, nil
						},
					),
				)
				request := core.Request{
					Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
				}
				options := core.Options{SourceFormat: translator.FormatAntigravity}
				switch entry {
				case "execute":
					_, err = manager.Execute(ctx, []string{"antigravity"}, request, options)
				case "stream":
					_, err = manager.ExecuteStream(ctx, []string{"antigravity"}, request, options)
				case "count":
					_, err = manager.ExecuteCount(ctx, []string{"antigravity"}, request, options)
				}
				if testExecutorStatusCode(err) != http.StatusTooManyRequests || calls.Load() != 1 {
					t.Fatalf("entry=%s result=%v provider calls=%d", entry, err, calls.Load())
				}
			},
		)
	}
}

func TestAntigravityHomeCreditsKeepsOriginalBalanceAuthority(t *testing.T) {
	failure := errors.New("synthetic Home credits unavailable")
	for _, sample := range []struct {
		name      string
		hint      string
		balance   string
		getErr    error
		available bool
		wantErr   bool
	}{
		{name: "balance", balance: `{"CreditAmount":10,"MinCreditAmount":50,"Known":true}`},
		{
			name: "hint-first", hint: `{"Known":true,"Available":false}`,
			balance: `{"CreditAmount":25000,"MinCreditAmount":50,"Known":true}`,
		},
		{name: "missing"},
		{name: "available", balance: `{"CreditAmount":25000,"MinCreditAmount":50,"Known":true}`, available: true},
		{name: "malformed", balance: `not json`, wantErr: true},
		{name: "read-error", getErr: failure, wantErr: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				id := t.Name()
				client := &antigravityCreditsHomeClient{values: make(map[string][]byte), getErr: sample.getErr}
				if sample.hint != "" {
					client.values["cpa:antigravity:credits-hint:"+id] = []byte(sample.hint)
				}
				if sample.balance != "" {
					client.values["cpa:antigravity:credits-balance:"+id] = []byte(sample.balance)
				}
				cliproxyauth.SetAntigravityCreditsHint(
					id, cliproxyauth.AntigravityCreditsHint{
						Known: true, Available: true, CreditAmount: 25000, MinCreditAmount: 50,
					},
				)
				ctx := home.WithClientScope(
					t.Context(),
					home.ClientScope{ID: t.Name(), Enabled: true, Client: client},
				)
				hint, found, err := antigravityCreditsHintRequired(ctx, id)
				if (err != nil) != sample.wantErr || (sample.getErr != nil && !errors.Is(err, failure)) {
					t.Fatalf("Home credits error=%v, want error=%t", err, sample.wantErr)
				}
				wantFound := sample.hint != "" || sample.balance != ""
				if !sample.wantErr && (hint.Available != sample.available || found != wantFound) {
					t.Fatalf(
						"Home credits=%+v found=%t, want available=%t found=%t", hint, found, sample.available,
						wantFound,
					)
				}
			},
		)
	}
}

func TestAntigravityHomeCreditsRefreshKeepsSharedThrottleAndScope(t *testing.T) {
	for _, sample := range []struct {
		name         string
		acquired     bool
		setErr       error
		local        bool
		balance      string
		refreshKnown bool
	}{
		{name: "shared-lock-denied"},
		{name: "shared-lock-error", setErr: errors.New("synthetic Home lock unavailable")},
		{name: "shared-lock-acquired", acquired: true},
		{name: "home-ignores-local-hint", acquired: true, local: true},
		{name: "known-home-balance", balance: `{"CreditAmount":10,"MinCreditAmount":50,"Known":true}`},
		{name: "malformed-home-balance", balance: `not json`},
		{name: "post-refresh-bypasses-warm-lock", refreshKnown: true},
		{
			name: "post-refresh-updates-known-balance", refreshKnown: true,
			balance: `{"CreditAmount":10,"MinCreditAmount":50,"Known":true}`,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						auth := &cliproxyauth.Auth{
							ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"access_token": "token"},
						}
						client := &antigravityCreditsHomeClient{
							acquired: sample.acquired,
							setErr:   sample.setErr,
						}
						if sample.balance != "" {
							client.values = map[string][]byte{
								"cpa:antigravity:credits-balance:" + auth.ID: []byte(sample.balance),
							}
						}
						if sample.local {
							cliproxyauth.SetAntigravityCreditsHint(
								auth.ID, cliproxyauth.AntigravityCreditsHint{Known: true, Available: false},
							)
						}
						var calls atomic.Int32
						ctx := home.WithClientScope(
							t.Context(), home.ClientScope{ID: t.Name(), Enabled: true, Client: client},
						)
						ctx = util.WithRoundTripper(
							ctx, antigravityCountTokensRoundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									calls.Add(1)
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
										Body: io.NopCloser(
											strings.NewReader(
												(`{"paidTier":{"id":"tier-1","availableCredits":` +
													`[{"creditType":` +
													`"GOOGLE_O`) +
													(`NE_AI","creditAmount":"25000",` +
														`"minimumCreditAmountForUsage":` +
														`"50"}]}}`),
											),
										),
									}, nil
								},
							),
						)
						exec := NewAntigravityExecutor(
							&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
						)
						exec.maybeRefreshAntigravityCreditsHint(ctx, auth, "token", sample.refreshKnown)
						synctest.Wait()
						wantLocks := 1
						if sample.balance != "" || sample.refreshKnown {
							wantLocks = 0
						}
						if client.locks != wantLocks || (wantLocks > 0 &&
							(client.lockKey != "cpa:antigravity:credits-refresh-lock:"+auth.ID ||
								client.lockTTL != antigravityCreditsHintRefreshInterval)) {
							t.Fatalf(
								"Home lock count=%d key=%q TTL=%s",
								client.locks,
								client.lockKey,
								client.lockTTL,
							)
						}
						if !sample.acquired && !sample.refreshKnown {
							if calls.Load() != 0 {
								t.Fatalf("Home lock failure sent %d refresh requests", calls.Load())
							}
							return
						}
						if calls.Load() != 1 {
							t.Fatalf("refresh requests=%d, want 1", calls.Load())
						}
						for _, prefix := range []string{
							"cpa:antigravity:credits-hint:", "cpa:antigravity:credits-balance:",
						} {
							key := prefix + auth.ID
							var hint cliproxyauth.AntigravityCreditsHint
							if err := json.Unmarshal(client.values[key], &hint); err != nil || !hint.Known ||
								hint.CreditAmount !=
									25000 ||
								hint.MinCreditAmount !=
									50 ||
								client.ttls[key] !=
									30*
										time.Minute {
								t.Fatalf(
									"Home credits key=%q value=%s TTL=%s error=%v", key, client.values[key],
									client.ttls[key], err,
								)
							}
						}
						local, found := cliproxyauth.GetAntigravityCreditsHint(auth.ID)
						if found != sample.local || (found && local.Available) {
							t.Fatalf("Home refresh changed local observation: %+v found=%t", local, found)
						}
					},
				)
			},
		)
	}
}

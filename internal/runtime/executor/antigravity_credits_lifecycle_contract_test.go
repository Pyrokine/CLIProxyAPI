package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAntigravityCreditsLifecycleCancellationAndReplacement(t *testing.T) {
	for _, mode := range []string{
		"cancel-and-immediate-retry", "replace-token", "replace-generation", "cancel-late-success",
	} {
		t.Run(
			mode, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						manager := cliproxyauth.NewManager(nil, nil, nil)
						manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
						credential, err := manager.Register(
							cliproxyauth.WithSkipPersist(t.Context()), &cliproxyauth.Auth{
								ID: t.Name(), Provider: "antigravity",
								Metadata: map[string]any{"access_token": "old-access"},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						exec := NewAntigravityExecutor(
							&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
						)
						release := make(chan struct{})
						var releaseOnce sync.Once
						defer releaseOnce.Do(func() { close(release) })
						var oldRequest context.Context
						oldCtx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name()})
						oldCtx, cancel := context.WithCancel(oldCtx)
						defer cancel()
						oldCtx = util.WithRoundTripper(
							oldCtx, antigravityCountTokensRoundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									oldRequest = req.Context()
									if mode == "cancel-late-success" {
										<-release
									} else {
										select {
										case <-req.Context().Done():
											return nil, req.Context().Err()
										case <-release:
										}
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
										Body: io.NopCloser(
											strings.NewReader(
												(`{"paidTier":{"availableCredits":[{"creditType":` +
													`"GOOGLE_ONE_AI",` +
													`"credit`) +
													`Amount":"10","minimumCreditAmountForUsage":"50"}]}}`,
											),
										),
									}, nil
								},
							),
						)
						exec.maybeRefreshAntigravityCreditsHint(oldCtx, credential, "old-access", true)
						synctest.Wait()
						if oldRequest == nil {
							t.Fatal("old credits query did not start")
						}
						value, _ := antigravityCreditsHintRefreshByID.Load(t.Name() + "\x00" + credential.ID)
						previous := value.(*antigravityCreditsHintRefreshState)
						if previous.mu.TryLock() {
							previous.mu.Unlock()
							t.Fatal("unfinished credits query released its completion barrier")
						}
						canceled := strings.HasPrefix(mode, "cancel-")
						if canceled {
							cancel()
							synctest.Wait()
						}
						healthyCalls := 0
						newCtx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name()})
						newCtx = util.WithRoundTripper(
							newCtx, antigravityCountTokensRoundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									healthyCalls++
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
										Body: io.NopCloser(
											strings.NewReader(
												(`{"paidTier":{"availableCredits":[{"creditType":` +
													`"GOOGLE_ONE_AI",` +
													`"credit`) +
													`Amount":"900","minimumCreditAmountForUsage":"50"}]}}`,
											),
										),
									}, nil
								},
							),
						)
						current := credential.Clone()
						token := "old-access"
						if !canceled {
							if mode == "replace-token" {
								token = "replacement-access"
								current.Metadata["access_token"] = token
							} else {
								current.Metadata["refresh_token"] = "replacement-refresh"
							}
							current, err = manager.Update(cliproxyauth.WithSkipPersist(newCtx), current)
							if err != nil {
								t.Fatal(err)
							}
						}
						exec.maybeRefreshAntigravityCreditsHint(newCtx, current, token, true)
						synctest.Wait()
						if !errors.Is(oldRequest.Err(), context.Canceled) {
							t.Errorf("old credits query remains active: %v", oldRequest.Err())
						}
						if newCtx.Err() != nil || (!canceled && oldCtx.Err() != nil) {
							t.Errorf(
								"task replacement canceled a parent: old=%v new=%v",
								oldCtx.Err(),
								newCtx.Err(),
							)
						}
						releaseOnce.Do(func() { close(release) })
						synctest.Wait()
						if !previous.mu.TryLock() {
							t.Fatal("finished credits query retained its completion barrier")
						}
						previous.mu.Unlock()
						exec.maybeRefreshAntigravityCreditsHint(newCtx, current, token, true)
						synctest.Wait()
						if healthyCalls != 1 {
							t.Errorf(
								"immediate healthy queries=%d, want 1 with completed-task throttle",
								healthyCalls,
							)
						}
						if hint, found := cliproxyauth.GetAntigravityCreditsHint(credential.ID); !found ||
							hint.CreditAmount != 900 ||
							!hint.Available {
							t.Errorf(
								"current credits observation=%+v found=%t, want available 900",
								hint,
								found,
							)
						}
					},
				)
			},
		)
	}
}

func TestIssue6199AntigravityOptionalCreditsRefreshIsDeduplicatedWithoutDeadline(t *testing.T) {
	for _, sample := range []struct {
		name     string
		cfg      *config.Config
		deadline bool
	}{
		{name: "nil config"},
		{name: "paid retry disabled", cfg: &config.Config{}},
		{
			name: "paid retry enabled",
			cfg:  &config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
		},
		{name: "caller deadline", cfg: &config.Config{}, deadline: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						exec := NewAntigravityExecutor(sample.cfg)
						auth := &cliproxyauth.Auth{
							ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{
								"access_token": "old-access", "refresh_token": t.Name() + "-refresh",
								"project_id": "project",
							},
						}
						var query context.Context
						queries := 0
						ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name()})
						ctx = util.WithRoundTripper(
							ctx, antigravityCountTokensRoundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									switch req.URL.String() {
									case "https://oauth2.googleapis.com/token":
										return &http.Response{
											StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
											Body: io.NopCloser(
												strings.NewReader(
													`{"access_token":"new-access","expires_in":3600}`,
												),
											),
										}, nil
									case "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
										queries++
										query = req.Context()
										<-query.Done()
										return nil, query.Err()
									default:
										t.Errorf("unexpected intercepted request: %s", req.URL)
										return nil, errors.New("unexpected synthetic path")
									}
								},
							),
						)
						ctx, cancel := context.WithCancel(ctx)
						defer cancel()
						if sample.deadline {
							var cancelDeadline context.CancelFunc
							ctx, cancelDeadline = context.WithTimeout(ctx, 7*time.Second)
							defer cancelDeadline()
						}
						for range 3 {
							if updated, err := exec.Refresh(
								ctx, auth.Clone(),
							); err != nil || updated == nil || metaStringValue(
								updated.Metadata, "access_token",
							) != "new-access" {
								t.Fatalf(
									"token refresh waited for or lost optional credits: auth=%v error=%v",
									updated,
									err,
								)
							}
							synctest.Wait()
						}
						if query == nil || queries != 1 {
							t.Fatalf("optional credits calls=%d, want one across three refreshes", queries)
						}
						deadline, bounded := query.Deadline()
						parentDeadline, parentBounded := ctx.Deadline()
						if bounded != parentBounded || (bounded && !deadline.Equal(parentDeadline)) {
							t.Fatalf(
								"optional credits changed caller deadline: query=%v/%t caller=%v/%t",
								deadline,
								bounded,
								parentDeadline,
								parentBounded,
							)
						}
						timer := time.NewTimer(6 * time.Second)
						defer timer.Stop()
						<-timer.C
						synctest.Wait()
						if query.Err() != nil {
							t.Fatalf("optional credits stopped before its caller: %v", query.Err())
						}
						cancel()
						synctest.Wait()
						if !errors.Is(query.Err(), context.Canceled) {
							t.Fatalf("optional credits ignored caller cancellation: %v", query.Err())
						}
					},
				)
			},
		)
	}
}

func TestAntigravityCreditsLifecycleAtRefreshEntries(t *testing.T) {
	for _, postRefresh := range []bool{false, true} {
		name := "warm-independent-budget"
		if postRefresh {
			name = "post-refresh-follows-lifecycle"
		}
		t.Run(
			name, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						exec := NewAntigravityExecutor(
							&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
						)
						auth := &cliproxyauth.Auth{
							ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{
								"access_token": "access", "refresh_token": t.Name(), "project_id": "project",
								"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
							},
						}
						var query context.Context
						queries := 0
						ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name()})
						ctx = util.WithRoundTripper(
							ctx, antigravityCountTokensRoundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									if req.URL.Path == "/token" {
										return &http.Response{
											StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
											Body: io.NopCloser(
												strings.NewReader(
													`{"access_token":"access","expires_in":3600}`,
												),
											),
										}, nil
									}
									if req.URL.Path != antigravityLoadCodeAssistPath {
										t.Errorf("unexpected intercepted request: %s", req.URL)
										return nil, errors.New("unexpected synthetic path")
									}
									queries++
									query = req.Context()
									<-query.Done()
									return nil, query.Err()
								},
							),
						)
						ctx, cancel := context.WithCancel(ctx)
						defer cancel()
						if postRefresh {
							for range 3 {
								if _, err := exec.Refresh(ctx, auth); err != nil {
									t.Fatal(err)
								}
							}
						} else if _, _, err := exec.ensureAccessToken(ctx, auth); err != nil {
							t.Fatal(err)
						}
						synctest.Wait()
						if query == nil || queries != 1 {
							t.Fatalf(
								"credits queries=%d, want one query independent of repeated token refreshes",
								queries,
							)
						}
						deadline, bounded := query.Deadline()
						if postRefresh {
							if bounded {
								t.Fatalf("post-refresh credits added a deadline: %v", deadline)
							}
						} else if !bounded || time.Until(deadline) != antigravityCreditsHintRefreshTimeout {
							t.Fatalf(
								"credits deadline=%v bounded=%t, want independent five-second "+
									"budget", deadline, bounded,
							)
						}
						cancel()
						synctest.Wait()
						if postRefresh {
							if !errors.Is(query.Err(), context.Canceled) {
								t.Fatalf("post-refresh query ignored lifecycle cancellation: %v", query.Err())
							}
						} else {
							if query.Err() != nil {
								t.Fatalf("warm query lost its independent budget: %v", query.Err())
							}
							timer := time.NewTimer(time.Until(deadline))
							defer timer.Stop()
							<-timer.C
							synctest.Wait()
							if !errors.Is(query.Err(), context.DeadlineExceeded) {
								t.Fatalf("warm query outlived its budget: %v", query.Err())
							}
						}
					},
				)
			},
		)
	}
}

func TestAntigravityCreditsLifecycleRejectsOlderCompletedGeneration(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
			old, err := manager.Register(
				cliproxyauth.WithSkipPersist(t.Context()), &cliproxyauth.Auth{
					ID: t.Name(), Provider: "antigravity", Metadata: map[string]any{"access_token": "old-access"},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			replacement := old.Clone()
			replacement.Metadata["access_token"] = "replacement-access"
			current, err := manager.Update(cliproxyauth.WithSkipPersist(t.Context()), replacement)
			if err != nil {
				t.Fatal(err)
			}
			exec := NewAntigravityExecutor(
				&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}},
			)
			currentCalls, staleCalls := 0, 0
			ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name()})
			currentCtx := util.WithRoundTripper(
				ctx, antigravityCountTokensRoundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						currentCalls++
						return &http.Response{
							StatusCode: 200, Header: make(http.Header), Request: req,
							Body: io.NopCloser(
								strings.NewReader(
									`{"paidTier":{"availableCredits":[{"creditType":"GOOGLE_ONE_AI","credit` +
										`Amount":"900","minimumCreditAmountForUsage":"50"}]}}`,
								),
							),
						}, nil
					},
				),
			)
			exec.maybeRefreshAntigravityCreditsHint(currentCtx, current, "replacement-access", true)
			synctest.Wait()
			if currentCalls != 1 {
				t.Fatal("current generation did not query its balance")
			}
			timer := time.NewTimer(antigravityCreditsHintRefreshInterval + time.Nanosecond)
			defer timer.Stop()
			<-timer.C
			staleCtx := util.WithRoundTripper(
				ctx, antigravityCountTokensRoundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						staleCalls++
						return &http.Response{
							StatusCode: 200, Header: make(http.Header), Request: req,
							Body: io.NopCloser(
								strings.NewReader(
									`{"paidTier":{"availableCredits":[{"creditType":"GOOGLE_ONE_AI","credit` +
										`Amount":"10","minimumCreditAmountForUsage":"50"}]}}`,
								),
							),
						}, nil
					},
				),
			)
			exec.maybeRefreshAntigravityCreditsHint(staleCtx, old, "old-access", true)
			synctest.Wait()
			if staleCalls != 0 {
				t.Errorf("old generation queried credits after throttle elapsed: %d", staleCalls)
			}
			if hint, found := cliproxyauth.GetAntigravityCreditsHint(old.ID); !found ||
				hint.CreditAmount != 900 {
				t.Fatalf("old generation changed current balance: %+v found=%t", hint, found)
			}
		},
	)
}

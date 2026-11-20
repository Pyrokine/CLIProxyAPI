package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type originalOAuthAliasExecutor struct {
	auditLegacyVersionExecutor
	mu    sync.Mutex
	calls []struct{ authID, model, alias string }
}

func (e *originalOAuthAliasExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	req core.Request,
	_ core.Options,
) (core.Response, error) {
	e.mu.Lock()
	e.calls = append(
		e.calls,
		struct{ authID, model, alias string }{auth.ID, req.Model, usage.RequestedModelAliasFromContext(ctx)},
	)
	e.mu.Unlock()
	return core.Response{Payload: []byte(req.Model)}, nil
}

func (e *originalOAuthAliasExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

// noinspection GoDfaErrorMayBeNotNil
func (e *originalOAuthAliasExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	response, err := e.Execute(ctx, auth, req, opts)
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Payload: response.Payload, Err: err}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, nil
}

func originalOAuthAliasManager(
	t *testing.T,
	auth *Auth,
	aliases []config.OAuthModelAlias,
	models []string,
	store CooldownStateStore,
) (*Manager, *Auth, *originalOAuthAliasExecutor) {
	t.Helper()
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{auth.Provider: aliases})
	if store != nil {
		manager.SetCooldownStateStore(store)
	}
	capture := &originalOAuthAliasExecutor{id: auth.Provider}
	manager.RegisterExecutor(capture)
	registered, err := manager.Register(WithSkipPersist(t.Context()), auth)
	if err != nil {
		t.Fatal(err)
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registerProjectionTestClient(t, registered.ID, registered.Provider, infos...)
	manager.RefreshSchedulerEntry(registered.ID)
	return manager, registered, capture
}

func originalOAuthAliasQuotaResult(manager *Manager, authID, model string, retry time.Duration) Result {
	auth, _ := manager.GetByID(authID)
	result := executionResult(
		auth, auth.Provider, model,
		&Error{Code: "rate_limit_exceeded", Message: "429 rate limit", HTTPStatus: http.StatusTooManyRequests},
		core.DispatchPhaseResponseStarted,
	)
	result.RetryAfter = &retry
	return result
}

func originalOAuthAliasRecords(store *recordingCooldownStateStore) []CooldownStateRecord {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneCooldownStateRecords(store.records)
}

func TestOriginalNoForkAliasRetainsCooldownAcrossRegistration(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	for _, test := range []struct {
		id   string
		fork bool
	}{
		{"nofork-auth-1", false}, {"nofork-persist-auth", false},
		{"fork-true-auth", true}, {"nofork-multi-alias-auth", false},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				store := &recordingCooldownStateStore{}
				route, target := "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high"
				if test.fork {
					route, target = "claude-opus-4-6", "claude-opus-4-6-thinking"
				}
				routes := []string{route}
				aliases := []config.OAuthModelAlias{{Name: target, Alias: route, Fork: test.fork}}
				if test.fork {
					routes = append(routes, target)
				}
				if test.id == "nofork-multi-alias-auth" {
					routes = []string{"[ant1]gemini-3.7-flash-high", "[ant2]gemini-3.7-flash-high"}
					aliases = []config.OAuthModelAlias{
						{Name: target, Alias: routes[0]}, {Name: target, Alias: routes[1]},
					}
				}
				manager, auth, _ := originalOAuthAliasManager(
					t, &Auth{ID: test.id, Provider: "antigravity"}, aliases, routes, store,
				)
				manager.MarkResult(
					t.Context(),
					originalOAuthAliasQuotaResult(manager, auth.ID, target, 30*time.Minute),
				)
				before, _ := manager.GetByID(auth.ID)
				deadline := before.ModelStates[target].NextRetryAfter
				reg := registry.GetGlobalRegistry()
				for range 2 {
					infos := make([]*registry.ModelInfo, 0, len(routes))
					for _, route := range routes {
						infos = append(infos, &registry.ModelInfo{ID: route})
					}
					reg.RegisterClient(auth.ID, auth.Provider, infos)
					manager.RefreshSchedulerEntry(auth.ID)
					current, _ := manager.GetByID(auth.ID)
					state := current.ModelStates[target]
					if len(current.ModelStates) != 1 || state == nil || !state.Unavailable || !state.Quota.Exceeded ||
						!state.NextRetryAfter.Equal(deadline) ||
						current.Generation != before.Generation {
						t.Fatalf(
							"registration changed target authority: before=%+v current=%+v",
							before,
							current,
						)
					}
					for _, route := range routes {
						if !reg.IsModelSuspendedForClient(auth.ID, route) || !reg.IsModelQuotaExceededForClient(
							auth.ID, route,
						) {
							t.Fatalf("route %s lost target suspension", route)
						}
						if _, err := manager.SelectAuth(t.Context(), auth.Provider, route, core.Options{}); err == nil {
							t.Fatalf("cooling target selected through route %s", route)
						}
					}
				}
				found := false
				for _, record := range originalOAuthAliasRecords(store) {
					if record.AuthID == auth.ID && record.Model == target {
						found = record.NextRetryAfter.Equal(deadline)
					}
				}
				if !found {
					t.Fatal("target deadline disappeared from persistence")
				}
			},
		)
	}
}

func TestOriginalNoForkAliasesBypassCoolingCredential(t *testing.T) {
	const target = "gemini-3.7-flash-high"
	for _, test := range []struct {
		cooling, available string
		routes             []string
	}{
		{"nofork-cooling-auth", "nofork-available-auth", []string{"[ant]gemini-3.7-flash-high"}},
		{
			"multi-cooling-auth", "multi-available-auth",
			[]string{"[ant1]gemini-3.7-flash-high", "[ant2]gemini-3.7-flash-high"},
		},
	} {
		t.Run(
			test.cooling, func(t *testing.T) {
				aliases := make([]config.OAuthModelAlias, 0, len(test.routes))
				for _, route := range test.routes {
					aliases = append(aliases, config.OAuthModelAlias{Name: target, Alias: route})
				}
				manager, cooling, capture := originalOAuthAliasManager(
					t, &Auth{ID: test.cooling, Provider: "antigravity"}, aliases, test.routes, nil,
				)
				manager.MarkResult(
					t.Context(), originalOAuthAliasQuotaResult(manager, cooling.ID, target, 30*time.Minute),
				)
				if _, err := manager.SelectAuth(
					t.Context(), cooling.Provider, test.routes[0], core.Options{},
				); err == nil {
					t.Fatal("sole cooling credential remains selectable")
				}
				available, err := manager.Register(
					t.Context(),
					&Auth{ID: test.available, Provider: cooling.Provider},
				)
				if err != nil {
					t.Fatal(err)
				}
				infos := make([]*registry.ModelInfo, 0, len(test.routes))
				for _, route := range test.routes {
					infos = append(infos, &registry.ModelInfo{ID: route})
				}
				registerProjectionTestClient(t, available.ID, available.Provider, infos...)
				manager.RefreshSchedulerEntry(available.ID)
				for _, route := range test.routes {
					response, err := manager.Execute(
						t.Context(), []string{cooling.Provider}, core.Request{Model: route}, core.Options{},
					)
					if err != nil || string(response.Payload) != target {
						t.Fatalf("alias response=%s error=%v", response.Payload, err)
					}
					call := capture.calls[len(capture.calls)-1]
					if call.authID != available.ID || call.model != target || call.alias != route {
						t.Fatalf("attempt identity=%+v", call)
					}
				}
			},
		)
	}
}

func TestOriginalNoForkAliasRemovalRepointAndPerAuthAuthority(t *testing.T) {
	for _, mode := range []string{"remove", "repoint", "per-auth", "legacy route", "chained", "both chained routes"} {
		t.Run(
			mode, func(t *testing.T) {
				const provider = "antigravity"
				route, target, other := "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high", "other-model"
				id := "nofork-cleanup-auth"
				aliases := []config.OAuthModelAlias{{Name: target, Alias: route}}
				switch mode {
				case "repoint":
					id, route, target, other = "repoint-auth-1", "[alias]gemini-model-b", "gemini-model-a",
						"gemini-model-c"
				case "per-auth":
					id, target, other = "nofork-perauth-auth-1", "gemini-3.7-flash-perauth", "gemini-3.7-flash-global"
				case "legacy route":
					id = "auth-legacy-prune"
				case "chained":
					id, route, target, other = "auth1-chain-test", "route-model-b-chain", "canonical-model-a-"+
						"chain", "target-model-c-chain"
				case "both chained routes":
					id, route, target, other = "auth-chain-both-test", "route-model-b-chain-both", "canonical-mode"+
						"l-a-chain-both", "target-model-c-chain-both"
				}
				aliases[0] = config.OAuthModelAlias{Name: target, Alias: route}
				if mode == "per-auth" {
					aliases[0].Name = other
				}
				if mode == "chained" || mode == "both chained routes" {
					aliases = append(aliases, config.OAuthModelAlias{Name: other, Alias: target})
				}
				deadline := time.Now().Add(30 * time.Minute)
				auth := &Auth{
					ID: id, Provider: provider, ModelStates: map[string]*ModelState{
						target: {
							Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
							Quota: QuotaState{
								Exceeded: true, NextRecoverAt: deadline, ResetAuthority: StateResetAuthorityOperator,
							},
							ResetAuthority: StateResetAuthorityOperator,
						},
					},
				}
				if mode == "per-auth" {
					auth.ModelStates[other] = auth.ModelStates[target].Clone()
					if err := SetOAuthModelAliasesAttribute(
						auth, []config.OAuthModelAlias{{Name: target, Alias: route}},
					); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "legacy route" {
					auth.ModelStates[route] = auth.ModelStates[target].Clone()
				}
				routes := []string{route}
				if mode == "both chained routes" {
					routes = append(routes, target)
				}
				store := &recordingCooldownStateStore{}
				manager, registered, _ := originalOAuthAliasManager(t, auth, aliases, routes, store)
				manager.PersistCooldownStates(t.Context())
				reg := registry.GetGlobalRegistry()
				if !reg.IsModelSuspendedForClient(id, route) {
					t.Fatal("initial target restriction was not projected")
				}
				if mode == "remove" {
					manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{provider: nil})
					reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: other}})
				} else if mode == "repoint" {
					manager.SetOAuthModelAlias(
						map[string][]config.OAuthModelAlias{
							provider: {
								{
									Name: other, Alias: route,
								},
							},
						},
					)
				}
				manager.RefreshSchedulerEntry(id)
				current, _ := manager.GetByID(id)
				if current.Generation != registered.Generation || !reflect.DeepEqual(
					current.ModelStates, registered.ModelStates,
				) {
					t.Fatal("route publication changed independently owned model state")
				}
				if mode == "remove" || mode == "repoint" {
					requested := route
					if mode == "remove" {
						requested = other
					}
					if selected, err := manager.SelectAuth(
						t.Context(), provider, requested, core.Options{},
					); err != nil || selected.ID != id {
						t.Fatalf("clean target was blocked: selected=%+v error=%v", selected, err)
					}
					if reg.IsModelSuspendedForClient(id, requested) {
						t.Fatal("removed target restriction affected a clean route")
					}
				} else if !reg.IsModelSuspendedForClient(id, route) {
					t.Fatal("target restriction disappeared during projection")
				}
				if len(originalOAuthAliasRecords(store)) == 0 {
					t.Fatal("route publication erased independently persisted authority")
				}
			},
		)
	}
}

func TestOriginalNoForkAliasRouteOnlyAndExpiredState(t *testing.T) {
	for _, mode := range []string{"historical route", "expired target", "repoint route"} {
		t.Run(
			mode, func(t *testing.T) {
				route, target, id := "[ant]gemini-3.7-flash-mig", "gemini-3.7-flash-mig", "auth-historical-mig-test"
				stateModel, deadline := route, time.Now().Add(30*time.Minute)
				if mode == "expired target" {
					route, target, id = "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high", "nofork-expired-auth"
					stateModel, deadline = target, time.Now().Add(-10*time.Minute)
				}
				if mode == "repoint route" {
					route, target, id = "gemini-model-a", "gemini-model-c", "repoint-auth-2"
					stateModel = route
				}
				manager, auth, capture := originalOAuthAliasManager(
					t, &Auth{
						ID: id, Provider: "antigravity", ModelStates: map[string]*ModelState{
							stateModel: {
								Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
								StatusMessage: "historical cooling state on route B",
							},
						},
					}, []config.OAuthModelAlias{{Name: target, Alias: route}}, []string{route}, nil,
				)
				response, err := manager.Execute(
					t.Context(), []string{auth.Provider}, core.Request{Model: route}, core.Options{},
				)
				if err != nil || string(response.Payload) != target || len(capture.calls) != 1 {
					t.Fatalf("actual target=%s calls=%v error=%v", response.Payload, capture.calls, err)
				}
				if registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, route) {
					t.Fatal("route-only or expired state suspended the healthy actual target")
				}
			},
		)
	}
}

func TestOriginalNoForkAliasCooldownRestoreKeepsRouting(t *testing.T) {
	const route, target = "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high"
	for _, disk := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "memory", true: "disk"}[disk], func(t *testing.T) {
				var store CooldownStateStore = &recordingCooldownStateStore{}
				coolingID, availableID := "restore-auth-1", "restore-auth-2"
				coolingFile, availableFile := "", ""
				if disk {
					authDir, stateDir := filepath.Join(t.TempDir(), "auths"), t.TempDir()
					if err := os.MkdirAll(authDir, 0700); err != nil {
						t.Fatal(err)
					}
					coolingID, availableID = "disk-auth-1", "disk-auth-2"
					coolingFile, availableFile = filepath.Join(authDir, "auth1.json"), filepath.Join(
						authDir, "auth2.json",
					)
					for path, data := range map[string]string{
						coolingFile:   `{"id":"disk-auth-1","provider":"antigravity"}`,
						availableFile: `{"id":"disk-auth-2","provider":"antigravity"}`,
					} {
						if err := os.WriteFile(path, []byte(data), 0600); err != nil {
							t.Fatal(err)
						}
					}
					store = NewFileCooldownStateStoreWithAuthDir(stateDir, authDir)
				}
				manager, cooling, _ := originalOAuthAliasManager(
					t, &Auth{ID: coolingID, Provider: "antigravity", FileName: coolingFile},
					[]config.OAuthModelAlias{{Name: target, Alias: route}}, []string{route}, store,
				)
				manager.MarkResult(
					t.Context(), originalOAuthAliasQuotaResult(manager, cooling.ID, target, 30*time.Minute),
				)
				var records []CooldownStateRecord
				if memory, ok := store.(*recordingCooldownStateStore); ok {
					records = originalOAuthAliasRecords(memory)
					store = &recordingCooldownStateStore{load: records}
				} else {
					var err error
					records, err = store.Load(t.Context())
					if err != nil {
						t.Fatal(err)
					}
				}
				if len(records) == 0 {
					t.Fatal("target cooldown was not persisted")
				}
				restored, fresh, capture := originalOAuthAliasManager(
					t, &Auth{ID: coolingID, Provider: "antigravity", FileName: coolingFile},
					[]config.OAuthModelAlias{{Name: target, Alias: route}}, []string{route}, store,
				)
				available, err := restored.Register(
					WithSkipPersist(t.Context()),
					&Auth{ID: availableID, Provider: fresh.Provider, FileName: availableFile},
				)
				if err != nil {
					t.Fatal(err)
				}
				registerProjectionTestClient(
					t,
					available.ID,
					available.Provider,
					&registry.ModelInfo{ID: route},
				)
				if err := restored.RestoreCooldownStates(t.Context()); err != nil {
					t.Fatal(err)
				}
				restored.RefreshSchedulerAll()
				if !registry.GetGlobalRegistry().IsModelSuspendedForClient(fresh.ID, route) {
					t.Fatal("restored target cooldown is absent from the route")
				}
				response, err := restored.Execute(
					t.Context(), []string{fresh.Provider}, core.Request{Model: route}, core.Options{},
				)
				if err != nil || string(response.Payload) != target || len(capture.calls) != 1 ||
					capture.calls[0].authID != available.ID {
					t.Fatalf(
						"restored alias routing calls=%v response=%s error=%v",
						capture.calls,
						response.Payload,
						err,
					)
				}
			},
		)
	}
}

func TestOriginalNoForkAliasQuotaResetKeepsErrorsVisible(t *testing.T) {
	const route, target = "[ant]gemini-3.7-flash-rq-test", "gemini-3.7-flash-rq-test"
	for _, mode := range []string{"success", "canceled", "save failure"} {
		t.Run(
			mode, func(t *testing.T) {
				cooldown := &recordingCooldownStateStore{}
				manager, auth, _ := originalOAuthAliasManager(
					t, &Auth{
						ID: "auth-resetquota-ctx-test", Provider: "antigravity",
						Metadata: map[string]any{"type": "oauth"},
					},
					[]config.OAuthModelAlias{{Name: target, Alias: route}}, []string{route}, cooldown,
				)
				manager.MarkResult(
					t.Context(),
					originalOAuthAliasQuotaResult(manager, auth.ID, target, 30*time.Minute),
				)
				before, _ := manager.GetByID(auth.ID)
				records := originalOAuthAliasRecords(cooldown)
				ctx := t.Context()
				var want error
				if mode == "canceled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					want = context.Canceled
				}
				if mode == "save failure" {
					want = errors.New("simulated disk write failure")
					manager.SetStore(&auditLegacyUpdateStore{fail: want})
				}
				_, _, err := manager.ResetQuota(ctx, auth.ID)
				if want != nil {
					after, _ := manager.GetByID(auth.ID)
					if !errors.Is(err, want) || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(
						records, originalOAuthAliasRecords(cooldown),
					) ||
						!registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, route) {
						t.Fatalf("failed reset changed authority or lost error: %v", err)
					}
				} else if err !=
					nil ||
					len(originalOAuthAliasRecords(cooldown)) !=
						0 ||
					registry.GetGlobalRegistry().IsModelSuspendedForClient(
						auth.ID, route,
					) {
					t.Fatalf("successful local reset retained suspension: %v", err)
				}
			},
		)
	}
}

func TestOriginalNoForkAliasResultsResolveTargetOnce(t *testing.T) {
	for _, test := range []struct{ id, route, target, chained string }{
		{"auth-mark-route-b", "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high", ""},
		{"auth-no-secondary-resolve", "route-model-b", "target-model-a", "chained-model-c"},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				aliases := []config.OAuthModelAlias{{Name: test.target, Alias: test.route}}
				if test.chained != "" {
					aliases = append(aliases, config.OAuthModelAlias{Name: test.chained, Alias: test.target})
				}
				manager, auth, _ := originalOAuthAliasManager(
					t, &Auth{ID: test.id, Provider: "antigravity"}, aliases, []string{test.route}, nil,
				)
				actual := manager.ResolveExecutionModel(auth, test.route)
				if actual != test.target {
					t.Fatalf("resolved target=%s", actual)
				}
				manager.MarkResult(
					t.Context(),
					originalOAuthAliasQuotaResult(manager, auth.ID, actual, 20*time.Minute),
				)
				current, _ := manager.GetByID(auth.ID)
				if len(current.ModelStates) != 1 || current.ModelStates[test.target] == nil ||
					!current.ModelStates[test.target].Quota.Exceeded {
					t.Fatalf("result was attributed to another model: %+v", current.ModelStates)
				}
				if !registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, test.route) {
					t.Fatal("target failure did not suspend the requesting alias")
				}
				manager.MarkResult(
					t.Context(),
					executionResult(current, auth.Provider, actual, nil, core.DispatchPhaseResponseStarted),
				)
				if registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, test.route) {
					t.Fatal("matching target success did not recover its alias")
				}
			},
		)
	}
}

func TestOriginalNoForkAliasUnknownTransientKeepsRouteAvailable(t *testing.T) {
	const route, target = "[ant]gemini-3.7-flash-transient", "gemini-3.7-flash-transient"
	manager, auth, _ := originalOAuthAliasManager(
		t, &Auth{ID: "auth-transient-suspend", Provider: "antigravity"},
		[]config.OAuthModelAlias{{Name: target, Alias: route}}, []string{route}, nil,
	)
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		manager.MarkResult(
			t.Context(), executionResult(
				auth, auth.Provider, target,
				&Error{HTTPStatus: status, Message: http.StatusText(status)}, core.DispatchPhaseResponseStarted,
			),
		)
		if registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, route) {
			t.Fatalf("unknown HTTP %d created target suspension", status)
		}
		if _, err := manager.SelectAuth(t.Context(), auth.Provider, route, core.Options{}); err != nil {
			t.Fatalf("unknown HTTP %d blocked the next request: %v", status, err)
		}
	}
}

func TestOriginalNoForkAliasConcurrentNaturalConsistency(t *testing.T) {
	for _, test := range []struct {
		id, route, target string
		iterations        int
		actions           []string
		delay             time.Duration
	}{
		{
			"nofork-concurrent-auth", "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high", 100,
			[]string{"publish", "failure-reset", "execute"}, time.Millisecond,
		},
		{
			"concurrent-consistency-auth", "[ant]gemini-3.7-flash-high", "gemini-3.7-flash-high", 100,
			[]string{"publish", "failure-success-reset", "failure-reset", "execute"}, 50 * time.Microsecond,
		},
		{
			"auth-nat-consistency", "[ant]gemini-3.7-flash-nat-test", "gemini-3.7-flash-nat-test", 80,
			[]string{"publish", "failure-success", "reset", "execute"}, 100 * time.Microsecond,
		},
	} {
		t.Run(
			test.id, func(t *testing.T) {
				route, target := test.route, test.target
				store := &recordingCooldownStateStore{}
				manager, auth, _ := originalOAuthAliasManager(
					t, &Auth{ID: test.id, Provider: "antigravity"},
					[]config.OAuthModelAlias{{Name: target, Alias: route}}, []string{route}, store,
				)
				var workers sync.WaitGroup
				for _, action := range test.actions {
					workers.Go(
						func() {
							for iteration := range test.iterations {
								switch action {
								case "publish":
									manager.RefreshSchedulerEntry(auth.ID)
								case "failure-success", "failure-reset", "failure-success-reset":
									position := iteration % 2
									if action == "failure-success-reset" {
										position = iteration % 3
									}
									if position == 0 {
										manager.MarkResult(
											t.Context(),
											originalOAuthAliasQuotaResult(
												manager,
												auth.ID,
												target,
												10*time.Minute,
											),
										)
									} else if action == "failure-reset" || position == 2 {
										if _, _, err := manager.ResetQuota(t.Context(), auth.ID); err != nil {
											t.Errorf("local quota reset: %v", err)
										}
									} else {
										current, _ := manager.GetByID(auth.ID)
										manager.MarkResult(
											t.Context(), executionResult(
												current, auth.Provider, target, nil, core.DispatchPhaseResponseStarted,
											),
										)
									}
								case "reset":
									if iteration%3 == 0 {
										if _, _, err := manager.ResetQuota(t.Context(), auth.ID); err != nil {
											t.Errorf("local quota reset: %v", err)
										}
									}
								case "execute":
									_, _ = manager.Execute(
										t.Context(),
										[]string{auth.Provider},
										core.Request{Model: route},
										core.Options{},
									)
								}
								time.Sleep(test.delay)
							}
						},
					)
				}
				workers.Wait()
				current, _ := manager.GetByID(auth.ID)
				blocked, _, _ := isAuthBlockedForModel(current, target, time.Now())
				if registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, route) != blocked {
					t.Fatal("natural final registry state differs from the target")
				}
				found := false
				for _, record := range originalOAuthAliasRecords(store) {
					if record.AuthID == auth.ID && record.Model == target {
						found = true
					}
					if record.AuthID == auth.ID && record.Model == route {
						t.Fatal("persistence contains the route instead of the actual target")
					}
				}
				if found != blocked || current.ModelStates[route] != nil {
					t.Fatalf(
						"natural final persistence differs from target: persisted=%t blocked=%t",
						found,
						blocked,
					)
				}
			},
		)
	}
}

func TestOriginalNoForkAliasModelFailureKeepsIndependentModel(t *testing.T) {
	const first, other = "model-a-isolated", "model-c-isolated"
	manager, auth, _ := originalOAuthAliasManager(
		t, &Auth{ID: "auth-isolation-test", Provider: "gemini"}, nil, []string{first, other}, nil,
	)
	manager.MarkResult(t.Context(), originalOAuthAliasQuotaResult(manager, auth.ID, first, 30*time.Minute))
	manager.RefreshSchedulerEntry(auth.ID)
	reg := registry.GetGlobalRegistry()
	if !reg.IsModelSuspendedForClient(auth.ID, first) || reg.IsModelSuspendedForClient(auth.ID, other) {
		t.Fatal("model failure changed an independent model projection")
	}
	if selected, err := manager.SelectAuth(
		t.Context(), auth.Provider, other, core.Options{},
	); err != nil || selected.ID != auth.ID {
		t.Fatalf("independent model was not selectable: selected=%+v error=%v", selected, err)
	}
}

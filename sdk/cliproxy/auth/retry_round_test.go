package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCredentialRetryRoundReadyContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override any
		maxWait  time.Duration
		failure  error
		want     int
	}{
		{name: "inherited_zero_wait", maxWait: 0, want: 3},
		{name: "inherited_positive_wait", maxWait: time.Second, want: 3},
		{name: "negative_inherits", override: -1, maxWait: 0, want: 3},
		{name: "explicit_zero", override: 0, maxWait: 0, want: 1},
		{name: "explicit_one", override: 1, maxWait: 0, want: 2},
		{name: "unknown_stops", maxWait: time.Second, failure: errors.New("synthetic unknown result"), want: 1},
		{name: "request_terminated_stops", maxWait: time.Second, failure: &coreexecutor.RequestTerminatedError{HTTPStatus: 429}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := map[string]any{"disable_cooling": true}
			if tc.override != nil {
				metadata["request_retry"] = tc.override
			}
			manager, _ := newExecutionOutcomeTestManager(t, &Auth{
				ID: "synthetic-ready-retry", Provider: "test", Status: StatusActive, Metadata: metadata,
			})
			manager.RegisterExecutor(schedulerTestExecutor{})
			manager.SetRetryConfig(2, tc.maxWait, 0)
			registerReservationContextModels(t, "test", "synthetic-retry-model", "synthetic-ready-retry")
			failure := tc.failure
			if failure == nil {
				failure = reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}
			}
			calls := 0
			_, err := retryWithCooldown(manager, context.Background(), []string{"test"},
				coreexecutor.Request{Model: "synthetic-retry-model"}, coreexecutor.Options{},
				func(context.Context, []string, coreexecutor.Request, coreexecutor.Options, int) (struct{}, error) {
					calls++
					if calls > 8 {
						t.Fatal("retry round exceeded the synthetic test bound")
					}
					return struct{}{}, failure
				})
			if !errors.Is(err, failure) {
				t.Fatalf("terminal error = %v, want %v", err, failure)
			}
			if calls != tc.want {
				t.Fatalf("ready credential rounds = %d, want %d", calls, tc.want)
			}
		})
	}
}

type roundExecutor struct {
	schedulerTestExecutor
	calls     []string
	failure   error
	onCall    func()
	provider  string
	bootstrap bool
}

func (e *roundExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "test"
}

func (e *roundExecutor) fail(ctx context.Context, auth *Auth) error {
	e.calls = append(e.calls, auth.ID)
	if e.onCall != nil {
		e.onCall()
	}
	return e.failure
}
func (e *roundExecutor) Execute(ctx context.Context, auth *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, e.fail(ctx, auth)
}
func (e *roundExecutor) CountTokens(ctx context.Context, auth *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, e.fail(ctx, auth)
}
func (e *roundExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	err := e.fail(ctx, auth)
	if e.bootstrap {
		chunks := make(chan coreexecutor.StreamChunk, 1)
		chunks <- coreexecutor.StreamChunk{Err: err}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks, Headers: http.Header{"X-Request-Id": {"synthetic-stream"}}}, nil
	}
	return nil, err
}

func invokeRetryRound(manager *Manager, kind string, ctx context.Context, opts coreexecutor.Options) error {
	req := coreexecutor.Request{Model: "round-model"}
	switch kind {
	case "count":
		_, err := manager.ExecuteCount(ctx, []string{"test"}, req, opts)
		return err
	case "stream":
		_, err := manager.ExecuteStream(ctx, []string{"test"}, req, opts)
		return err
	default:
		_, err := manager.Execute(ctx, []string{"test"}, req, opts)
		return err
	}
}

func TestCredentialRetryRoundExecutionWindows(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, cap := range []int{0, 1, 2} {
			t.Run(kind+string(rune('0'+cap)), func(t *testing.T) {
				m := NewManager(nil, &FillFirstSelector{}, nil)
				executor := &roundExecutor{failure: reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}}
				m.RegisterExecutor(executor)
				m.SetRetryConfig(0, 0, cap)
				for id, limit := range map[string]int{"a": 3, "b": 2, "c": 2} {
					if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "test", Status: StatusActive, Metadata: map[string]any{"request_retry": limit, "disable_cooling": true}}); err != nil {
						t.Fatal(err)
					}
				}
				registerReservationContextModels(t, "test", "round-model", "a", "b", "c")
				if err := invokeRetryRound(m, kind, apiKeyReservationTestContext(), coreexecutor.Options{}); err == nil {
					t.Fatal("missing terminal failure")
				}
				counts := map[string]int{}
				for _, id := range executor.calls {
					counts[id]++
				}
				want := map[string]int{"a": 4, "b": 3, "c": 3}
				if cap == 1 {
					want = map[string]int{"a": 4}
				}
				if cap == 2 {
					want = map[string]int{"a": 4, "b": 3}
				}
				if !reflect.DeepEqual(counts, want) {
					t.Fatalf("calls=%v want=%v", counts, want)
				}
			})
		}
	}
}

func TestCredentialRetryRoundSnapshotAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		m := NewManager(nil, nil, nil)
		m.SetRetryConfig(2, 0, 1)
		ctx, cancel := context.WithCancel(apiKeyReservationTestContext())
		defer cancel()
		executor := &roundExecutor{failure: reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}}
		executor.onCall = func() {
			m.SetRetryConfig(20, 0, 1)
			if cancelRequest {
				cancel()
			}
		}
		m.RegisterExecutor(executor)
		if _, err := m.Register(ctx, &Auth{ID: "snapshot", Provider: "test", Metadata: map[string]any{"disable_cooling": true}}); err != nil {
			t.Fatal(err)
		}
		registerReservationContextModels(t, "test", "round-model", "snapshot")
		err := invokeRetryRound(m, "execute", ctx, coreexecutor.Options{})
		want := 3
		if cancelRequest {
			want = 1
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		}
		if len(executor.calls) != want {
			t.Fatalf("calls=%v want=%d", executor.calls, want)
		}
	}
}

type roundsHomeDispatcher struct {
	constraints  []home.AuthDispatchConstraints
	retry        *int
	errorPayload []byte
	errorAfter   int
}

func (*roundsHomeDispatcher) HeartbeatOK() bool { return true }
func (*roundsHomeDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	panic("constraints capability was bypassed")
}
func (d *roundsHomeDispatcher) RPopAuthWithConstraints(_ context.Context, _, _ string, _ http.Header, _ int, c home.AuthDispatchConstraints) ([]byte, error) {
	d.constraints = append(d.constraints, c)
	if d.errorPayload != nil && len(d.constraints) > d.errorAfter {
		return d.errorPayload, nil
	}
	for _, id := range []string{"home-a", "home-b"} {
		excluded := false
		for _, other := range c.ExcludedAuthIDs {
			if other == id {
				excluded = true
			}
		}
		if excluded || (c.PinnedAuthID != "" && c.PinnedAuthID != id) {
			continue
		}
		return json.Marshal(homeAuthDispatchResponse{Auth: Auth{ID: id, Provider: "test", Metadata: map[string]any{"disable_cooling": true}}, RequestRetry: d.retry})
	}
	return []byte(`{"error":{"type":"auth_unavailable","message":"round exhausted"}}`), nil
}

func TestHomeCredentialRetryRoundContract(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, tc := range []struct {
			name                         string
			global, aggregate, cap, want int
			pinned                       string
		}{
			{"fresh", 0, 0, 2, 2, ""}, {"rounds", 0, 1, 2, 4, ""}, {"zero_authority", 3, 0, 2, 2, ""}, {"cap", 0, 1, 1, 2, ""}, {"pinned", 1, 1, 2, 2, "home-a"},
		} {
			t.Run(kind+tc.name, func(t *testing.T) {
				d := &roundsHomeDispatcher{retry: &tc.aggregate}
				m := NewManager(nil, nil, nil)
				m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				m.SetRetryConfig(tc.global, 0, tc.cap)
				m.PublishHomeDispatch(d, executionregistry.New(), 1)
				e := &roundExecutor{failure: reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}}
				m.RegisterExecutor(e)
				opts := coreexecutor.Options{Metadata: map[string]any{}}
				if tc.pinned != "" {
					opts.Metadata[coreexecutor.PinnedAuthMetadataKey] = tc.pinned
				}
				err := invokeRetryRound(m, kind, context.Background(), opts)
				if err == nil || len(e.calls) != tc.want {
					t.Fatalf("calls=%v want=%d error=%v constraints=%v", e.calls, tc.want, err, d.constraints)
				}
				for _, c := range d.constraints {
					if c.PinnedAuthID != tc.pinned {
						t.Fatal(c)
					}
				}
			})
		}
	}
}

func TestCredentialRetryRoundWaitEligibility(t *testing.T) {
	m, _ := newExecutionOutcomeTestManager(t, &Auth{ID: "wait", Provider: "test", Status: StatusActive})
	m.RegisterExecutor(schedulerTestExecutor{})
	m.SetRetryConfig(2, 0, 0)
	registerReservationContextModels(t, "test", "round-model", "wait")
	state := &retryRoundState{defaultRetry: 2, attempted: map[string]struct{}{"wait": {}}}
	ctx := context.WithValue(context.Background(), retryRoundContextKey{}, state)
	wait, ok := m.closestCooldownWait(ctx, coreexecutor.Options{}, []string{"test"}, "round-model", 0, 429)
	if !ok || wait < 10*time.Second {
		t.Fatalf("attempted 429 wait=%v allowed=%t", wait, ok)
	}
	for _, opts := range []coreexecutor.Options{{Metadata: map[string]any{ExcludedAuthIDsMetadataKey: []string{"wait"}}}, {Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: "other"}}} {
		if _, ok := m.closestCooldownWait(ctx, opts, []string{"test"}, "round-model", 0, 429); ok {
			t.Fatal("ineligible wait candidate")
		}
	}
	if _, ok := m.closestCooldownWait(ctx, coreexecutor.Options{}, []string{"test"}, "unsupported", 0, 429); ok {
		t.Fatal("unsupported model waited")
	}
}

func TestHomeCredentialRetryErrorAuthorityAndPendingCooldown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		calls   int
	}{
		{"zero", `{"error":{"type":"model_cooldown","message":"synthetic","retryable":true,"retry_after_ms":1,"request_retry":0}}`, 1},
		{"pending_stops", `{"error":{"type":"model_cooldown","message":"synthetic","retryable":true,"retry_after_ms":1,"request_retry":4}}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &roundsHomeDispatcher{errorPayload: []byte(tc.payload)}
			m := NewManager(nil, nil, nil)
			m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			m.SetRetryConfig(3, time.Second, 0)
			m.PublishHomeDispatch(d, executionregistry.New(), 1)
			if err := invokeRetryRound(m, "execute", context.Background(), coreexecutor.Options{}); err == nil {
				t.Fatal("expected cooldown error")
			}
			if len(d.constraints) != tc.calls {
				t.Fatalf("dispatches=%d want=%d", len(d.constraints), tc.calls)
			}
		})
	}
	state := &retryRoundState{homeLimit: 5, homeAuthority: true}
	limit := 1
	state.observeHomeLimit(&limit, nil)
	if state.homeLimit != 1 {
		t.Fatal("aggregate limit did not decrease")
	}
	state.observeHomeLimit(nil, &Auth{Metadata: map[string]any{"request_retry": 9}})
	if state.homeLimit != 1 {
		t.Fatal("credential metadata replaced authoritative budget")
	}
	state.observeHomeAttempt(&homeDispatchRetryAfterError{cause: &Error{}, retryAfter: time.Second})
	state.observeHomeAttempt(&homeDispatchRetryAfterError{cause: &Error{}, retryAfter: 2 * time.Second})
	if state.homeWait != time.Second {
		t.Fatal("did not choose earliest deadline")
	}
	state.observeHomeError(&homeDispatchRetryAfterError{cause: &Error{Code: "model_cooldown"}, retryAfter: 3 * time.Second})
	if state.homeWait != 3*time.Second {
		t.Fatal("remote cooldown did not override attempted timing")
	}
}

func TestHomeCooldownTimingPreservesExecutionDisposition(t *testing.T) {
	cause := reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}
	failure := &homeDispatchRetryAfterError{cause: cause, retryAfter: 1500 * time.Millisecond}
	if !errors.Is(failure, cause) || !retryDirectiveAllowsCooldownRetry(executionDispositionForError(failure, coreexecutor.DispatchPhaseMaybeSent)) {
		t.Fatal("original disposition lost")
	}
	if *retryAfterFromError(failure) != 1500*time.Millisecond || SafeResponseHeaders(failure).Get("Retry-After") != "2" {
		t.Fatal("remote timing lost")
	}
	unknown := errors.New("synthetic unknown")
	if retryDirectiveAllowsCooldownRetry(executionDispositionForError(&homeDispatchRetryAfterError{cause: unknown, retryAfter: time.Second}, coreexecutor.DispatchPhaseMaybeSent)) {
		t.Fatal("timing authorized unknown replay")
	}
}

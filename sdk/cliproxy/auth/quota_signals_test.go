package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestQuotaObservationBoundsAndSnapshotSemantics(t *testing.T) {
	observedAt := time.Unix(100, 0)
	var quota QuotaState
	if !quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"x-codex-plan-type":            {"pro"},
		"X-Codex-Primary-Used-Percent": {"91"},
		"Retry-After":                  {"120"},
		"Authorization":                {"Bearer secret"},
		"X-Codex-Turn-State":           {"opaque"},
	}, observedAt) {
		t.Fatal("initial observation reported no change")
	}
	if !quota.ObservedAt.Equal(observedAt) {
		t.Fatalf("ObservedAt = %v, want %v", quota.ObservedAt, observedAt)
	}
	if quota.Signals["X-Codex-Plan-Type"] != "pro" ||
		quota.Signals["X-Codex-Primary-Used-Percent"] != "91" ||
		quota.Signals["Retry-After"] != "120" {
		t.Fatalf("quota signals = %#v", quota.Signals)
	}
	if _, ok := quota.Signals["Authorization"]; ok {
		t.Fatal("authorization header was retained")
	}
	if _, ok := quota.Signals["X-Codex-Turn-State"]; ok {
		t.Fatal("non-quota Codex header was retained")
	}

	if !quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"X-Codex-Primary-Used-Percent": {"5"},
	}, time.Unix(200, 0)) {
		t.Fatal("replacement observation reported no change")
	}
	if _, ok := quota.Signals["Retry-After"]; ok {
		t.Fatalf("stale Retry-After survived replacement: %#v", quota.Signals)
	}
	if quota.Signals["X-Codex-Primary-Used-Percent"] != "5" ||
		!quota.ObservedAt.Equal(time.Unix(200, 0)) {
		t.Fatalf("replacement snapshot = %#v", quota)
	}

	before := quota.Clone()
	if quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"Content-Type": {"application/json"},
	}, time.Unix(300, 0)) {
		t.Fatal("signal-free response reported a change")
	}
	if !reflect.DeepEqual(quota, before) {
		t.Fatalf("signal-free response changed snapshot: got %#v want %#v", quota, before)
	}

	if !quota.ObserveResponseHeadersForProvider("gemini", http.Header{
		"X-Codex-Primary-Used-Percent": {"100"},
	}, time.Unix(400, 0)) {
		t.Fatal("unsupported provider did not clear an old snapshot")
	}
	if !quota.ObservedAt.IsZero() || len(quota.Signals) != 0 {
		t.Fatalf("unsupported provider retained observation: %#v", quota)
	}
}

func TestQuotaObservationRejectsInvalidValuesAndTruncatesDeterministically(t *testing.T) {
	invalid := []string{
		"",
		strings.Repeat("x", maxQuotaSignalValue+1),
		"evil\r\nX-Injected: 1",
		"evil\x00value",
		"evil\x7fvalue",
		"evil\u0085value",
	}
	for index, value := range invalid {
		var quota QuotaState
		if quota.ObserveResponseHeadersForProvider("codex", http.Header{
			"X-Codex-Bengalfox-Limit-Name": {value},
		}, time.Unix(int64(index+1), 0)) {
			t.Fatalf("invalid value %q was accepted", value)
		}
		if len(quota.Signals) != 0 || !quota.ObservedAt.IsZero() {
			t.Fatalf("invalid value changed quota: %#v", quota)
		}
	}

	headers := make(http.Header, maxQuotaSignalHeaders*2+3)
	headers.Set("X-Codex-Plan-Type", "pro")
	headers.Set("X-Codex-Primary-Used-Percent", "81")
	headers.Set("X-Codex-Credits-Balance", "0")
	for index := 0; index < maxQuotaSignalHeaders*2; index++ {
		headers.Set(
			fmt.Sprintf("X-Codex-Additional-L%03d-Primary-Used-Percent", index),
			strconv.Itoa(index),
		)
	}
	var first QuotaState
	first.ObserveResponseHeadersForProvider("codex", headers, time.Unix(100, 0))
	if len(first.Signals) != maxQuotaSignalHeaders {
		t.Fatalf("snapshot size = %d, want %d", len(first.Signals), maxQuotaSignalHeaders)
	}
	for _, key := range []string{
		"X-Codex-Plan-Type",
		"X-Codex-Primary-Used-Percent",
		"X-Codex-Credits-Balance",
	} {
		if _, ok := first.Signals[key]; !ok {
			t.Fatalf("priority signal %q was truncated: %#v", key, first.Signals)
		}
	}
	for iteration := 0; iteration < 10; iteration++ {
		var next QuotaState
		next.ObserveResponseHeadersForProvider("codex", headers, time.Unix(100, 0))
		if !reflect.DeepEqual(first.Signals, next.Signals) {
			t.Fatal("identical observations produced different truncated snapshots")
		}
	}
}

func TestQuotaObservationCloneAndCooldownUpdatesAreIndependent(t *testing.T) {
	quota := QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		NextRecoverAt: time.Unix(20, 0),
		BackoffLevel:  1,
		ObservedAt:    time.Unix(10, 0),
		Signals:       map[string]string{"X-Codex-Primary-Used-Percent": "51"},
	}
	clone := quota.Clone()
	clone.Signals["X-Codex-Primary-Used-Percent"] = "99"
	if quota.Signals["X-Codex-Primary-Used-Percent"] != "51" {
		t.Fatal("QuotaState.Clone shared the signal map")
	}

	applyCooldownFields(&quota, QuotaState{
		Exceeded:      true,
		Reason:        "credential_quota",
		NextRecoverAt: time.Unix(40, 0),
		BackoffLevel:  2,
	})
	if quota.Reason != "credential_quota" || quota.BackoffLevel != 2 ||
		!quota.NextRecoverAt.Equal(time.Unix(40, 0)) {
		t.Fatalf("cooldown fields were not updated: %#v", quota)
	}
	if !quota.ObservedAt.Equal(time.Unix(10, 0)) ||
		quota.Signals["X-Codex-Primary-Used-Percent"] != "51" {
		t.Fatalf("cooldown update replaced observation: %#v", quota)
	}
}

func TestUpstreamAttemptResponseHeadersAreIsolatedMergedAndCloned(t *testing.T) {
	parent := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(parent, http.Header{"X-Attempt": {"parent"}})
	first := newUpstreamAttemptContext(parent, true)
	second := newUpstreamAttemptContext(parent, false)
	httpHeaders := http.Header{"X-Codex-Primary-Used-Percent": {"91"}}
	websocketHeaders := http.Header{"X-Codex-Secondary-Used-Percent": {"42"}}
	RecordUpstreamResponseHeaders(first, httpHeaders)
	RecordUpstreamResponseHeaders(first, websocketHeaders)
	httpHeaders.Set("X-Codex-Primary-Used-Percent", "mutated")
	websocketHeaders.Set("X-Codex-Secondary-Used-Percent", "mutated")

	gotFirst := upstreamResponseHeaders(first)
	if gotFirst.Get("X-Codex-Primary-Used-Percent") != "91" ||
		gotFirst.Get("X-Codex-Secondary-Used-Percent") != "42" {
		t.Fatalf("first attempt headers = %#v", gotFirst)
	}
	gotFirst.Set("X-Codex-Primary-Used-Percent", "caller-mutated")
	if upstreamResponseHeaders(first).Get("X-Codex-Primary-Used-Percent") != "91" {
		t.Fatal("returned headers shared holder storage")
	}
	if gotSecond := upstreamResponseHeaders(second); len(gotSecond) != 0 {
		t.Fatalf("second attempt inherited first headers: %#v", gotSecond)
	}
	if gotParent := upstreamResponseHeaders(parent); len(gotParent) != 0 {
		t.Fatalf("request context received attempt headers: %#v", gotParent)
	}
	if inherited := internallogging.GetResponseHeaders(first); len(inherited) != 0 {
		t.Fatalf("attempt logging holder inherited parent headers: %#v", inherited)
	}
	if !coreusage.StreamFromContext(first) || coreusage.StreamFromContext(second) {
		t.Fatalf(
			"attempt stream flags = first:%t second:%t",
			coreusage.StreamFromContext(first),
			coreusage.StreamFromContext(second),
		)
	}
}

type quotaCoreTestExecutor struct {
	provider     string
	countHeaders http.Header
	countErr     error
}

func (e *quotaCoreTestExecutor) Identifier() string { return e.provider }

func (*quotaCoreTestExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected Execute call")
}

func (*quotaCoreTestExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("unexpected ExecuteStream call")
}

func (*quotaCoreTestExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, errors.New("unexpected Refresh call")
}

func (e *quotaCoreTestExecutor) CountTokens(
	ctx context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	RecordUpstreamResponseHeaders(ctx, e.countHeaders)
	return cliproxyexecutor.Response{}, e.countErr
}

func (*quotaCoreTestExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HttpRequest call")
}

func TestExecuteCountDoesNotAdvanceObservationOrRecoverAvailability(t *testing.T) {
	const (
		provider = "codex-count-observation"
		model    = "count-observation-model"
		authID   = "count-observation-auth"
	)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &quotaCoreTestExecutor{
		provider: provider,
		countHeaders: http.Header{
			"X-Codex-Primary-Used-Percent": {"99"},
		},
	}
	manager.RegisterExecutor(executor)
	registerSchedulerModels(t, provider, model, authID)
	originalObservation := time.Unix(10, 0)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:            authID,
		Provider:      provider,
		Status:        StatusError,
		StatusMessage: "existing failure",
		LastError:     &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "existing failure"},
		Quota: QuotaState{
			ObservedAt: originalObservation,
			Signals:    map[string]string{"X-Codex-Primary-Used-Percent": "40"},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	if _, errCount := manager.ExecuteCount(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{},
	); errCount != nil {
		t.Fatalf("ExecuteCount() error = %v", errCount)
	}
	updated, ok := manager.GetByID(registered.ID)
	if !ok || updated == nil {
		t.Fatal("count_tokens auth disappeared")
	}
	if updated.Status != StatusError || updated.StatusMessage != "existing failure" || updated.LastError == nil {
		t.Fatalf("count_tokens recovered availability state: %#v", updated)
	}
	if !updated.Quota.ObservedAt.Equal(originalObservation) ||
		updated.Quota.Signals["X-Codex-Primary-Used-Percent"] != "40" {
		t.Fatalf("count_tokens advanced observation: %#v", updated.Quota)
	}

	executor.countErr = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
	executor.countHeaders.Set("X-Codex-Primary-Used-Percent", "100")
	_, _ = manager.ExecuteCount(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		cliproxyexecutor.Options{},
	)
	failed, _ := manager.GetByID(registered.ID)
	if !failed.Quota.ObservedAt.Equal(originalObservation) ||
		failed.Quota.Signals["X-Codex-Primary-Used-Percent"] != "40" {
		t.Fatalf("failed count_tokens advanced observation: %#v", failed.Quota)
	}
}

type quotaAttemptIsolationSelector struct{}

func (quotaAttemptIsolationSelector) Pick(
	_ context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	var selected *Auth
	for _, auth := range auths {
		if auth != nil && (selected == nil || auth.ID < selected.ID) {
			selected = auth
		}
	}
	return selected, nil
}

type quotaAttemptIsolationExecutor struct{}

func (*quotaAttemptIsolationExecutor) Identifier() string { return "codex" }

func (*quotaAttemptIsolationExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth != nil && strings.HasSuffix(auth.ID, "-b")
}

func (*quotaAttemptIsolationExecutor) PrepareRequestAuth(context.Context, *Auth) (*Auth, error) {
	return nil, errors.New("prepare failed before upstream response")
}

func (*quotaAttemptIsolationExecutor) Execute(
	ctx context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	RecordUpstreamResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": {"91"}})
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
}

func (*quotaAttemptIsolationExecutor) ExecuteStream(
	ctx context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	RecordUpstreamResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": {"91"}})
	return nil, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
}

func (*quotaAttemptIsolationExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, errors.New("unexpected Refresh call")
}

func (*quotaAttemptIsolationExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected CountTokens call")
}

func (*quotaAttemptIsolationExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HttpRequest call")
}

func TestExecutionAttemptsDoNotReuseQuotaResponseHeaders(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Manager, context.Context, string) error
	}{
		{
			name: "non-stream",
			run: func(manager *Manager, ctx context.Context, model string) error {
				_, errExecute := manager.Execute(
					ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
				)
				return errExecute
			},
		},
		{
			name: "stream",
			run: func(manager *Manager, ctx context.Context, model string) error {
				_, errExecute := manager.ExecuteStream(
					ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true},
				)
				return errExecute
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
			manager.RegisterExecutor(&quotaAttemptIsolationExecutor{})
			model := "quota-attempt-isolation-" + test.name
			firstID := "quota-attempt-" + test.name + "-a"
			secondID := "quota-attempt-" + test.name + "-b"
			registerSchedulerModels(t, "codex", model, firstID, secondID)
			for _, authID := range []string{firstID, secondID} {
				if _, errRegister := manager.Register(context.Background(), &Auth{
					ID: authID, Provider: "codex", Status: StatusActive,
				}); errRegister != nil {
					t.Fatalf("Register(%s) error = %v", authID, errRegister)
				}
			}

			if errExecute := test.run(manager, context.Background(), model); errExecute == nil {
				t.Fatal("execution error = nil, want terminal prepare failure")
			}
			first, _ := manager.GetByID(firstID)
			second, _ := manager.GetByID(secondID)
			if first.Quota.Signals["X-Codex-Primary-Used-Percent"] != "91" {
				t.Fatalf("first attempt observation = %#v", first.Quota)
			}
			if len(second.Quota.Signals) != 0 || !second.Quota.ObservedAt.IsZero() {
				t.Fatalf("pre-response failure inherited first attempt headers: %#v", second.Quota)
			}
		})
	}
}

func TestProviderSupportsQuotaObservation(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "devin", " CODEX "} {
		if !ProviderSupportsQuotaObservation(provider) {
			t.Fatalf("provider %q should support passive quota observation", provider)
		}
	}
	for _, provider := range []string{"", "gemini", "xai", "grok", "antigravity", "openai"} {
		if ProviderSupportsQuotaObservation(provider) {
			t.Fatalf("provider %q unexpectedly supports passive quota observation", provider)
		}
	}
}

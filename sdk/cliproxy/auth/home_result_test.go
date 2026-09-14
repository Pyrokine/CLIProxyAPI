package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type c08HomeResultCapturePlugin struct {
	records chan coreusage.Record
}

func (p c08HomeResultCapturePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	if p.records != nil {
		p.records <- record
	}
}

type c08HomeCountTokensExecutor struct {
	calls           atomic.Int32
	refreshCalls    atomic.Int32
	refreshInputsMu sync.Mutex
	refreshInputs   []string
}

func (*c08HomeCountTokensExecutor) Identifier() string { return "codex" }

func (*c08HomeCountTokensExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("unexpected Execute call")
}

func (*c08HomeCountTokensExecutor) ExecuteStream(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	return nil, fmt.Errorf("unexpected ExecuteStream call")
}

func (e *c08HomeCountTokensExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	e.refreshInputsMu.Lock()
	e.refreshInputs = append(e.refreshInputs, authAccessToken(auth))
	e.refreshInputsMu.Unlock()
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["access_token"] = "refreshed-access-token"
	return updated, nil
}

func (e *c08HomeCountTokensExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	call := e.calls.Add(1)
	effective := auth.Clone()
	if effective.Metadata == nil {
		effective.Metadata = make(map[string]any)
	}
	if call == 1 {
		effective.Metadata["access_token"] = "executor-internal-token"
	} else {
		effective.Metadata["access_token"] = "retry-internal-token"
	}
	NotifyAccessTokenFingerprint(ctx, effective)
	return cliproxyexecutor.Response{}, &Error{
		HTTPStatus: http.StatusUnauthorized,
		Message:    "expired access token",
	}
}

func (*c08HomeCountTokensExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected HttpRequest call")
}

func TestC08ReportHomeUnauthorizedPublishesZeroTokenFingerprint(t *testing.T) {
	records := make(chan coreusage.Record, 8)
	const pluginName = "auth-home-result-c08-test"
	coreusage.RegisterNamedPlugin(pluginName, c08HomeResultCapturePlugin{records: records})
	t.Cleanup(
		func() {
			coreusage.RegisterNamedPlugin(pluginName, c08HomeResultCapturePlugin{})
		},
	)

	auth := &Auth{
		ID:         "home-result-auth",
		Index:      "home-result-index",
		Provider:   "codex",
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata: map[string]any{
			"token": map[string]any{"accessToken": " current-access-token "},
		},
	}
	ctx := coreusage.WithRequestedModelAlias(context.Background(), "client-model")
	NewManager(nil, nil, nil).ReportHomeUnauthorized(ctx, auth, "codex", "upstream-model")

	deadline := time.After(time.Second)
	for {
		select {
		case record := <-records:
			if record.AuthID != auth.ID || record.ExecutorType != homeResultExecutorType {
				continue
			}
			if !record.Failed || record.Fail.StatusCode != http.StatusUnauthorized {
				t.Fatalf("failure = %#v, want 401", record.Fail)
			}
			if record.AuthIndex != auth.Index {
				t.Fatalf("auth index = %q, want %q", record.AuthIndex, auth.Index)
			}
			if record.AccessTokenSHA256 != AccessTokenSHA256(auth) || record.AccessTokenSHA256 == "" {
				t.Fatalf("access token fingerprint = %q", record.AccessTokenSHA256)
			}
			if record.Model != "upstream-model" || record.Alias != "client-model" {
				t.Fatalf("model/alias = %q/%q", record.Model, record.Alias)
			}
			if coreusage.GenerateEnabled(record.Generate) {
				t.Fatal("unauthorized result-only record was marked as generation")
			}
			if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
				t.Fatalf("unauthorized token detail = %#v, want zero", record.Detail)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for Home unauthorized usage record")
		}
	}
}

func TestC08HomeCountTokensReportsEveryUnauthorizedAttempt(t *testing.T) {
	records := make(chan coreusage.Record, 8)
	const pluginName = "auth-home-count-unauthorized-c08-test"
	coreusage.RegisterNamedPlugin(pluginName, c08HomeResultCapturePlugin{records: records})
	t.Cleanup(
		func() {
			coreusage.RegisterNamedPlugin(pluginName, c08HomeResultCapturePlugin{})
		},
	)

	executor := &c08HomeCountTokensExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	dispatchPayload, errMarshal := json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "home-count-index",
			Auth: Auth{
				ID:         "home-count-auth",
				Provider:   "codex",
				Attributes: map[string]string{"auth_kind": "oauth"},
				Metadata:   map[string]any{"access_token": "dispatch-access-token"},
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal Home dispatch payload: %v", errMarshal)
	}
	dispatcher := &testHomeDispatcher{heartbeat: true, payload: dispatchPayload}
	previousDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previousDispatcher })

	_, errCount := manager.ExecuteCount(
		coreusage.WithRequestedModelAlias(context.Background(), "client-model"),
		[]string{"codex"},
		cliproxyexecutor.Request{Model: "upstream-model"},
		cliproxyexecutor.Options{},
	)
	if statusCodeFromError(errCount) != http.StatusUnauthorized {
		t.Fatalf("ExecuteCount() error = %v, want final 401", errCount)
	}
	if got := executor.calls.Load(); got != 2 {
		t.Fatalf("CountTokens calls = %d, want 2", got)
	}
	if got := executor.refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	executor.refreshInputsMu.Lock()
	refreshInputs := append([]string(nil), executor.refreshInputs...)
	executor.refreshInputsMu.Unlock()
	if len(refreshInputs) != 1 || refreshInputs[0] != "executor-internal-token" {
		t.Fatalf("refresh input tokens = %#v, want executor-internal-token", refreshInputs)
	}

	wantHashes := map[string]bool{
		AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "executor-internal-token"}}): false,
		AccessTokenSHA256(&Auth{Metadata: map[string]any{"access_token": "retry-internal-token"}}):    false,
	}
	matched := 0
	deadline := time.After(time.Second)
	for remaining := len(wantHashes); remaining > 0; {
		select {
		case record := <-records:
			if record.AuthID != "home-count-auth" || record.ExecutorType != homeResultExecutorType ||
				record.Fail.StatusCode != http.StatusUnauthorized {
				continue
			}
			matched++
			if seen, ok := wantHashes[record.AccessTokenSHA256]; ok && !seen {
				wantHashes[record.AccessTokenSHA256] = true
				remaining--
			}
		case <-deadline:
			t.Fatalf("unauthorized attempt fingerprints = %#v", wantHashes)
		}
	}
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case record := <-records:
			if record.AuthID == "home-count-auth" && record.ExecutorType == homeResultExecutorType &&
				record.Fail.StatusCode == http.StatusUnauthorized {
				matched++
			}
		case <-timer.C:
			if matched != 2 {
				t.Fatalf("unauthorized usage records = %d, want exactly 2", matched)
			}
			return
		}
	}
}

func TestC08ReportHomeUnauthorizedRequiresTokenFingerprint(t *testing.T) {
	records := make(chan coreusage.Record, 1)
	const pluginName = "auth-home-result-empty-token-c08-test"
	coreusage.RegisterNamedPlugin(pluginName, c08HomeResultCapturePlugin{records: records})
	t.Cleanup(
		func() {
			coreusage.RegisterNamedPlugin(pluginName, c08HomeResultCapturePlugin{})
		},
	)

	NewManager(nil, nil, nil).ReportHomeUnauthorized(
		context.Background(), &Auth{
			ID:       "home-result-no-token",
			Index:    "home-result-no-token",
			Provider: "codex",
		}, "codex", "model",
	)

	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case record := <-records:
			if record.AuthID == "home-result-no-token" {
				t.Fatalf("unexpected usage record without token fingerprint: %#v", record)
			}
		case <-timer.C:
			return
		}
	}
}

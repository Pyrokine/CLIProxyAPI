package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
)

type originalQuotaAttemptExecutor struct {
	quotaAttemptIsolationExecutor
	failure      *Error
	recordQuota  bool
	executions   int
	preparations int
}

func (e *originalQuotaAttemptExecutor) PrepareRequestAuth(context.Context, *Auth) (*Auth, error) {
	e.preparations++
	return nil, &Error{
		HTTPStatus: http.StatusInternalServerError,
		Message:    "prepare failed before upstream response",
	}
}

func (e *originalQuotaAttemptExecutor) recordResponse(ctx context.Context) {
	e.executions++
	headers := http.Header{
		"X-Codex-Plan-Type":                   {"pro"},
		"X-Codex-Primary-Used-Percent":        {"91"},
		"X-Codex-Primary-Window-Minutes":      {"10080"},
		"X-Codex-Primary-Reset-After-Seconds": {"3600"},
	}
	internallogging.SetResponseHeaders(ctx, headers)
	if e.recordQuota {
		RecordUpstreamResponseHeaders(ctx, headers)
	}
}

func (e *originalQuotaAttemptExecutor) Execute(
	ctx context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.recordResponse(ctx)
	return cliproxyexecutor.Response{}, e.failure
}

func (e *originalQuotaAttemptExecutor) ExecuteStream(
	ctx context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.recordResponse(ctx)
	return nil, e.failure
}

func TestOriginalExecutionAttemptsDoNotReuseQuotaResponseHeaders(t *testing.T) {
	for _, operation := range []string{"non-stream", "stream"} {
		for _, producer := range []string{
			"original_logging_only", "current_attempt_unknown_500", "current_attempt_rejected_429",
		} {
			t.Run(
				operation+"/"+producer, func(t *testing.T) {
					status := http.StatusInternalServerError
					if producer == "current_attempt_rejected_429" {
						status = http.StatusTooManyRequests
					}
					provider := &originalQuotaAttemptExecutor{
						failure:     &Error{HTTPStatus: status, Message: "first upstream attempt failed"},
						recordQuota: producer != "original_logging_only",
					}
					manager := NewManager(nil, quotaAttemptIsolationSelector{}, nil)
					manager.RegisterExecutor(provider)
					model := "gpt-quota-attempt-isolation-" + operation
					firstID := "quota-attempt-" + operation + "-a"
					secondID := "quota-attempt-" + operation + "-b"
					for _, id := range []string{firstID, secondID} {
						if _, err := manager.Register(
							context.Background(), &Auth{
								ID: id, Provider: "codex", Status: StatusActive,
							},
						); err != nil {
							t.Fatal(err)
						}
						registerSchedulerModels(t, "codex", model, id)
					}
					ctx := internallogging.WithResponseHeadersHolder(context.Background())
					var err error
					if operation == "stream" {
						_, err = manager.ExecuteStream(
							ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model},
							cliproxyexecutor.Options{Stream: true},
						)
					} else {
						_, err = manager.Execute(
							ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
						)
					}
					if err == nil || provider.executions != 1 {
						t.Fatalf("execution error=%v attempts=%d", err, provider.executions)
					}
					wantPreparations := 0
					if producer == "current_attempt_rejected_429" {
						wantPreparations = 1
					}
					if provider.preparations != wantPreparations {
						t.Fatalf("preparations=%d, want %d", provider.preparations, wantPreparations)
					}
					first := getExecutionOutcomeTestAuth(t, manager, firstID)
					second := getExecutionOutcomeTestAuth(t, manager, secondID)
					want := map[string]string{
						"X-Codex-Plan-Type": "pro", "X-Codex-Primary-Used-Percent": "91",
						"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-After-Seconds": "3600",
					}
					if provider.recordQuota {
						if !reflect.DeepEqual(first.Quota.Signals, want) || first.Quota.ObservedAt.IsZero() {
							t.Fatalf("first attempt lost the original quota headers: %+v", first.Quota)
						}
					} else if len(first.Quota.Signals) != 0 || !first.Quota.ObservedAt.IsZero() {
						t.Fatalf("logging-only headers entered the independent quota holder: %+v", first.Quota)
					}
					if len(second.Quota.Signals) != 0 || !second.Quota.ObservedAt.IsZero() {
						t.Fatalf("pre-response failure inherited earlier headers: %+v", second.Quota)
					}
					if headers := internallogging.GetResponseHeaders(ctx); len(headers) != 0 {
						t.Fatalf("attempt headers leaked into the request holder: %#v", headers)
					}
				},
			)
		}
	}
}

func TestOriginalSyncMetadataSessionToContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous internallogging.ClientRequestMetadata
		metadata map[string]any
		session  string
		parent   string
	}{
		{
			name:     "canonical overrides raw alias",
			previous: internallogging.ClientRequestMetadata{SessionID: "raw-alias-sid"},
			metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "canonical-override"},
			session:  "canonical-override",
		},
		{
			name: "canonical with parent",
			metadata: map[string]any{
				cliproxyexecutor.CanonicalSessionIDMetadataKey: "lcp:branch-123",
				cliproxyexecutor.ParentSessionIDMetadataKey:    "lcp:trunk-000",
			},
			session: "lcp:branch-123", parent: "lcp:trunk-000",
		},
		{
			name: "eliminate self loop",
			metadata: map[string]any{
				cliproxyexecutor.CanonicalSessionIDMetadataKey: "same-loop",
				cliproxyexecutor.ParentSessionIDMetadataKey:    "same-loop",
			},
			session: "same-loop",
		},
		{
			name: "new root clears old parent",
			previous: internallogging.ClientRequestMetadata{
				SessionID: "old-child", ParentSessionID: "old-stale-parent",
			},
			metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "new-root-session"},
			session:  "new-root-session",
		},
		{
			name:     "execution fallback",
			metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "call-live-123"},
			session:  "execution:call-live-123",
		},
		{
			name:     "derived fallback",
			metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:raw-hash"},
			session:  "derived:ctx:v1:raw-hash",
		},
		{
			name: "empty metadata clears hierarchy",
			previous: internallogging.ClientRequestMetadata{
				SessionID: "old-stale-session", ParentSessionID: "old-stale-parent",
			},
			metadata: map[string]any{},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				ctx := internallogging.WithClientRequestMetadata(context.Background(), tc.previous)
				updated := syncMetadataSessionToContext(ctx, tc.metadata)
				got := internallogging.GetClientRequestMetadata(updated)
				if got.SessionID != tc.session || got.ParentSessionID != tc.parent {
					t.Fatalf(
						"session=(%q,%q), want (%q,%q)",
						got.SessionID,
						got.ParentSessionID,
						tc.session,
						tc.parent,
					)
				}
			},
		)
	}
}

func TestOriginalGhostParentElimination(t *testing.T) {
	req := cliproxyexecutor.Request{}
	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Session-ID": {"clean-root-session"}},
		Metadata: map[string]any{cliproxyexecutor.ParentSessionIDMetadataKey: "ghost-parent-from-prior-turn"},
	}
	_, enriched := coresession.Enrich(req, opts)
	if _, exists := enriched.Metadata[cliproxyexecutor.ParentSessionIDMetadataKey]; exists {
		t.Fatal("session.Enrich retained the original ghost parent")
	}
	if canonical := enriched.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey]; canonical !=
		("header:clean-r" +
			"oot-session") {
		t.Fatalf("canonical=%v, want header:clean-root-session", canonical)
	}
}

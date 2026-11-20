package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAuditDiscardStreamChunksEndsOnCancelAndBound(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			for _, canceled := range []bool{false, true} {
				ctx, cancel := context.WithCancel(t.Context())
				done := discardStreamChunks(ctx, make(chan executor.StreamChunk), nil)
				if canceled {
					cancel()
				} else {
					time.Sleep(time.Second)
				}
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("stream discard still waiting")
				}
				cancel()
			}
		},
	)
}

type auditNilStreamExecutor struct{ ProviderExecutor }

func (auditNilStreamExecutor) ExecuteStream(
	context.Context,
	*Auth,
	executor.Request,
	executor.Options,
) (*executor.StreamResult, error) {
	return nil, nil
}

func TestAuditLocalStreamBootstrapRejectsMissingSource(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	result, err := manager.executeStreamWithBootstrap(
		t.Context(), &Auth{ID: "synthetic", Provider: "synthetic"}, auditNilStreamExecutor{}, "synthetic",
		executor.Request{Model: "synthetic"}, executor.Options{}, "synthetic", OAuthModelAliasResult{},
	)
	if err == nil || result != nil {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestReadStreamBootstrapPreservesFirstDeliverableBoundary(t *testing.T) {
	failure := &Error{Code: "synthetic_bootstrap_failure", Message: "synthetic bootstrap failure"}
	payload := []byte("data: {\"text\":\"first\"}\n\n")
	metadata := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	for _, test := range []struct {
		name      string
		chunks    []executor.StreamChunk
		buffered  int
		remaining int
		closed    bool
		failed    bool
	}{
		{
			name: "empty-before-payload", chunks: []executor.StreamChunk{{}, {}, {Payload: payload}, {Err: failure}},
			buffered: 3, remaining: 1,
		},
		{
			name: "metadata-is-deliverable", chunks: []executor.StreamChunk{{Payload: metadata}, {Payload: payload}},
			buffered: 1, remaining: 1,
		},
		{
			name: "empty-then-error", chunks: []executor.StreamChunk{{}, {Err: failure}, {Payload: payload}},
			failed: true, remaining: 1,
		},
		{
			name: "error-precedes-same-chunk-payload", chunks: []executor.StreamChunk{{Payload: payload, Err: failure}},
			failed: true,
		},
		{name: "only-empty-chunks", chunks: []executor.StreamChunk{{}, {}}, buffered: 2, closed: true},
		{name: "closed-source", closed: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				chunks := make(chan executor.StreamChunk, len(test.chunks))
				for _, chunk := range test.chunks {
					chunks <- chunk
				}
				close(chunks)
				buffered, closed, err := readStreamBootstrap(t.Context(), chunks)
				if len(buffered) != test.buffered || closed != test.closed || len(chunks) != test.remaining ||
					(test.failed && err != failure) || (!test.failed && err != nil) {
					t.Fatalf("buffered=%d closed=%v remaining=%d error=%v", len(buffered), closed, len(chunks), err)
				}
				for i, chunk := range buffered {
					if string(chunk.Payload) != string(test.chunks[i].Payload) || chunk.Err != nil {
						t.Fatalf("buffered chunk %d changed: %+v", i, chunk)
					}
				}
			},
		)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if buffered, closed, err := readStreamBootstrap(
		ctx, make(chan executor.StreamChunk),
	); len(buffered) != 0 || closed || err != context.Canceled {
		t.Fatalf("canceled bootstrap: buffered=%d closed=%v error=%v", len(buffered), closed, err)
	}
}

func TestAuditHomeSessionLocksAreSerializedAndReclaimed(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	var workers sync.WaitGroup
	var active atomic.Int32
	for range 64 {
		workers.Go(
			func() {
				release := manager.lockHomeSession("same-session")
				if active.Add(1) != 1 {
					t.Error("same session executed concurrently")
				}
				active.Add(-1)
				release()
			},
		)
	}
	workers.Wait()
	manager.homeSessionLocks.Range(
		func(key, value any) bool {
			t.Errorf(
				"session lock retained: %v", key,
			)
			return true
		},
	)
	for range 64 {
		manager.lockHomeSession("independent-session")()
	}
	manager.homeSessionLocks.Range(
		func(key, value any) bool {
			t.Errorf(
				"completed session lock retained: %v", key,
			)
			return true
		},
	)
}

func TestAuditHomeUnavailableErrorStatus(t *testing.T) {
	for _, code := range []string{
		"auth_not_found", "auth_unavailable", "refresh_temporarily_unavailable", "home_unavailable",
	} {
		err := decodeHomeDispatchError([]byte(`{"error":{"type":"` + code + `","message":"synthetic unavailable"}}`))
		status, ok := err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != http.StatusServiceUnavailable {
			t.Fatalf("%s = %v", code, err)
		}
	}
}

func TestAuditExplicitCredentialKindAndExternalIndex(t *testing.T) {
	credential := &Auth{
		ID: "synthetic", Provider: "claude", Index: "assigned-by-owner",
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, AttributeAPIKey: "legacy-key"},
		Metadata:   map[string]any{"access_token": "synthetic"},
	}
	if kind, identity := credential.AccountInfo(); kind != "oauth" || identity != "" {
		t.Fatalf("OAuth account=%s/%s", kind, identity)
	}
	credential.Attributes[AttributeAuthKind] = AuthKindAPIKey
	credential.Metadata["email"] = "synthetic@example.invalid"
	if kind, identity := credential.AccountInfo(); kind != "api_key" || identity != "legacy-key" {
		t.Fatalf("API-key account=%s/%s", kind, identity)
	}
	manager := NewManager(nil, nil, nil)
	if _, err := manager.Register(t.Context(), credential); err != nil {
		t.Fatal(err)
	}
	current, ok := manager.GetByID(credential.ID)
	if !ok || current.Index != "assigned-by-owner" {
		t.Fatalf("external index replaced: %+v", current)
	}
}

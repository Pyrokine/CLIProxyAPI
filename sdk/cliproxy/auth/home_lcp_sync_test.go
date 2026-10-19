package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type homeLCPRecordingDispatcher struct {
	roundsHomeDispatcher
	sessionIDs []string
	headers    []http.Header
}

func (d *homeLCPRecordingDispatcher) RPopAuthWithConstraints(
	ctx context.Context, model, sessionID string, headers http.Header, count int, constraints home.AuthDispatchConstraints,
) ([]byte, error) {
	d.sessionIDs = append(d.sessionIDs, sessionID)
	d.headers = append(d.headers, headers.Clone())
	return d.roundsHomeDispatcher.RPopAuthWithConstraints(ctx, model, sessionID, headers, count, constraints)
}

func TestHomeDispatchCarriesLCPCompactionParentAndNodeKind(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Hour})
	manager.SetSelector(selector)
	t.Cleanup(selector.Stop)
	dispatcher := &homeLCPRecordingDispatcher{}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(&roundExecutor{})
	request := func(messages string) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			OriginalRequest: []byte(`{"messages":[` + messages + `]}`),
			SourceFormat:    sdktranslator.FormatOpenAI,
			Metadata: map[string]any{
				cliproxyexecutor.CallerScopeMetadataKey:    "home-test-caller",
				cliproxyexecutor.RequestedModelMetadataKey: "test-model",
			},
		}
	}
	dispatch := func(opts cliproxyexecutor.Options) string {
		t.Helper()
		selection, err := manager.pickHomeDispatchSelection(context.Background(), "test-model", opts)
		if err != nil || selection == nil {
			t.Fatalf("Home dispatch selection = %+v, error = %v", selection, err)
		}
		selection.End("test completed")
		return dispatcher.sessionIDs[len(dispatcher.sessionIDs)-1]
	}

	parent := request(`{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"},{"role":"user","content":"four"},{"role":"user","content":"five"}`)
	parentID := dispatch(parent)
	if parentID == "" || dispatcher.constraints[0].ParentSessionID != "" {
		t.Fatalf("root dispatch = %q, constraints = %+v", parentID, dispatcher.constraints[0])
	}
	selector.RecordResult("test", "test-model", parent, "home-a", true)

	compacted := request(`{"role":"user","content":"summary"},{"role":"user","content":"four"},{"role":"user","content":"five"},{"role":"user","content":"six"}`)
	childID := dispatch(compacted)
	if childID == parentID || dispatcher.constraints[1].ParentSessionID != parentID ||
		dispatcher.headers[1].Get("X-Node-Kind") != "compaction" ||
		!metadataBool(compacted.Metadata, cliproxyexecutor.IsCompactionMetadataKey) {
		t.Fatalf("compacted dispatch: parent=%q, child=%q, constraints=%+v, headers=%v, metadata=%v",
			parentID, childID, dispatcher.constraints[1], dispatcher.headers[1], compacted.Metadata)
	}
	if got := sessionMetadataString(compacted.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey); got != childID {
		t.Fatalf("canonical session = %q, want %q", got, childID)
	}
	selector.RecordResult("test", "test-model", compacted, "home-a", true)
	continued := request(`{"role":"user","content":"summary"},{"role":"user","content":"four"},{"role":"user","content":"five"},{"role":"user","content":"six"},{"role":"user","content":"seven"}`)
	selected, err := selector.Pick(context.Background(), "openai", "test-model", continued, []*Auth{{ID: "home-a"}})
	if err != nil || selected == nil || selected.ID != "home-a" ||
		sessionMetadataString(continued.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey) != childID {
		t.Fatalf("continuation after Home result = %+v, error = %v, metadata = %v", selected, err, continued.Metadata)
	}
}

func homeLCPOptions(messages string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.CallerScopeMetadataKey: "home-test-caller",
		},
		OriginalRequest: []byte(`{"messages":[` + messages + `]}`),
	}
}

func TestHomeExecuteBindsLCPToDispatchedAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Hour})
	manager.SetSelector(selector)
	t.Cleanup(selector.Stop)
	dispatcher := &homeLCPRecordingDispatcher{}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(&roundExecutor{})

	request := homeLCPOptions
	parent := request(`{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"},{"role":"user","content":"four"},{"role":"user","content":"five"}`)
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "test-model", Payload: parent.OriginalRequest}, parent); err != nil {
		t.Fatalf("Home parent execution: %v", err)
	}
	continued := request(`{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"},{"role":"user","content":"four"},{"role":"user","content":"five"},{"role":"user","content":"six"}`)
	namespace := lcpAffinityNamespace("openai", "test-model", continued.Metadata)
	fingerprints, minimum, tails, envDigest := selector.matcher.PrepareExt(
		cliproxysession.ExtractCanonicalTurns(continued.SourceFormat, continued.OriginalRequest),
	)
	matched, ok := selector.matcher.MatchFingerprintsWithContext(namespace, fingerprints, tails, envDigest, minimum)
	if !ok || matched.AuthID != "home-a" {
		t.Fatalf("continuation after Home execution = %+v, matched = %t", matched, ok)
	}
	parentID := dispatcher.sessionIDs[0]
	if matched.SessionID != parentID {
		t.Fatalf("Home parent session = %q, LCP session = %q", parentID, matched.SessionID)
	}

	compacted := request(`{"role":"user","content":"summary"},{"role":"user","content":"four"},{"role":"user","content":"five"},{"role":"user","content":"six"}`)
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "test-model", Payload: compacted.OriginalRequest}, compacted); err != nil {
		t.Fatalf("Home compaction execution: %v", err)
	}
	childID := dispatcher.sessionIDs[1]
	if childID == parentID || dispatcher.constraints[1].ParentSessionID != parentID || dispatcher.headers[1].Get("X-Node-Kind") != "compaction" {
		t.Fatalf("Home compaction: child = %q, parent = %q, constraints = %+v, headers = %v",
			childID, parentID, dispatcher.constraints[1], dispatcher.headers[1])
	}
	childContinued := request(`{"role":"user","content":"summary"},{"role":"user","content":"four"},{"role":"user","content":"five"},{"role":"user","content":"six"},{"role":"user","content":"seven"}`)
	selected, err := selector.Pick(context.Background(), "openai", "test-model", childContinued, []*Auth{{ID: "home-a"}})
	if err != nil || selected == nil || selected.ID != "home-a" ||
		sessionMetadataString(childContinued.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey) != childID {
		t.Fatalf("continuation after compaction = %+v, error = %v, metadata = %v", selected, err, childContinued.Metadata)
	}

	reselected := homeLCPOptions(`{"role":"user","content":"summary"},{"role":"user","content":"four"},{"role":"user","content":"five"},{"role":"user","content":"six"},{"role":"user","content":"seven"}`)
	reselected.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = "home-b"
	if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "test-model", Payload: reselected.OriginalRequest}, reselected); err != nil {
		t.Fatalf("Home credential change: %v", err)
	}
	if authID, state := selector.LookupAffinity("openai", "test-model", childID); authID != "home-b" || state != "bound" {
		t.Fatalf("Home credential change affinity = (%q, %q), constraints = %+v", authID, state, dispatcher.constraints[2])
	}
}

func TestFailedHomeDispatchDoesNotBindPendingLCPAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Hour})
	manager.SetSelector(selector)
	t.Cleanup(selector.Stop)
	dispatcher := &homeLCPRecordingDispatcher{roundsHomeDispatcher: roundsHomeDispatcher{
		errorPayload: []byte(`{"error":{"type":"auth_unavailable","message":"no auth"}}`),
	}}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(&roundExecutor{})
	opts := homeLCPOptions(`{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"}`)
	selection, err := manager.pickHomeDispatchSelection(context.Background(), "test-model", opts)
	if err == nil || selection != nil || len(dispatcher.sessionIDs) != 1 {
		t.Fatalf("Home dispatch = %+v, error = %v, sessions = %v", selection, err, dispatcher.sessionIDs)
	}
	if authID, state := selector.LookupAffinity("openai", "test-model", dispatcher.sessionIDs[0]); authID != "" || state != "unbound" {
		t.Fatalf("failed dispatch LCP affinity = (%q, %q)", authID, state)
	}
}

type homeLCPStreamExecutor struct{ schedulerTestExecutor }

func (homeLCPStreamExecutor) ExecuteStream(
	_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"output":"ok"}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestHomeCountAndStreamRecordLCPAffinity(t *testing.T) {
	for _, mode := range []string{"count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Hour})
			manager.SetSelector(selector)
			t.Cleanup(selector.Stop)
			dispatcher := &homeLCPRecordingDispatcher{}
			manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
			manager.RegisterExecutor(homeLCPStreamExecutor{})
			opts := homeLCPOptions(`{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"}`)
			req := cliproxyexecutor.Request{Model: "test-model", Payload: opts.OriginalRequest}
			if mode == "stream" {
				opts.Stream = true
				result, err := manager.ExecuteStream(context.Background(), []string{"test"}, req, opts)
				if err != nil || result == nil {
					t.Fatalf("Home stream = %+v, error = %v", result, err)
				}
				for range result.Chunks {
				}
			} else if _, err := manager.ExecuteCount(context.Background(), []string{"test"}, req, opts); err != nil {
				t.Fatalf("Home count tokens: %v", err)
			}
			if authID, state := selector.LookupAffinity("openai", "test-model", dispatcher.sessionIDs[0]); authID != "home-a" || state != "bound" {
				t.Fatalf("Home %s LCP affinity = (%q, %q)", mode, authID, state)
			}
		})
	}
}

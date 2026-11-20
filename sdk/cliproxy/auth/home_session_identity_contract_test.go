package auth

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestHomeSourceFormatCasePreservesLCPNamespace(t *testing.T) {
	for _, tc := range []struct {
		format   sdktranslator.Format
		provider string
		payload  string
	}{
		{
			sdktranslator.FormatGemini, "google",
			`{"contents":[{"role":"user","parts":[{"text":"one"}]},{"role":"model",` +
				`"parts":[{"text":"two"}]},{"role":"user","parts":[{"text":"three"}]}]}`,
		},
		{
			sdktranslator.FormatAntigravity, "google",
			`{"request":{"contents":[{"role":"user","parts":[{"text":"one"}]},{"rol` +
				`e":"model","parts":[{"text":"two"}]},{"role":"user","parts":[{"text":"` +
				`three"}]}]}}`,
		},
		{
			sdktranslator.FormatClaude, "claude",
			`{"messages":[{"role":"user","content":"one"},{"role":"assistant",` +
				`"content":"two"},{"role":"user","content":` +
				`"three"}]}`,
		},
		{
			sdktranslator.FormatInteractions, "devin",
			`{"input":[{"type":"user_input","content":"one"},{"type":"user_input","` +
				`content":"two"},{"type":"user_input","content":"three"}]}`,
		},
		{
			sdktranslator.FormatOpenAI, "openai",
			`{"messages":[{"role":"user","content":"one"},{"role":"assistant",` +
				`"content":"two"},{"role":"user","content":` +
				`"three"}]}`,
		},
		{
			sdktranslator.FormatOpenAIResponse, "openai",
			`{"input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"assistant",` +
				`"content":"two"},{"type":"message","role":"user","content":"three"}]}`,
		},
		{
			sdktranslator.FormatCodex, "openai",
			`{"input":[{"type":"message","role":"user","content":"one"},{"type":"message","role":"assistant",` +
				`"content":"two"},{"type":"message","role":"user","content":"three"}]}`,
		},
	} {
		t.Run(
			tc.format.String(), func(t *testing.T) {
				hook := &resultCaptureHook{}
				manager := NewManager(nil, nil, hook)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Hour})
				manager.SetSelector(selector)
				t.Cleanup(selector.Stop)
				manager.RegisterExecutor(&replaceAwareExecutor{id: tc.provider})
				payload, err := json.Marshal(
					homeAuthDispatchResponse{
						Auth: Auth{
							ID: "format-case-auth", Provider: tc.provider, Status: StatusActive,
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				manager.PublishHomeDispatch(
					&fixtureHomeDispatcher{payload: payload},
					executionregistry.New(),
					1,
				)
				var canonical string
				for _, format := range []string{
					tc.format.String(), strings.ToUpper(tc.format.String()),
					strings.ToUpper(tc.format.String()[:1]) + tc.format.String()[1:],
				} {
					t.Run(
						format, func(t *testing.T) {
							opts := cliproxyexecutor.Options{
								SourceFormat: sdktranslator.Format(format), OriginalRequest: []byte(tc.payload),
								Metadata: map[string]any{
									cliproxyexecutor.CallerScopeMetadataKey:    "format-case-caller",
									cliproxyexecutor.RequestedModelMetadataKey: "case-model",
								},
							}
							if _, err := manager.Execute(
								t.Context(), []string{tc.provider},
								cliproxyexecutor.Request{Model: "case-model", Payload: opts.OriginalRequest}, opts,
							); err != nil {
								t.Fatalf("Home execution: %v", err)
							}
							results := hook.Results()
							if len(results) == 0 {
								t.Fatal("successful execution did not report its result")
							}
							opts = results[len(results)-1].Options
							sessionID := sessionMetadataString(
								opts.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey,
							)
							if sessionID == "" || !strings.HasPrefix(sessionID, "lcp:v1:") {
								t.Fatalf("canonical session=%q, want LCP identity", sessionID)
							}
							if canonical == "" {
								canonical = sessionID
							}
							if sessionID != canonical || sessionMetadataString(
								opts.Metadata, cliproxyexecutor.SessionAffinityProviderMetadataKey,
							) != tc.provider {
								t.Fatalf(
									"format=%q session=%q want=%q provider=%v want=%q",
									format,
									sessionID,
									canonical,
									opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey],
									tc.provider,
								)
							}
							if authID, state := selector.LookupAffinity(
								tc.provider, "case-model", canonical,
							); authID != "format-case-auth" || state != "bound" {
								t.Fatalf("executed session affinity=(%q,%q)", authID, state)
							}
						},
					)
				}
			},
		)
	}
}

func TestHomeHierarchyRequiresNonemptySharedNamespace(t *testing.T) {
	for _, tc := range []struct {
		primary, parent string
		want            bool
	}{
		{":child", ":parent", false},
		{"::child", "::parent", false},
		{":", ":parent", false},
		{":child", "parent", false},
		{"child", ":parent", false},
		{"child", "parent", true},
		{"session:child", "session:parent", true},
		{"claude:root:agent:worker", "claude:root", true},
		{"lcp:v1:child", "lcp:v1:parent", true},
		{"conv:child", "pck:parent", false},
		{"session:same", "session:same", false},
		{"", "session:parent", false},
		{"session:child", "", false},
	} {
		t.Run(
			tc.primary+"/"+tc.parent, func(t *testing.T) {
				if got := isHierarchyParent(tc.primary, tc.parent); got != tc.want {
					t.Fatalf("hierarchy(%q,%q)=%t, want %t", tc.primary, tc.parent, got, tc.want)
				}
				if got := isSubagentSession(tc.primary, tc.parent); got != tc.want {
					t.Fatalf("subagent(%q,%q)=%t, want %t", tc.primary, tc.parent, got, tc.want)
				}
			},
		)
	}
}

func TestHomeLeadingColonSessionIDsKeepIndependentOwners(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	provider := &retainingHomeExecutionExecutor{}
	manager.RegisterExecutor(provider)
	dispatcher := &fixtureHomeDispatcher{}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	t.Cleanup(func() { manager.CloseExecutionSession(CloseAllExecutionSessionsID) })
	sessions := []string{":session-1", ":session-2"}
	for _, sessionID := range sessions {
		payload, err := json.Marshal(
			homeAuthDispatchResponse{
				Auth: Auth{
					ID: "home-session-alias-auth", Provider: provider.Identifier(), Status: StatusActive,
					Attributes: map[string]string{"websockets": "true"},
					Metadata:   map[string]any{"session_marker": sessionID},
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		dispatcher.payloads = append(dispatcher.payloads, payload)
	}
	owners := make(map[string]*HomeDispatchSelection)
	closed := make(map[string]*atomic.Int32)
	for _, sessionID := range sessions {
		opts := cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey:   sessionID,
				cliproxyexecutor.CanonicalSessionIDMetadataKey: sessionID,
			},
		}
		if _, err := manager.Execute(
			cliproxyexecutor.WithDownstreamWebsocket(t.Context()), []string{provider.Identifier()},
			cliproxyexecutor.Request{Model: "test-model"}, opts,
		); err != nil {
			t.Fatalf("session=%q execution: %v", sessionID, err)
		}
		snapshot, found := manager.GetExecutionSessionAuthByID(sessionID, "home-session-alias-auth")
		if !found || snapshot.Metadata["session_marker"] != sessionID || snapshot.homeRuntimeOwner == nil {
			t.Fatalf("session=%q snapshot=%+v found=%t", sessionID, snapshot, found)
		}
		owners[sessionID] = snapshot.homeRuntimeOwner
		counter := &atomic.Int32{}
		closed[sessionID] = counter
		if err := owners[sessionID].Bind(func() error { counter.Add(1); return nil }); err != nil {
			t.Fatal(err)
		}
		if got := sessionMetadataString(
			opts.Metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey,
		); got != sessionID {
			t.Fatalf("session identity=%q, want %q", got, sessionID)
		}
	}
	if _, found := manager.GetByID("home-session-alias-auth"); found {
		t.Fatal("unscoped getter exposed Home session credential")
	}
	if owners[sessions[0]] == owners[sessions[1]] {
		t.Fatal("independent sessions share selection owner")
	}
	for index, sessionID := range sessions {
		manager.CloseExecutionSession(sessionID)
		owners[sessionID].End("duplicate_end")
		if _, found := manager.GetExecutionSessionAuthByID(
			sessionID, "home-session-alias-auth",
		); found || owners[sessionID].Active() {
			t.Fatalf("closed session %q retained credential or owner", sessionID)
		}
		if index == 0 {
			other, found := manager.GetExecutionSessionAuthByID(sessions[1], "home-session-alias-auth")
			if !found || other.Metadata["session_marker"] != sessions[1] || !owners[sessions[1]].Active() {
				t.Fatal("closing first session changed independent second session")
			}
		}
	}
	for _, sessionID := range sessions {
		if got := closed[sessionID].Load(); got != 1 {
			t.Fatalf("session=%q resource closes=%d, want 1", sessionID, got)
		}
	}
}

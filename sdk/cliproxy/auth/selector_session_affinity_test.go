package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type affinityLastSelector struct{}

func (affinityLastSelector) Pick(
	_ context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}
	return auths[len(auths)-1], nil
}

func TestSessionAffinitySubagentInheritanceCanBeDisabled(t *testing.T) {
	t.Parallel()

	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	for _, test := range []struct {
		name    string
		enabled *bool
		want    string
	}{
		{name: "default enabled", want: "a"},
		{name: "explicitly disabled", enabled: new(false), want: "b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback:         affinityLastSelector{},
				TTL:              time.Hour,
				SubagentAffinity: test.enabled,
			})
			defer selector.Stop()

			rootOptions := cliproxyexecutor.Options{
				OriginalRequest: []byte(`{"session_id":"root"}`),
				Metadata:        map[string]any{},
			}
			selectedRoot, errRoot := selector.Pick(context.Background(), "gemini", "model", rootOptions, auths[:1])
			if errRoot != nil || selectedRoot == nil || selectedRoot.ID != "a" {
				t.Fatalf("root Pick() = %#v, %v, want auth a", selectedRoot, errRoot)
			}
			childOptions := cliproxyexecutor.Options{
				OriginalRequest: []byte(`{"session_id":"child","parent_session_id":"root"}`),
				Metadata:        map[string]any{},
			}
			selected, err := selector.Pick(context.Background(), "gemini", "model", childOptions, auths)
			if err != nil {
				t.Fatalf("Pick() error = %v", err)
			}
			if selected == nil || selected.ID != test.want {
				t.Fatalf("Pick() = %#v, want auth %s", selected, test.want)
			}
			parentKey := "gemini::session:root::model"
			childKey := "gemini::session:child::model"
			if got, ok := selector.cache.Get(parentKey); !ok || got != "a" {
				t.Fatalf("parent binding = %q, %v, want auth a", got, ok)
			}
			if got, ok := selector.cache.Get(childKey); !ok || got != test.want {
				t.Fatalf("child binding = %q, %v, want auth %s", got, ok, test.want)
			}
		})
	}
}

func TestSessionAffinityForkInheritsWhenSubagentsDisabled(t *testing.T) {
	t.Parallel()

	disabled := false
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:         affinityLastSelector{},
		TTL:              time.Hour,
		SubagentAffinity: &disabled,
	})
	defer selector.Stop()
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	selector.Remember("codex", cliproxyexecutor.Options{
		Headers:  http.Header{"Session-Id": {"parent"}},
		Metadata: map[string]any{cliproxyexecutor.SessionAffinityModelMetadataKey: "model"},
	}, auths[0])

	forkOptions := cliproxyexecutor.Options{
		Headers: http.Header{
			"Session-Id": {"parent"},
			"Thread-Id":  {"fork"},
			"X-Codex-Turn-Metadata": {
				`{"session_id":"parent","thread_id":"fork","forked_from_thread_id":"parent"}`,
			},
		},
		Metadata: map[string]any{},
	}
	selected, err := selector.Pick(context.Background(), "codex", "model", forkOptions, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if selected == nil || selected.ID != "a" {
		t.Fatalf("fork Pick() = %#v, want parent auth a", selected)
	}
	if !metadataBool(forkOptions.Metadata, cliproxyexecutor.IsForkMetadataKey) {
		t.Fatal("fork metadata was not retained")
	}
}

func TestSessionAffinityLCPExtensionAndBranch(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: affinityLastSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	auths := []*Auth{{ID: "a"}, {ID: "b"}}
	baseMetadata := func() map[string]any {
		return map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller-scope"}
	}
	first := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"one"}]}`),
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        baseMetadata(),
	}
	selected, err := selector.Pick(context.Background(), "openai", "model", first, auths)
	if err != nil || selected == nil || selected.ID != "b" {
		t.Fatalf("first Pick() = %#v, %v, want auth b", selected, err)
	}
	firstSession := sessionMetadataString(first.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey)
	if firstSession == "" {
		t.Fatal("first request did not bind an LCP session")
	}

	grown := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"one"},{"role":"assistant","content":"answer"},{"role":"user","content":"two"}]}`),
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        baseMetadata(),
	}
	selected, err = selector.Pick(context.Background(), "openai", "model", grown, auths)
	if err != nil || selected == nil || selected.ID != "b" {
		t.Fatalf("grown Pick() = %#v, %v, want auth b", selected, err)
	}
	if got := sessionMetadataString(grown.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey); got != firstSession {
		t.Fatalf("linear session = %q, want %q", got, firstSession)
	}

	branch := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"one"},{"role":"assistant","content":"different"},{"role":"user","content":"branch"}]}`),
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        baseMetadata(),
	}
	selected, err = selector.Pick(context.Background(), "openai", "model", branch, auths)
	if err != nil || selected == nil || selected.ID != "b" {
		t.Fatalf("branch Pick() = %#v, %v, want inherited auth b", selected, err)
	}
	branchSession := sessionMetadataString(branch.Metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey)
	if branchSession == "" || branchSession == firstSession {
		t.Fatalf("branch session = %q, root = %q", branchSession, firstSession)
	}
	if parent := sessionMetadataString(branch.Metadata, cliproxyexecutor.ParentSessionIDMetadataKey); parent == "" {
		t.Fatal("branch parent lineage is empty")
	}
}

func TestSessionAffinityRecordResultDoesNotDeleteReboundExplicitSession(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: affinityLastSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()
	options := cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-ID": {"session"}},
		Metadata: map[string]any{
			cliproxyexecutor.SessionAffinityModelMetadataKey:    "model",
			cliproxyexecutor.SessionAffinityProviderMetadataKey: "gemini",
		},
	}
	selector.Remember("gemini", options, &Auth{ID: "old"})
	selector.Remember("gemini", options, &Auth{ID: "new"})
	selector.RecordResult("gemini", "model", options, "old", false)

	key := "gemini::header:session::model"
	if got, ok := selector.cache.Get(key); !ok || got != "new" {
		t.Fatalf("rebound binding = %q, %v, want new", got, ok)
	}
}

func TestSessionAffinityConcurrentStop(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	var done sync.WaitGroup
	for index := 0; index < 32; index++ {
		done.Add(1)
		go func() {
			defer done.Done()
			selector.Stop()
		}()
	}
	done.Wait()
}

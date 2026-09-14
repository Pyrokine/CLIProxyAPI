package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type c02CandidateRecorder struct {
	mu         sync.Mutex
	candidates []string
}

func (r *c02CandidateRecorder) Pick(
	_ context.Context,
	_, _ string,
	_ cliproxyexecutor.Options,
	candidates []*Auth,
) (*Auth, error) {
	r.mu.Lock()
	r.candidates = r.candidates[:0]
	for _, candidate := range candidates {
		r.candidates = append(r.candidates, candidate.ID)
	}
	r.mu.Unlock()
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates[0], nil
}

func (r *c02CandidateRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.candidates...)
}

type c02PolicyHomeDispatcher struct {
	mu       sync.Mutex
	auths    []Auth
	counts   []int
	policies []string
}

func (*c02PolicyHomeDispatcher) HeartbeatOK() bool { return true }

func (d *c02PolicyHomeDispatcher) RPopAuth(
	_ context.Context,
	_, _ string,
	_ http.Header,
	count int,
) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counts = append(d.counts, count)
	index := count - 1
	if index < 0 || index >= len(d.auths) {
		return nil, nil
	}
	return json.Marshal(homeAuthDispatchResponse{Provider: "codex", Auth: d.auths[index]})
}

func (d *c02PolicyHomeDispatcher) RPopAuthWithPolicy(
	ctx context.Context,
	model, sessionID string,
	headers http.Header,
	count int,
	policy string,
) ([]byte, error) {
	d.mu.Lock()
	d.policies = append(d.policies, policy)
	d.mu.Unlock()
	return d.RPopAuth(ctx, model, sessionID, headers, count)
}

func (d *c02PolicyHomeDispatcher) snapshot() ([]int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.counts...), append([]string(nil), d.policies...)
}

func TestSelectAuthByKindFiltersCandidatesBeforeSelector(t *testing.T) {
	recorder := &c02CandidateRecorder{}
	manager := NewManager(nil, recorder, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, credential := range []*Auth{
		{ID: "api-key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "key"}},
		{ID: "oauth", Provider: "codex", Metadata: map[string]any{"access_token": "token"}},
	} {
		if _, errRegister := manager.Register(context.Background(), credential); errRegister != nil {
			t.Fatalf("register %s: %v", credential.ID, errRegister)
		}
	}

	selected, errSelect := manager.SelectAuthByKind(
		context.Background(),
		"codex",
		"",
		AuthKindOAuth,
		cliproxyexecutor.Options{},
	)
	if errSelect != nil {
		t.Fatalf("SelectAuthByKind() error = %v", errSelect)
	}
	if selected == nil || selected.ID != "oauth" {
		t.Fatalf("selected = %#v, want oauth", selected)
	}
	candidates := recorder.snapshot()
	if len(candidates) != 1 || candidates[0] != "oauth" {
		t.Fatalf("selector candidates = %v, want [oauth]", candidates)
	}
}

func TestCodexAlphaSearchPolicyRejectsOrdinaryAPIKey(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(
		context.Background(), &Auth{
			ID:         "ordinary-api-key",
			Provider:   "codex",
			Attributes: map[string]string{AttributeAPIKey: "ordinary"},
		},
	); errRegister != nil {
		t.Fatalf("register ordinary API key: %v", errRegister)
	}

	selected, errSelect := manager.SelectAuthWithCredentialPolicy(
		context.Background(),
		"codex",
		"",
		CredentialPolicyCodexAlphaSearchV1,
		cliproxyexecutor.Options{},
	)
	if selected != nil {
		t.Fatalf("selected = %#v, want nil", selected)
	}
	var authErr *Error
	if !errors.As(errSelect, &authErr) || authErr.Code != "auth_not_found" {
		t.Fatalf("error = %#v, want auth_not_found", errSelect)
	}
}

func TestSelectHomeAuthWithCredentialPolicyTransportsAndValidatesPolicy(t *testing.T) {
	dispatcher := &c02PolicyHomeDispatcher{
		auths: []Auth{
			{
				ID:         "ordinary-api-key",
				Provider:   "codex",
				Attributes: map[string]string{AttributeAPIKey: "ordinary", "base_url": "https://ordinary.example.com"},
			},
			{
				ID:       "alpha-api-key",
				Provider: "codex",
				Attributes: map[string]string{
					AttributeAPIKey:           "alpha",
					AttributeCodexAlphaSearch: "true",
					"base_url":                "https://alpha.example.com",
				},
			},
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	manager.executors["codex"] = schedulerTestExecutor{}

	selection, errSelect := manager.SelectHomeAuthWithCredentialPolicy(
		context.Background(),
		"codex",
		"gpt-5.6-sol",
		CredentialPolicyCodexAlphaSearchV1,
		cliproxyexecutor.Options{},
	)
	if errSelect != nil {
		t.Fatalf("SelectHomeAuthWithCredentialPolicy() error = %v", errSelect)
	}
	selected := selection.CloneAuth()
	if selected == nil || selected.ID != "alpha-api-key" {
		t.Fatalf("selected = %#v, want alpha-api-key", selected)
	}
	counts, policies := dispatcher.snapshot()
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("dispatch counts = %v, want [1 2]", counts)
	}
	if len(policies) != 2 || policies[0] != CredentialPolicyCodexAlphaSearchV1 || policies[1] != CredentialPolicyCodexAlphaSearchV1 {
		t.Fatalf("dispatch policies = %v", policies)
	}
	selection.End("test_complete")
	if errDrain := registry.Drain(context.Background()); errDrain != nil {
		t.Fatalf("drain registry: %v", errDrain)
	}
}

func TestCodexAlphaSearchPolicyConcurrentSelection(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, credential := range []*Auth{
		{
			ID:       "alpha-a",
			Provider: "codex",
			Attributes: map[string]string{
				AttributeAPIKey:           "a",
				AttributeCodexAlphaSearch: "true",
			},
		},
		{
			ID:       "alpha-b",
			Provider: "codex",
			Attributes: map[string]string{
				AttributeAPIKey:           "b",
				AttributeCodexAlphaSearch: "true",
			},
		},
	} {
		if _, errRegister := manager.Register(context.Background(), credential); errRegister != nil {
			t.Fatalf("register %s: %v", credential.ID, errRegister)
		}
	}

	const workers = 32
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Go(func() {
			selected, errSelect := manager.SelectAuthWithCredentialPolicy(
				context.Background(),
				"codex",
				"",
				CredentialPolicyCodexAlphaSearchV1,
				cliproxyexecutor.Options{},
			)
			if errSelect != nil {
				errorsCh <- errSelect
				return
			}
			if selected == nil || (selected.ID != "alpha-a" && selected.ID != "alpha-b") {
				errorsCh <- &Error{Code: "unexpected_selection", Message: "unexpected selected auth"}
			}
		})
	}
	wait.Wait()
	close(errorsCh)
	for errSelect := range errorsCh {
		t.Errorf("concurrent selection error: %v", errSelect)
	}
}

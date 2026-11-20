package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditCreditsKV struct {
	home.KVClient
	raw   []byte
	found bool
	err   error
	key   string
}

func (*auditCreditsKV) Enabled() bool     { return true }
func (*auditCreditsKV) HeartbeatOK() bool { return true }
func (c *auditCreditsKV) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.key = key
	return c.raw, c.found, c.err
}

func TestAuditCreditsGetterRetainsLocalAndHomeAuthority(t *testing.T) {
	id := t.Name()
	defer antigravityCreditsHintByAuth.Delete(id)
	local := home.WithClientScope(t.Context(), home.ClientScope{Enabled: false})
	if hint, found, err := GetAntigravityCreditsHintRequired(
		local, id,
	); hint != (AntigravityCreditsHint{}) || found || err != nil {
		t.Fatalf("unknown local: hint=%+v found=%t err=%v", hint, found, err)
	}
	want := AntigravityCreditsHint{
		Known: true, CreditAmount: 1500, MinCreditAmount: 50,
		PaidTierID: "synthetic-tier", UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
	for _, available := range []bool{false, true} {
		want.Available = available
		SetAntigravityCreditsHint(id, want)
		hint, found, err := GetAntigravityCreditsHintRequired(local, " \t"+id+"\n")
		if err != nil || !found || hint != want {
			t.Fatalf("local=%+v found=%t err=%v, want %+v", hint, found, err, want)
		}
	}
	raw, errMarshal := json.Marshal(want)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	readFailure := errors.New("synthetic Home read failure")
	for _, sample := range []struct {
		name      string
		body      string
		found     bool
		wantFound bool
		want      AntigravityCreditsHint
		err       error
		malformed bool
	}{
		{
			name: "unavailable", body: `{"Known":true,"Available":false}`, found: true, wantFound: true,
			want: AntigravityCreditsHint{Known: true},
		},
		{
			name: "available", body: `{"Known":true,"Available":true}`, found: true, wantFound: true,
			want: AntigravityCreditsHint{Known: true, Available: true},
		},
		{name: "complete observation", body: string(raw), found: true, wantFound: true, want: want},
		{name: "unknown"},
		{name: "undiscovered", body: `{"Known":false,"Available":false}`, found: true, wantFound: true},
		{name: "read failure", err: readFailure},
		{name: "canceled read", err: context.Canceled},
		{name: "malformed observation", body: `{"Known":`, found: true, malformed: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				client := &auditCreditsKV{raw: []byte(sample.body), found: sample.found, err: sample.err}
				ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, Enabled: true})
				hint, found, err := GetAntigravityCreditsHintRequired(ctx, " \t"+id+"\n")
				if found != sample.wantFound || hint != sample.want {
					t.Fatalf(
						"Home=%+v found=%t err=%v, want %+v found=%t", hint, found, err, sample.want, sample.wantFound,
					)
				}
				if sample.malformed {
					if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
						t.Fatalf("malformed Home observation lost JSON error: %v", err)
					}
				} else if !errors.Is(err, sample.err) {
					t.Fatalf("Home error=%v, want %v", err, sample.err)
				}
				if client.key != "cpa:antigravity:credits-hint:"+id {
					t.Fatalf("key=%s", client.key)
				}
			},
		)
	}
	for _, id := range []string{"", " \t\n"} {
		client := &auditCreditsKV{err: readFailure}
		ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, Enabled: true})
		if hint, found, err := GetAntigravityCreditsHintRequired(
			ctx, id,
		); hint != (AntigravityCreditsHint{}) || found || err != nil || client.key != "" {
			t.Fatalf("empty identity: hint=%+v found=%t err=%v key=%q", hint, found, err, client.key)
		}
	}
	if hint, found, err := GetAntigravityCreditsHintRequired(
		home.WithClientScope(
			t.Context(), home.ClientScope{Enabled: true},
		), id,
	); hint != (AntigravityCreditsHint{}) || found || !errors.Is(err, home.ErrNotConnected) {
		t.Fatalf("unavailable Home: hint=%+v found=%t err=%v", hint, found, err)
	}
}

func TestAuditRawSessionCompatibilityAndCanonicalReporting(t *testing.T) {
	for _, sample := range []struct {
		headers    http.Header
		body, want string
	}{
		{http.Header{"Session-Id": {"synthetic"}}, `{}`, "synthetic"},
		{nil, `{"session_id":"session","prompt_cache_key":"cache"}`, "cache"},
		{nil, `{"request":{"session_id":"nested","prompt_cache_key":"cache"}}`, ""},
	} {
		if got := ExtractSessionID(sample.headers, []byte(sample.body), nil); got != sample.want {
			t.Fatalf("raw=%q want=%q", got, sample.want)
		}
		if got := CanonicalSessionID(sample.headers, []byte(sample.body), nil); got == "" {
			t.Fatal("canonical report identity missing")
		}
	}
}

func TestAuditCreditsCandidatesUseCredentialExecutorAndHomeGate(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerTestExecutor{})
	credential := &Auth{
		ID: t.Name(), Provider: "antigravity",
		Attributes: map[string]string{"compat_name": "synthetic-antigravity", "provider_key": "test"},
	}
	manager.mu.Lock()
	manager.auths[credential.ID] = credential
	manager.mu.Unlock()
	key := schedulerTestExecutor{}.Identifier()
	candidates := manager.findAllAntigravityCreditsCandidateAuths("claude-synthetic", executor.Options{})
	if len(candidates) != 1 || candidates[0].executor.Identifier() != key {
		t.Fatalf("candidates=%+v", candidates)
	}
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	if candidates := manager.findAllAntigravityCreditsCandidateAuths(
		"claude-synthetic", executor.Options{},
	); len(candidates) != 0 {
		t.Fatal("Home used local credits fallback")
	}
}

func TestAuditHomeSelectionExportsSessionLineageWithOwnedLifetime(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	manager.RegisterExecutor(schedulerTestExecutor{})
	payload, err := json.Marshal(
		homeAuthDispatchResponse{
			Model: "model",
			Auth:  Auth{ID: t.Name(), Provider: "test", Metadata: map[string]any{"access_token": "synthetic"}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.PublishHomeDispatch(&testHomeDispatcher{heartbeat: true, payload: payload}, executionregistry.New(), 1)
	selection, err := manager.SelectHomeAuthByKind(
		t.Context(), "test", "model", AuthKindOAuth,
		executor.Options{Headers: http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer selection.End("test_complete")
	if selection.CanonicalSessionID != "header:child" || selection.ParentSessionID != "header:parent" ||
		!selection.Active() {
		t.Fatalf(
			"session=%q parent=%q active=%t", selection.CanonicalSessionID, selection.ParentSessionID,
			selection.Active(),
		)
	}
}

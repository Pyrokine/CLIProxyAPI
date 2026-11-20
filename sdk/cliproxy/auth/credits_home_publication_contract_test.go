package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type creditsPublicationClient struct {
	auditCreditsKV
	writes    int
	values    map[string][]byte
	setErr    error
	beforeCAS func()
}

func (c *creditsPublicationClient) KVGet(ctx context.Context, key string) ([]byte, bool, error) {
	if value, found := c.values[key]; found {
		return append([]byte(nil), value...), true, nil
	}
	return c.auditCreditsKV.KVGet(ctx, key)
}

func (c *creditsPublicationClient) KVCompareAndSwap(
	ctx context.Context, key string, expected []byte, exists bool, value []byte, ttl time.Duration,
) (bool, error) {
	if c.beforeCAS != nil {
		c.beforeCAS()
		c.beforeCAS = nil
	}
	previous, found := c.values[key]
	if found != exists || !bytes.Equal(previous, expected) {
		return false, nil
	}
	return c.KVSet(ctx, key, value, home.KVSetOptions{EX: ttl})
}

func (c *creditsPublicationClient) KVSet(
	_ context.Context, key string, value []byte, opts home.KVSetOptions,
) (bool, error) {
	c.writes++
	if c.setErr != nil {
		return false, c.setErr
	}
	if opts.EX != 30*time.Minute {
		return false, errors.New("unexpected credits TTL")
	}
	if c.values == nil {
		c.values = make(map[string][]byte)
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func TestCreditsHomePublicationKeepsConcurrentWinner(t *testing.T) {
	credential := &Auth{ID: t.Name(), Provider: "antigravity"}
	key := "cpa:antigravity:credits-hint:" + credential.ID
	now := time.Now()
	newer := AntigravityCreditsHint{Known: true, Available: true, CreditAmount: 1500, UpdatedAt: now}
	raw, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	client := &creditsPublicationClient{values: make(map[string][]byte)}
	client.beforeCAS = func() { client.values[key] = raw }
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Client: client, Enabled: true})
	if credential.PublishAntigravityCreditsHintWithContext(
		ctx, AntigravityCreditsHint{
			Known: true, CreditAmount: 10, MinCreditAmount: 50, UpdatedAt: now.Add(-time.Second),
		},
	) {
		t.Fatal("publication overwrote a concurrent Home observation")
	}
	if !bytes.Equal(client.values[key], raw) || client.writes != 0 {
		t.Fatalf("concurrent observation changed: value=%s writes=%d", client.values[key], client.writes)
	}
	unsupported := home.WithClientScope(t.Context(), home.ClientScope{Client: &auditCreditsKV{}, Enabled: true})
	if credential.PublishAntigravityCreditsHintWithContext(unsupported, newer) {
		t.Fatal("Home without CAS reported a successful publication")
	}
}

func TestCreditsHomePublicationRejectsOlderObservation(t *testing.T) {
	client := &creditsPublicationClient{}
	ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Client: client, Enabled: true})
	credential := &Auth{ID: t.Name(), Provider: "antigravity"}
	now := time.Now()
	latest := AntigravityCreditsHint{
		Known: true, Available: true, CreditAmount: 1500, MinCreditAmount: 50, UpdatedAt: now,
	}
	if !credential.PublishAntigravityCreditsHintWithContext(ctx, latest) {
		t.Fatal("latest Home observation was rejected")
	}
	older := AntigravityCreditsHint{
		Known: true, CreditAmount: 10, MinCreditAmount: 50, UpdatedAt: now.Add(-time.Second),
	}
	if credential.PublishAntigravityCreditsHintWithContext(ctx, older) {
		t.Fatal("older Home observation replaced the current balance")
	}
	for _, prefix := range []string{"cpa:antigravity:credits-hint:", "cpa:antigravity:credits-balance:"} {
		var stored AntigravityCreditsHint
		if err := json.Unmarshal(
			client.values[prefix+credential.ID], &stored,
		); err != nil || stored.CreditAmount != 1500 {
			t.Fatalf("latest Home observation changed: %+v error=%v", stored, err)
		}
	}
}

func TestCreditsHomePublicationKeepsCredentialAndSelectionOwners(t *testing.T) {
	for _, name := range []string{
		"current-credential", "replaced-credential", "removed-credential",
		"active-selection", "ended-selection", "replaced-selection-token", "canceled-request", "write-error",
	} {
		t.Run(
			name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				client := &creditsPublicationClient{}
				ctx := home.WithClientScope(t.Context(), home.ClientScope{ID: t.Name(), Client: client, Enabled: true})
				var credential *Auth
				if name == "active-selection" || name == "ended-selection" || name == "replaced-selection-token" {
					provider := &replaceAwareExecutor{id: "antigravity"}
					manager.RegisterExecutor(provider)
					selection, _ := auditOwnedHomeSelection(t, manager, t.Name(), provider)
					credential = selection.CloneAuth()
					attempt, err := manager.prepareHomeAttempt(
						ctx, selection, credential,
						executor.Request{Model: "owner-model"}, executor.Options{}, "owner-model",
					)
					if err != nil {
						t.Fatal(err)
					}
					ctx = attempt.ctx
					if name == "ended-selection" {
						selection.End("test_finished")
					}
					if name == "replaced-selection-token" {
						replacement := credential.Clone()
						replacement.Metadata["access_token"] = "synthetic-replacement"
						selection.ReplaceAuth(replacement)
					}
				} else {
					var err error
					credential, err = manager.Register(
						WithSkipPersist(t.Context()), &Auth{
							ID: t.Name(), Provider: "antigravity",
							Metadata: map[string]any{"access_token": "synthetic-access"},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					if name == "replaced-credential" {
						replacement := credential.Clone()
						replacement.Metadata["access_token"] = "synthetic-replacement"
						if _, err := manager.Update(WithSkipPersist(t.Context()), replacement); err != nil {
							t.Fatal(err)
						}
					}
					if name == "removed-credential" {
						if err := manager.Remove(WithSkipPersist(t.Context()), credential.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if name == "canceled-request" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if name == "write-error" {
					client.setErr = errors.New("synthetic Home write unavailable")
				}
				defer antigravityCreditsHintByAuth.Delete(credential.ID)
				SetAntigravityCreditsHint(credential.ID, AntigravityCreditsHint{Known: true, Available: false})
				accepted := credential.PublishAntigravityCreditsHintWithContext(
					ctx, AntigravityCreditsHint{
						Known: true, Available: true, CreditAmount: 25000, MinCreditAmount: 50, PaidTierID: "tier-1",
					},
				)
				wantAccepted := name == "current-credential" || name == "active-selection"
				if accepted != wantAccepted {
					t.Fatalf("publication accepted=%t, want %t", accepted, wantAccepted)
				}
				wantWrites := 0
				if wantAccepted {
					wantWrites = 2
				}
				if name == "write-error" {
					wantWrites = 1
				}
				if client.writes != wantWrites {
					t.Fatalf("Home writes=%d, want %d", client.writes, wantWrites)
				}
				local, found := GetAntigravityCreditsHint(credential.ID)
				if !found || local.Available {
					t.Fatalf("Home publication changed local observation: %+v found=%t", local, found)
				}
				if wantAccepted {
					for _, prefix := range []string{
						"cpa:antigravity:credits-hint:", "cpa:antigravity:credits-balance:",
					} {
						var stored AntigravityCreditsHint
						if err := json.Unmarshal(client.values[prefix+credential.ID], &stored); err != nil ||
							stored.CreditAmount != 25000 || stored.MinCreditAmount != 50 || stored.UpdatedAt.IsZero() {
							t.Fatalf("Home value=%+v error=%v", stored, err)
						}
					}
				}
			},
		)
	}
}

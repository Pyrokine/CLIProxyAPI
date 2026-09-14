package home

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
)

func TestMembershipLifetimeProtocolTable(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*Client)
		wantArgs   func(*Client) []string
		wantLegacy bool
	}{
		{
			name: "stable membership includes instance id",
			wantArgs: func(client *Client) []string {
				return []string{redisChannelConfig, "7", client.membershipInstanceID()}
			},
		},
		{
			name:    "takeover eligible membership requests takeover",
			prepare: (*Client).markMembershipTakeoverEligible,
			wantArgs: func(client *Client) []string {
				return []string{redisChannelConfig, "7", "takeover", client.membershipInstanceID()}
			},
		},
		{
			name: "suppressed membership omits takeover",
			prepare: func(client *Client) {
				client.markMembershipTakeoverEligible()
				client.SuppressTakeover()
			},
			wantArgs: func(client *Client) []string {
				return []string{redisChannelConfig, "7", client.membershipInstanceID()}
			},
		},
		{
			name: "legacy membership omits takeover and instance id",
			prepare: func(client *Client) {
				client.markMembershipTakeoverEligible()
				client.EnableLegacyMembership()
			},
			wantArgs:   func(*Client) []string { return []string{redisChannelConfig, "7"} },
			wantLegacy: true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				client := New(config.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 6379})
				if errSet := client.SetLifecycleConfig(config.CredentialConcurrencyConfig{LifecycleConfigRevision: 7}); errSet != nil {
					t.Fatalf("SetLifecycleConfig() error = %v", errSet)
				}
				if test.prepare != nil {
					test.prepare(client)
				}
				args, _ := client.subscriptionParameters()
				if want := test.wantArgs(client); !reflect.DeepEqual(args, want) {
					t.Fatalf("subscriptionParameters() args = %#v, want %#v", args, want)
				}
				if client.LegacyMembership() != test.wantLegacy {
					t.Fatalf("LegacyMembership() = %v, want %v", client.LegacyMembership(), test.wantLegacy)
				}
				next := client.NewLifetime()
				if next.membershipInstanceID() != client.membershipInstanceID() {
					t.Fatalf(
						"NewLifetime() instance id = %q, want %q", next.membershipInstanceID(),
						client.membershipInstanceID(),
					)
				}
				if next.LegacyMembership() != test.wantLegacy {
					t.Fatalf("NewLifetime() legacy = %v, want %v", next.LegacyMembership(), test.wantLegacy)
				}
			},
		)
	}
}

func TestMembershipErrorClassificationTable(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantTakeover bool
		wantLegacy   bool
	}{
		{name: "takeover unavailable", err: errors.New("ERR membership_takeover_unavailable"), wantTakeover: true},
		{
			name: "legacy subscribe arity", err: errors.New("ERR wrong number of arguments for 'subscribe' command"),
			wantLegacy: true,
		},
		{
			name: "embedded takeover text is not trusted",
			err:  errors.New("proxy: membership_takeover_unavailable while retrying"),
		},
		{name: "unrelated transport error", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if got := IsMembershipTakeoverUnavailableError(test.err); got != test.wantTakeover {
					t.Fatalf("IsMembershipTakeoverUnavailableError() = %v, want %v", got, test.wantTakeover)
				}
				if got := IsLegacyMembershipProtocolError(test.err); got != test.wantLegacy {
					t.Fatalf("IsLegacyMembershipProtocolError() = %v, want %v", got, test.wantLegacy)
				}
			},
		)
	}
}

func TestReleaseFlusherFlushAndSenderReplacementRace(t *testing.T) {
	flusher := NewReleaseFlusher(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		flusher.Run(ctx)
	}()

	var mu sync.Mutex
	accepted := make(map[executionregistry.ReleaseGroup]int64)
	setSender := func() {
		flusher.SetSender(
			func(_ context.Context, frame concurrencyReleaseFrame) error {
				group := executionregistry.ReleaseGroup{CredentialID: frame.CredentialID, Model: frame.Model}
				mu.Lock()
				if frame.ReleaseSeq > accepted[group] {
					accepted[group] = frame.ReleaseSeq
				}
				mu.Unlock()
				return nil
			},
		)
	}
	setSender()

	groups := []executionregistry.ReleaseGroup{
		{CredentialID: "credential-a", Model: "model-a"},
		{CredentialID: "credential-b", Model: "model-b"},
	}
	var tickets []*executionregistry.ReleaseTicket
	var writers sync.WaitGroup
	for index := range 32 {
		writers.Add(1)
		go func(sequence int64) {
			defer writers.Done()
			group := groups[sequence%int64(len(groups))]
			if ticket := flusher.MarkDirty(group, sequence+1); ticket != nil {
				mu.Lock()
				tickets = append(tickets, ticket)
				mu.Unlock()
			}
			setSender()
		}(int64(index))
	}
	writers.Wait()

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), time.Second)
	defer cancelFlush()
	if errFlush := flusher.Flush(flushCtx); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	for _, ticket := range tickets {
		if errWait := ticket.Wait(flushCtx); errWait != nil {
			t.Fatalf("release ticket wait error = %v", errWait)
		}
	}
	mu.Lock()
	for _, group := range groups {
		if accepted[group] == 0 {
			t.Fatalf("group %#v was not released", group)
		}
	}
	mu.Unlock()
	cancel()
	<-done
}

func TestConcurrencyReleaseRejectedDuringTakeoverStateRace(t *testing.T) {
	client := New(config.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 6379})
	client.markMembershipTakeoverEligible()
	var wait sync.WaitGroup
	for range 64 {
		wait.Go(func() {
			_, errClient := client.concurrencyReleaseClient()
			if !errors.Is(errClient, ErrNotConnected) {
				t.Errorf("concurrencyReleaseClient() error = %v, want %v", errClient, ErrNotConnected)
			}
			_ = client.NewLifetime().membershipInstanceID()
		})
	}
	wait.Wait()
}

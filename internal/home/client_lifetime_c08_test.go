package home

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/redis/go-redis/v9"
)

func TestGetConfigSkipsSecondDialAfterClusterTransportFailure(t *testing.T) {
	client := New(config.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 1})
	var mu sync.Mutex
	dials := 0
	options := &redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: time.Second, MaxRetries: -1,
		DialerRetries: 1, ContextTimeoutEnabled: true,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			return nil, errors.New("test Home unavailable")
		},
	}
	client.cmdOptions = cloneRedisOptions(options)
	client.cmd = redis.NewClient(options)
	t.Cleanup(client.Close)
	if _, err := client.GetConfig(t.Context()); !errors.Is(err, errClusterDiscoveryTransport) {
		t.Fatalf("GetConfig error = %v, want cluster discovery transport error", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Fatalf("GetConfig dial attempts = %d, want 1", dials)
	}
}

func TestGetConfigContinuesAfterClusterDiscoveryResponseError(t *testing.T) {
	for _, test := range []struct{ name, response string }{
		{"protocol error", "-ERR cluster command unsupported\r\n"},
		{"response type error", ":1\r\n"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				client, commands := newC08RedisCommandClient(
					t, func(args []string) string {
						switch {
						case len(args) >= 2 && strings.EqualFold(args[0], "CLUSTER") && strings.EqualFold(
							args[1], "NODES",
						):
							return test.response
						case len(args) >= 2 && strings.EqualFold(args[0], "GET") && args[1] == redisKeyConfig:
							payload := "host: 127.0.0.1\n"
							return fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
						default:
							return "-ERR unexpected command\r\n"
						}
					},
				)
				client.mu.Lock()
				client.homeCfg.DisableClusterDiscovery = false
				client.mu.Unlock()
				if raw, err := client.GetConfig(t.Context()); err != nil || string(raw) != "host: 127.0.0.1\n" {
					t.Fatalf("GetConfig = %q, error = %v", raw, err)
				}
				if commands.count("CLUSTER", "NODES") != 1 || commands.count("GET", redisKeyConfig) != 1 {
					t.Fatal("cluster response error changed the original command order or counts")
				}
			},
		)
	}
}

func TestEnsureClientsWaitsForPreviousTargetClose(t *testing.T) {
	client := New(config.HomeConfig{Enabled: true, Host: "next.example.com", Port: 8327})
	t.Cleanup(client.Close)
	closing := make(chan struct{})
	client.closing = closing
	release := sync.OnceFunc(func() { close(closing) })
	defer release()
	done := make(chan error, 1)
	go func() { done <- client.ensureClients() }()
	select {
	case err := <-done:
		t.Fatalf("ensureClients returned before the old target closed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ensureClients did not continue after the old target closed")
	}
}

func TestReleaseFlusherSenderReplacementPreservesTicket(t *testing.T) {
	flusher := newReleaseFlusher(
		time.Hour, time.Hour, func(context.Context, concurrencyReleaseFrame) error {
			return errors.New("old Home unavailable")
		},
	)
	group := executionregistry.ReleaseGroup{CredentialID: "cred-1", Model: "gpt"}
	ticket := flusher.MarkDirty(group, 1)
	if ticket == nil || !flusher.flush(t.Context()) {
		t.Fatal("original release did not retain a ticket after sender failure")
	}
	flusher.SetSender(
		func(_ context.Context, frame concurrencyReleaseFrame) error {
			if frame.CredentialID != group.CredentialID || frame.Model != group.Model || frame.ReleaseSeq != 1 {
				t.Fatalf("replacement release frame = %+v", frame)
			}
			return nil
		},
	)
	if flusher.flush(t.Context()) {
		t.Fatal("replacement sender failed to acknowledge the original release")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ticket.Wait(ctx); err != nil {
		t.Fatalf("ticket did not survive sender replacement: %v", err)
	}
}

func TestConfigSubscriberUsesAppliedLifecycleRevisionAndRebuildsCommands(t *testing.T) {
	client := New(config.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 6379})
	client.cmd = redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(client.Close)
	if err := client.SetLifecycleConfig(
		config.CredentialConcurrencyConfig{
			LifecycleConfigRevision: 9, CPAHeartbeatTimeout: 4 * time.Second, CPACancelBound: 5 * time.Second,
		},
	); err != nil {
		t.Fatal(err)
	}
	args, timeout := client.subscriptionParameters()
	if !reflect.DeepEqual(args, []string{"config", "9", client.membershipInstanceID()}) ||
		timeout != 4*time.Second {
		t.Fatalf("subscription args=%v timeout=%s", args, timeout)
	}
	client.recoveryState.Store(uint32(recoveryStateSwitchingTakeover))
	args, _ = client.subscriptionParameters()
	if !reflect.DeepEqual(args, []string{"config", "9", "takeover", client.membershipInstanceID()}) {
		t.Fatalf("takeover subscription args=%v", args)
	}
	client.recoveryState.Store(uint32(recoveryStateStable))
	client.promoteSubscription()
	if client.cmd != nil {
		t.Fatal("bootstrap command client was retained after subscription")
	}
}

func TestRunConfigSubscriberLifetimePreservesTakeoverWhenFreshCommandProbeFails(t *testing.T) {
	const payload = "host: 127.0.0.1\n"
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			switch strings.ToUpper(args[0]) {
			case "HELLO":
				return "%1\r\n$5\r\nproto\r\n:3\r\n"
			case "GET":
				return fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
			case "SUBSCRIBE":
				return "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n"
			case "PING":
				return "-ERR fresh command probe failed\r\n"
			default:
				return "+OK\r\n"
			}
		},
	)
	lifecycle := config.CredentialConcurrencyConfig{LifecycleConfigRevision: 9}
	if err := client.SetLifecycleConfig(lifecycle); err != nil {
		t.Fatal(err)
	}
	ready := false
	err := client.RunConfigSubscriberLifetime(
		t.Context(),
		func([]byte) error { return nil },
		func() { ready = true },
	)
	if err == nil || !strings.Contains(err.Error(), "fresh command probe failed") || ready ||
		client.HeartbeatOK() {
		t.Fatalf("probe error=%v ready=%t heartbeat=%t", err, ready, client.HeartbeatOK())
	}
	client.mu.Lock()
	retained := client.cmd != nil || client.sub != nil
	client.mu.Unlock()
	if retained || recoveryState(client.recoveryState.Load()) != recoveryStateTakeoverEligible {
		t.Fatal("failed command probe retained clients or lost the acknowledged membership")
	}
	if commands.count("SUBSCRIBE", "config") != 1 {
		t.Fatal("command probe failure changed the original subscription count")
	}
	commands.mu.Lock()
	for _, args := range commands.commands {
		if strings.EqualFold(args[0], "SUBSCRIBE") && !reflect.DeepEqual(
			args[1:], []string{"config", "9", client.membershipInstanceID()},
		) {
			t.Errorf("initial subscription args=%v", args)
		}
	}
	commands.mu.Unlock()
	next := client.NewLifetime()
	t.Cleanup(next.Close)
	if err := next.SetLifecycleConfig(lifecycle); err != nil {
		t.Fatal(err)
	}
	if args, _ := next.subscriptionParameters(); !reflect.DeepEqual(
		args, []string{"config", "9", "takeover", client.membershipInstanceID()},
	) {
		t.Fatalf("replacement subscription args=%v", args)
	}
}

func TestRunConfigSubscriberLifetimeReturnsAfterHeartbeatLoss(t *testing.T) {
	const payload = "credential-concurrency:\n  lifecycle-config-revision: 1\n  cpa-heartbeat-timeout: 20ms\n"
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			switch strings.ToUpper(args[0]) {
			case "HELLO":
				return "%1\r\n$5\r\nproto\r\n:3\r\n"
			case "GET":
				return fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
			case "SUBSCRIBE":
				return "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n"
			default:
				return "+OK\r\n"
			}
		},
	)
	client.homeCfg.DisableClusterDiscovery = false
	client.clusterNodes = []clusterNode{{IP: "failover.example.com", Port: 8327}}
	client.recoveryState.Store(uint32(recoveryStateSwitchingTakeover))
	ready, cleared := false, false
	err := client.RunConfigSubscriberLifetime(
		t.Context(), func(raw []byte) error {
			parsed, err := config.ParseConfigBytes(raw)
			if err != nil {
				return err
			}
			return client.SetLifecycleConfig(parsed.CredentialConcurrency)
		}, func() {
			ready = true
			cleared = recoveryState(client.recoveryState.Load()) == recoveryStateStable
		},
	)
	if err == nil || !ready || !cleared || client.HeartbeatOK() {
		t.Fatalf("heartbeat error=%v ready=%t cleared=%t healthy=%t", err, ready, cleared, client.HeartbeatOK())
	}
	if addr, _ := client.addr(); addr != "failover.example.com:8327" ||
		recoveryState(client.recoveryState.Load()) != recoveryStateSwitchingTakeover {
		t.Fatalf("heartbeat failover addr=%s state=%d", addr, client.recoveryState.Load())
	}
	client.mu.Lock()
	retained := client.cmd != nil || client.sub != nil
	client.mu.Unlock()
	if retained || commands.count("GET", "config") != 1 || commands.count("SUBSCRIBE", "config") != 1 {
		t.Fatal("heartbeat loss retained clients or changed the original config/subscription counts")
	}
	commands.mu.Lock()
	defer commands.mu.Unlock()
	for _, args := range commands.commands {
		if strings.EqualFold(args[0], "SUBSCRIBE") && !reflect.DeepEqual(
			args[1:], []string{"config", "1", "takeover", client.membershipInstanceID()},
		) {
			t.Fatalf("takeover subscription args=%v", args)
		}
	}
}

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
				if errSet :=
					client.SetLifecycleConfig(config.CredentialConcurrencyConfig{LifecycleConfigRevision: 7}); errSet !=
					nil {
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
		{
			name:         "takeover unavailable",
			err:          errors.New("ERR membership_takeover_unavailable"),
			wantTakeover: true,
		},
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
		wait.Go(
			func() {
				_, errClient := client.concurrencyReleaseClient()
				if !errors.Is(errClient, ErrNotConnected) {
					t.Errorf("concurrencyReleaseClient() error = %v, want %v", errClient, ErrNotConnected)
				}
				_ = client.NewLifetime().membershipInstanceID()
			},
		)
	}
	wait.Wait()
}

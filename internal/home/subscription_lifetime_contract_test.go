package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHomeSubscriptionACKsKeepOriginalMultiChannelMatrix(t *testing.T) {
	const first = "*3\r\n$9\r\nsubscribe\r\n$5\r\nfirst\r\n:1\r\n"
	const second = "*3\r\n$9\r\nsubscribe\r\n$6\r\nsecond\r\n:2\r\n"
	for _, sample := range []struct {
		name, response string
		wantError      bool
	}{
		{"ordered final count", first + second, false},
		{"missing final ACK", first, true},
		{"wrong second channel", first + strings.Replace(second, "$6\r\nsecond", "$5\r\nother", 1), true},
		{"wrong second kind", first + strings.Replace(second, "$9\r\nsubscribe", "$11\r\nunsubscribe", 1), true},
		{"wrong second count", first + strings.Replace(second, ":2", ":1", 1), true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				client, _ := newC08RedisCommandClient(
					t, func(args []string) string {
						if strings.EqualFold(args[0], "SUBSCRIBE") {
							return sample.response
						}
						return "-ERR unknown command\r\n"
					},
				)
				pubsub := client.cmd.Subscribe(t.Context(), "first", "second")
				t.Cleanup(func() { _ = pubsub.Close() })
				err := receiveSubscriptionACKs(t.Context(), pubsub, 50*time.Millisecond, []string{"first", "second"})
				if (err != nil) != sample.wantError {
					t.Fatalf("subscription ACKs error=%v, wantError=%t", err, sample.wantError)
				}
			},
		)
	}
}

func TestHomeSubscriberRejectsInvalidACKBeforeCommandProbe(t *testing.T) {
	for name, ack := range map[string]string{
		"message":       "*3\r\n$7\r\nmessage\r\n$6\r\nconfig\r\n$2\r\n{}\r\n",
		"wrong channel": "*3\r\n$9\r\nsubscribe\r\n$5\r\nother\r\n:1\r\n",
		"wrong count":   "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:2\r\n",
	} {
		t.Run(
			name, func(t *testing.T) {
				client, commands := newC08RedisCommandClient(
					t, func(args []string) string {
						switch strings.ToUpper(args[0]) {
						case "GET":
							return "$2\r\n{}\r\n"
						case "SUBSCRIBE":
							return ack
						default:
							return "-ERR unknown command\r\n"
						}
					},
				)
				ready := false
				err := client.RunConfigSubscriberLifetime(
					t.Context(), func([]byte) error { return nil }, func() { ready = true },
				)
				if err == nil || ready || client.HeartbeatOK() {
					t.Fatalf("invalid ACK accepted: error=%v ready=%t heartbeat=%t", err, ready, client.HeartbeatOK())
				}
				commands.mu.Lock()
				defer commands.mu.Unlock()
				for _, command := range commands.commands {
					if strings.EqualFold(command[0], "PING") {
						t.Fatal("command pool was probed before a valid subscription ACK")
					}
				}
			},
		)
	}
}

func TestHomeSubscriberCancellationClosesPendingRead(t *testing.T) {
	for _, phase := range []string{"ACK", "ready"} {
		t.Run(
			phase, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				var scheduled sync.Once
				scheduleCancel := func() {
					scheduled.Do(
						func() {
							timer := time.AfterFunc(25*time.Millisecond, cancel)
							t.Cleanup(func() { timer.Stop() })
						},
					)
				}
				client, commands := newC08RedisCommandClient(
					t, func(args []string) string {
						switch strings.ToUpper(args[0]) {
						case "GET":
							return "$2\r\n{}\r\n"
						case "PING":
							return "+PONG\r\n"
						case "SUBSCRIBE":
							if phase == "ACK" {
								scheduleCancel()
								return ""
							}
							return "*3\r\n$9\r\nsubscribe\r\n$6\r\nconfig\r\n:1\r\n"
						default:
							return "-ERR unknown command\r\n"
						}
					},
				)
				client.testOperationTimeout = 2 * time.Second
				var configCalls, readyCalls atomic.Int32
				done := make(chan error, 1)
				go func() {
					done <- client.RunConfigSubscriberLifetime(
						ctx, func(raw []byte) error {
							configCalls.Add(1)
							if string(raw) != "{}" {
								return fmt.Errorf("unexpected config: %s", raw)
							}
							return nil
						}, func() {
							readyCalls.Add(1)
							scheduleCancel()
						},
					)
				}()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("subscriber error=%v, want context.Canceled", err)
					}
				case <-time.After(500 * time.Millisecond):
					t.Error("subscriber cancellation left a pending socket read")
					client.Close()
					<-done
				}
				wantReady := int32(0)
				if phase == "ready" {
					wantReady = 1
				}
				if configCalls.Load() != 1 || readyCalls.Load() != wantReady || client.HeartbeatOK() {
					t.Fatalf(
						"config=%d ready=%d heartbeat=%t", configCalls.Load(), readyCalls.Load(), client.HeartbeatOK(),
					)
				}
				if count := commands.count("SUBSCRIBE", "config"); count != 1 {
					t.Fatalf("legacy config subscriptions=%d, want 1", count)
				}
				commands.mu.Lock()
				defer commands.mu.Unlock()
				for _, command := range commands.commands {
					if strings.EqualFold(command[0], "SUBSCRIBE") && len(command) != 2 {
						t.Fatalf("legacy subscription changed: %v", command)
					}
				}
				client.mu.Lock()
				defer client.mu.Unlock()
				if client.cmd != nil || client.sub != nil || len(client.connections) != 0 ||
					!client.dispatchFenced.Load() {
					t.Fatal("unmanaged subscriber retained clients or accepted new dispatch after cancellation")
				}
			},
		)
	}
}

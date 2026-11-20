package api

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
)

func TestRedisPopRejectsOtherQueueNamesWithoutConsumingUsage(t *testing.T) {
	for _, command := range []string{"LPOP", "RPOP"} {
		for _, channel := range []string{"usage", "errors", "missing-queue"} {
			for _, withCount := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/%s/count=%t", command, channel, withCount), func(t *testing.T) {
						const password = "fixture-management-password"
						t.Setenv("MANAGEMENT_PASSWORD", password)
						queue := redisqueue.NewQueue()
						server := newTestServerWithOptions(t, WithUsageQueue(queue))
						queue.SetEnabled(true)
						client, peer := net.Pipe()
						done := make(chan struct{})
						go func() {
							defer close(done)
							server.handleRedisConnection(
								&redisTestConn{
									Conn: peer, remoteAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345},
								}, nil,
							)
						}()
						t.Cleanup(
							func() {
								_ = client.Close()
								queue.Close()
								select {
								case <-done:
								case <-time.After(2 * time.Second):
									t.Error("Redis handler did not stop")
								}
							},
						)
						if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
							t.Fatal(err)
						}
						reader := bufio.NewReader(client)
						if err := writeTestRESPCommand(client, "AUTH", password); err != nil {
							t.Fatal(err)
						}
						if response, err := readTestRESPSimpleString(reader); err != nil || response != "OK" {
							t.Fatalf("AUTH response=%q error=%v", response, err)
						}
						const marker = "fixture-usage-event"
						queue.Enqueue([]byte(marker))
						args := []string{command, channel}
						if withCount {
							args = append(args, "2")
						}
						if err := writeTestRESPCommand(client, args...); err != nil {
							t.Fatal(err)
						}
						prefix, err := reader.Peek(1)
						if err != nil {
							t.Fatal(err)
						}
						rejected := prefix[0] == '-'
						var response any
						switch prefix[0] {
						case '-':
							response, err = readTestRESPError(reader)
						case '$':
							response, err = readTestRESPBulkString(reader)
						case '*':
							response, err = readRESPArrayOfBulkStrings(reader)
						default:
							t.Fatalf("unexpected prefix %q", prefix)
						}
						if err != nil {
							t.Fatal(err)
						}
						remaining := queue.PopOldest(10)
						if channel == "usage" {
							if rejected || len(remaining) != 0 {
								t.Fatalf("valid usage pop failed: response=%q remaining=%q", response, remaining)
							}
						} else if !rejected || len(remaining) != 1 || string(remaining[0]) != marker {
							t.Fatalf("unsupported queue consumed usage: response=%q remaining=%q", response, remaining)
						}
					},
				)
			}
		}
	}
}

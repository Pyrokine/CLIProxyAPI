package home

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type c08RedisCommandLog struct {
	mu       sync.Mutex
	commands [][]string
}

func (l *c08RedisCommandLog) append(args []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.commands = append(l.commands, append([]string(nil), args...))
}

func (l *c08RedisCommandLog) last() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.commands) == 0 {
		return nil
	}
	return append([]string(nil), l.commands[len(l.commands)-1]...)
}

func (l *c08RedisCommandLog) count(commandName, key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, command := range l.commands {
		if len(command) >= 2 && strings.EqualFold(command[0], commandName) && command[1] == key {
			count++
		}
	}
	return count
}

func newC08RedisCommandClient(
	t *testing.T,
	handler func([]string) string,
) (*Client, *c08RedisCommandLog) {
	t.Helper()
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	commands := &c08RedisCommandLog{}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			go serveC08RedisCommandConn(conn, commands, handler)
		}
	}()
	t.Cleanup(
		func() {
			_ = listener.Close()
			<-serverDone
		},
	)

	host, portText, errSplit := net.SplitHostPort(listener.Addr().String())
	if errSplit != nil {
		t.Fatalf("split listener address: %v", errSplit)
	}
	port, errPort := strconv.Atoi(portText)
	if errPort != nil {
		t.Fatalf("parse listener port: %v", errPort)
	}
	client := New(
		config.HomeConfig{
			Enabled:                 true,
			Host:                    host,
			Port:                    port,
			DisableClusterDiscovery: true,
		},
	)
	client.testOperationTimeout = 50 * time.Millisecond
	options := &redis.Options{
		Addr:                  listener.Addr().String(),
		Protocol:              2,
		DisableIdentity:       true,
		DialTimeout:           50 * time.Millisecond,
		ReadTimeout:           50 * time.Millisecond,
		WriteTimeout:          50 * time.Millisecond,
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	}
	client.cmdOptions = cloneRedisOptions(options)
	client.cmd = redis.NewClient(options)
	t.Cleanup(client.Close)
	return client, commands
}

func serveC08RedisCommandConn(
	conn net.Conn,
	commands *c08RedisCommandLog,
	handler func([]string) string,
) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for {
		args, errRead := readC08RedisCommand(reader)
		if errRead != nil {
			return
		}
		commands.append(args)
		response := "+OK\r\n"
		if handler != nil {
			response = handler(args)
		}
		if _, errWrite := io.WriteString(conn, response); errWrite != nil {
			return
		}
	}
}

func readC08RedisCommand(reader *bufio.Reader) ([]string, error) {
	line, errRead := reader.ReadString('\n')
	if errRead != nil {
		return nil, errRead
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("expected RESP array, got %q", line)
	}
	count, errCount := strconv.Atoi(strings.TrimPrefix(line, "*"))
	if errCount != nil {
		return nil, errCount
	}
	args := make([]string, 0, count)
	for range count {
		bulkLine, errBulk := reader.ReadString('\n')
		if errBulk != nil {
			return nil, errBulk
		}
		bulkLine = strings.TrimSpace(bulkLine)
		if !strings.HasPrefix(bulkLine, "$") {
			return nil, fmt.Errorf("expected RESP bulk string, got %q", bulkLine)
		}
		size, errSize := strconv.Atoi(strings.TrimPrefix(bulkLine, "$"))
		if errSize != nil {
			return nil, errSize
		}
		payload := make([]byte, size+2)
		if _, errFull := io.ReadFull(reader, payload); errFull != nil {
			return nil, errFull
		}
		args = append(args, string(payload[:size]))
	}
	return args, nil
}

func TestC08KVCompareAndSwapUsesNativeCAS(t *testing.T) {
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			if len(args) > 0 && strings.EqualFold(args[0], "CAS") {
				return ":1\r\n"
			}
			return "-ERR unexpected command\r\n"
		},
	)

	swapped, errCAS := client.KVCompareAndSwap(
		context.Background(), "key", []byte("old"), true, []byte("new"), 1500*time.Millisecond,
	)
	if errCAS != nil || !swapped {
		t.Fatalf("KVCompareAndSwap() = (%t, %v), want true, nil", swapped, errCAS)
	}
	want := []string{"CAS", "key", "1", "old", "new", "PX", "1500"}
	if got := commands.last(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CAS command = %#v, want %#v", got, want)
	}
}

func TestC08KVCompareAndSwapOmitsPXWithoutTTL(t *testing.T) {
	for _, swapped := range []bool{false, true} {
		t.Run(
			strconv.FormatBool(swapped), func(t *testing.T) {
				client, commands := newC08RedisCommandClient(
					t, func(args []string) string {
						if len(args) > 0 && strings.EqualFold(args[0], "CAS") {
							if swapped {
								return ":1\r\n"
							}
							return ":0\r\n"
						}
						return "-ERR unexpected command\r\n"
					},
				)

				got, errCAS := client.KVCompareAndSwap(context.Background(), "key", nil, false, []byte("new"), 0)
				if errCAS != nil || got != swapped {
					t.Fatalf("KVCompareAndSwap() = (%t, %v), want %t, nil", got, errCAS, swapped)
				}
				want := []string{"CAS", "key", "0", "", "new"}
				if got := commands.last(); !reflect.DeepEqual(got, want) {
					t.Fatalf("CAS command = %#v, want %#v", got, want)
				}
			},
		)
	}
}

func TestKVCompareAndSwapReportsMismatch(t *testing.T) {
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			if len(args) > 0 && strings.EqualFold(args[0], "CAS") {
				return ":0\r\n"
			}
			return "-ERR unexpected command\r\n"
		},
	)

	swapped, errCAS := client.KVCompareAndSwap(
		context.Background(), "key", []byte("old"), true, []byte("new"), time.Minute,
	)
	if errCAS != nil || swapped {
		t.Fatalf("KVCompareAndSwap() = (%t, %v), want false, nil", swapped, errCAS)
	}
	want := []string{"CAS", "key", "1", "old", "new", "PX", "60000"}
	if got := commands.last(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CAS command = %#v, want %#v", got, want)
	}
}

func TestC08KVCompareAndSwapLatchesUnsupportedPerLifetime(t *testing.T) {
	logger := log.StandardLogger()
	oldLevel := logger.GetLevel()
	logger.SetLevel(log.WarnLevel)
	capture := &logtest.Hook{}
	oldHooks := logger.ReplaceHooks(log.LevelHooks{})
	logger.AddHook(capture)
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks); logger.SetLevel(oldLevel) })

	var upgraded atomic.Bool
	client, commands := newC08RedisCommandClient(
		t, func(args []string) string {
			if len(args) > 0 && strings.EqualFold(args[0], "CAS") {
				if upgraded.Load() {
					return ":1\r\n"
				}
				return "-ERR unknown command 'cas'\r\n"
			}
			return "-ERR unexpected command\r\n"
		},
	)

	for attempt := range 2 {
		_, errCAS := client.KVCompareAndSwap(
			context.Background(), "key", nil, false, []byte("new"), time.Minute,
		)
		if !errors.Is(errCAS, ErrCompareAndSwapUnsupported) {
			t.Fatalf("attempt %d error = %v, want ErrCompareAndSwapUnsupported", attempt+1, errCAS)
		}
	}
	if got := commands.count("CAS", "key"); got != 1 {
		t.Fatalf("CAS round trips = %d, want 1", got)
	}
	if !client.casUnsupported.Load() {
		t.Fatal("CAS unsupported latch was not set")
	}
	entries := capture.AllEntries()
	if len(entries) != 1 || entries[0].Level != log.WarnLevel ||
		!strings.Contains(entries[0].Message, "does not implement CAS") || len(entries[0].Data) != 0 {
		t.Fatalf("unsupported CAS diagnostics = %+v, want one warning without raw error fields", entries)
	}

	upgraded.Store(true)
	next := client.NewLifetime()
	t.Cleanup(next.Close)
	if next.casUnsupported.Load() {
		t.Fatal("CAS unsupported latch leaked into the next Home lifetime")
	}
	swapped, errCAS := next.KVCompareAndSwap(
		context.Background(), "key", nil, false, []byte("new"), time.Minute,
	)
	if errCAS != nil || !swapped || commands.count("CAS", "key") != 2 {
		t.Fatalf("new lifetime CAS = (%t, %v), commands=%d", swapped, errCAS, commands.count("CAS", "key"))
	}
	want := []string{"CAS", "key", "0", "", "new", "PX", "60000"}
	if got := commands.last(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new lifetime command = %#v, want %#v", got, want)
	}
	if _, err := client.KVCompareAndSwap(
		context.Background(), "key", nil, false, []byte("new"), time.Minute,
	); !errors.Is(err, ErrCompareAndSwapUnsupported) || commands.count("CAS", "key") != 2 {
		t.Fatalf("old lifetime changed its unsupported state: %v", err)
	}
	if len(capture.AllEntries()) != 1 {
		t.Fatal("unsupported warning was repeated after lifetime recovery")
	}
}

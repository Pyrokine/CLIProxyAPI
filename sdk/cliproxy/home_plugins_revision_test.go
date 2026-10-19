package cliproxy

import (
	"bufio"
	"fmt"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPrepareHomePluginsRunsAgainAfterAuthRevisionChanges(t *testing.T) {
	svc := &Service{}
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     t.TempDir(),
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}

	prepared, syncKey, didSync, errPrepare := svc.prepareHomePlugins(context.Background(), cfg)
	if errPrepare != nil || !didSync {
		t.Fatalf("first prepare = (%q, %t, %v)", syncKey, didSync, errPrepare)
	}
	if _, errApply := prepared.Apply(nil); errApply != nil {
		t.Fatalf("first Apply() error = %v", errApply)
	}
	svc.markHomePluginsSynced(syncKey)
	prepared, _, didSync, errPrepare = svc.prepareHomePlugins(context.Background(), cfg)
	if errPrepare != nil || didSync {
		t.Fatalf("unchanged prepare = (%t, %v), want skipped", didSync, errPrepare)
	}
	if errClose := prepared.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	cfg.Plugins.AuthRevision = 2
	prepared, nextKey, didSync, errPrepare := svc.prepareHomePlugins(context.Background(), cfg)
	if errPrepare != nil || !didSync {
		t.Fatalf("revision prepare = (%q, %t, %v)", nextKey, didSync, errPrepare)
	}
	defer func() { _ = prepared.Close() }()
	if nextKey == syncKey {
		t.Fatalf("sync key unchanged after auth revision: %q", syncKey)
	}
}

func TestHomePluginSyncKeyIncludesAuthRevision(t *testing.T) {
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     "plugins",
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}
	first := homePluginSyncKey(cfg)
	cfg.Plugins.AuthRevision = 2
	second := homePluginSyncKey(cfg)
	if first == second {
		t.Fatalf("homePluginSyncKey() unchanged after auth revision update: %q", first)
	}
}

func readHomeStatusCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 || line[0] != '*' {
		return nil, fmt.Errorf("invalid RESP array %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || count < 1 || count > 64 {
		return nil, fmt.Errorf("invalid RESP array length")
	}
	args := make([]string, 0, count)
	for range count {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) < 3 || line[0] != '$' {
			return nil, fmt.Errorf("invalid RESP bulk string")
		}
		length, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || length < 0 || length > 1<<20 {
			return nil, fmt.Errorf("invalid RESP bulk length")
		}
		value := make([]byte, length+2)
		if _, err := io.ReadFull(reader, value); err != nil {
			return nil, err
		}
		args = append(args, string(value[:length]))
	}
	return args, nil
}

func homePluginStatusClient(t *testing.T, statusResponse func(int32) string) (*home.Client, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	writes := &atomic.Int32{}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					args, err := readHomeStatusCommand(reader)
					if err != nil {
						return
					}
					response := "+OK\r\n"
					switch strings.ToUpper(args[0]) {
					case "HELLO":
						response = "%6\r\n$6\r\nserver\r\n$5\r\nredis\r\n$5\r\nproto\r\n:3\r\n$2\r\nid\r\n:1\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n$4\r\nrole\r\n$6\r\nmaster\r\n$7\r\nmodules\r\n*0\r\n"
					case "RPUSH":
						if len(args) > 1 && args[1] == "plugin-status" {
							response = statusResponse(writes.Add(1))
						}
					case "GET":
						response = "$-1\r\n"
					}
					if _, err := io.WriteString(conn, response); err != nil {
						return
					}
				}
			})
		}
	})
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client := home.New(config.HomeConfig{Enabled: true, Host: host, Port: port})
	t.Cleanup(func() { client.Close(); listener.Close(); workers.Wait() })
	return client, writes
}

func TestHomePluginStatusRejectionRemainsRetryable(t *testing.T) {
	client, writes := homePluginStatusClient(t, func(count int32) string {
		if count == 1 {
			return "-ERR blocked\r\n"
		}
		return ":1\r\n"
	})
	base := &config.Config{}
	base.Home.Enabled, base.Home.NodeID = true, "node-1"
	service := &Service{cfg: base}
	remote := &config.Config{}
	if err := service.applyHomeOverlayWithClient(t.Context(), remote, client); err == nil {
		t.Error("Home status rejection returned success")
	}
	if writes.Load() != 1 || service.homePluginSyncKey != "" {
		t.Fatalf("rejected status writes=%d sync=%q", writes.Load(), service.homePluginSyncKey)
	}
	if err := service.applyHomeOverlayWithClient(t.Context(), remote, client); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 2 || service.homePluginSyncKey == "" {
		t.Fatalf("retry writes=%d sync=%q", writes.Load(), service.homePluginSyncKey)
	}
}

func TestHomePluginLateStatusKeepsCurrentConfigurationSignature(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	client, _ := homePluginStatusClient(t, func(count int32) string {
		if count == 1 {
			close(started)
			<-release
		}
		return ":1\r\n"
	})
	base := &config.Config{}
	base.Home.Enabled, base.Home.NodeID = true, "node-1"
	service := &Service{cfg: base}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := &config.Config{}
	first.Plugins.AuthRevision = 1
	firstResult := make(chan error, 1)
	go func() { firstResult <- service.applyHomeOverlayWithClient(ctx, first, client) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := &config.Config{}
	second.Plugins.AuthRevision = 2
	secondClient, _ := homePluginStatusClient(t, func(int32) string { return ":1\r\n" })
	if err := service.applyHomeOverlayWithClient(ctx, second, secondClient); err != nil {
		t.Fatal(err)
	}
	service.cfgMu.RLock()
	want := homePluginSyncKey(service.cfg)
	service.cfgMu.RUnlock()
	once.Do(func() { close(release) })
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	service.homePluginSyncMu.Lock()
	got := service.homePluginSyncKey
	service.homePluginSyncMu.Unlock()
	if got != want {
		t.Fatalf("late status replaced current signature=%q, want %q", got, want)
	}
}

func TestHomePluginStatusRequiresClientAndNode(t *testing.T) {
	service := &Service{}
	if err := service.reportHomePluginStatusWithClient(context.Background(), &config.Config{}, homeplugins.SyncReport{}, nil); err == nil {
		t.Fatal("missing Home client returned success")
	}
	client, _ := homePluginStatusClient(t, func(int32) string { return ":1\r\n" })
	if err := service.reportHomePluginStatusWithClient(context.Background(), &config.Config{}, homeplugins.SyncReport{}, client); err == nil {
		t.Fatal("missing Home node returned success")
	}
}

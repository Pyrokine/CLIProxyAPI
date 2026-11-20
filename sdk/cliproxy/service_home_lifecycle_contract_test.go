package cliproxy

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

func homeLifecycleRedis(t *testing.T, handle func([]string) string) (config.HomeConfig, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	subscriptions := make(chan net.Conn, 8)
	var workers sync.WaitGroup
	var connections sync.Map
	workers.Go(
		func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				connections.Store(conn, true)
				workers.Go(
					func() {
						defer func(closeResource func() error) { _ = closeResource() }(conn.Close)
						defer connections.Delete(conn)
						reader := bufio.NewReader(conn)
						for {
							args, err := readHomeStatusCommand(reader)
							if err != nil {
								return
							}
							response := ""
							if handle != nil {
								response = handle(args)
							}
							if response == "" {
								switch strings.ToUpper(args[0]) {
								case "HELLO":
									response = "%1\r\n$5\r\nproto\r\n:3\r\n"
								case "PING":
									response = "+PONG\r\n"
								case "GET":
									response = "$-1\r\n"
								case "SUBSCRIBE":
									response = fmt.Sprintf(
										">3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(args[1]), args[1],
									)
									subscriptions <- conn
								case "RPUSH":
									response = ":1\r\n"
								default:
									response = "+OK\r\n"
								}
							}
							if _, err := io.WriteString(conn, response); err != nil {
								return
							}
						}
					},
				)
			}
		},
	)
	t.Cleanup(
		func() {
			_ = listener.Close()
			connections.Range(
				func(conn, _ any) bool {
					_ = conn.(net.Conn).Close()
					return true
				},
			)
			workers.Wait()
		},
	)
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return config.HomeConfig{
		Enabled: true, NodeID: "synthetic-home-node", Host: host, Port: port, DisableClusterDiscovery: true,
	}, subscriptions
}

func TestHomeRuntimePublicationClearsTemporaryStoreAuth(t *testing.T) {
	for _, withRule := range []bool{false, true} {
		t.Run(
			fmt.Sprint(withRule), func(t *testing.T) {
				base := &config.Config{Home: config.HomeConfig{Enabled: true, NodeID: "synthetic-home-node"}}
				service := &Service{cfg: base}
				remote := &config.Config{}
				if withRule {
					remote.Plugins.StoreAuth = []sdkpluginstore.AuthConfig{
						{
							Match: "https://downloads.example/", Type: sdkpluginstore.AuthTypeBearer,
							TokenEnv: "HOME_UNUSED_SYNTHETIC_TOKEN",
						},
					}
				}
				err := service.applyHomeOverlayWithClient(t.Context(), remote, nil)
				if err == nil || !strings.Contains(err.Error(), "home client is unavailable") {
					t.Fatalf("post-publication status error = %v", err)
				}
				if service.cfg == base || service.cfg.Plugins.StoreAuth != nil {
					t.Fatal("Home runtime publication retained temporary store authentication")
				}
				if withRule && len(remote.Plugins.StoreAuth) != 1 {
					t.Fatal("runtime publication changed the supplied download configuration")
				}
			},
		)
	}
}

type homeArtifactTransport func(*http.Request) (*http.Response, error)

func (transport homeArtifactTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestHomeStoreAuthReachesPreparationThenLeavesRuntime(t *testing.T) {
	var archive bytes.Buffer
	archiveWriter := zip.NewWriter(&archive)
	const artifact = "synthetic-plugin-artifact"
	extension := ".so"
	if runtime.GOOS == "darwin" {
		extension = ".dylib"
	}
	if runtime.GOOS == "windows" {
		extension = ".dll"
	}
	file, err := archiveWriter.Create("sample" + extension)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, artifact); err != nil {
		t.Fatal(err)
	}
	if err := archiveWriter.Close(); err != nil {
		t.Fatal(err)
	}
	const tokenEnv = "HOME_LIFECYCLE_SYNTHETIC_ARTIFACT_TOKEN"
	t.Setenv(tokenEnv, "synthetic-artifact-token")
	previousTransport := http.DefaultTransport
	requests := 0
	http.DefaultTransport = homeArtifactTransport(
		func(request *http.Request) (*http.Response, error) {
			requests++
			if request.URL.String() != "http://1.1.1.1/sample.zip" ||
				request.Header.Get("Authorization") != "Bearer synthetic-artifact-token" {
				t.Errorf("artifact request lost its download-scoped authentication")
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(bytes.NewReader(archive.Bytes())), Request: request,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	checksum := sha256.Sum256(archive.Bytes())
	raw := fmt.Sprintf(
		"enabled: true\nstore:\n  schema-version: 2\n  id: sample\n  version: 0"+
			".2.0\n  install:\n    type: direct\n    artifacts:\n      - goos: %s\n"+
			"        goarch: %s\n        url: http://1.1.1.1/sample.zip\n        sh"+
			"a256: %s\n",
		runtime.GOOS, runtime.GOARCH, hex.EncodeToString(checksum[:]),
	)
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatal(err)
	}
	var item config.PluginInstanceConfig
	if err := node.Content[0].Decode(&item); err != nil {
		t.Fatal(err)
	}
	homeCfg, _ := homeLifecycleRedis(t, nil)
	client := home.New(homeCfg)
	t.Cleanup(client.Close)
	base := &config.Config{Home: homeCfg}
	service := &Service{cfg: base}
	remote := &config.Config{
		Plugins: config.PluginsConfig{
			Enabled: true, Dir: t.TempDir(), Configs: map[string]config.PluginInstanceConfig{"sample": item},
			StoreAuth: []sdkpluginstore.AuthConfig{
				{
					Match: "http://1.1.1.1/", ApplyTo: []string{sdkpluginstore.RequestKindArtifact},
					Type: sdkpluginstore.AuthTypeBearer, TokenEnv: tokenEnv, AllowInsecure: true,
				},
			},
		},
	}
	if err := service.applyHomeOverlayWithClient(t.Context(), remote, client); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || service.cfg.Plugins.StoreAuth != nil || len(remote.Plugins.StoreAuth) != 1 {
		t.Fatalf(
			"download requests=%d runtime rules=%d input rules=%d",
			requests,
			len(service.cfg.Plugins.StoreAuth),
			len(remote.Plugins.StoreAuth),
		)
	}
	data, err := os.ReadFile(
		filepath.Join(remote.Plugins.Dir, runtime.GOOS, runtime.GOARCH, "sample-v0.2.0"+extension),
	)
	if err != nil || string(data) != artifact {
		t.Fatalf("prepared artifact did not reach installation: %q, %v", data, err)
	}
}

func TestHomeDeleteReportRetryKeepsOriginalActionResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plugin-b-v1.0.0.so")
	if err := os.WriteFile(path, []byte("original-artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reports []string
	homeCfg, _ := homeLifecycleRedis(
		t, func(args []string) string {
			if strings.EqualFold(args[0], "GET") && args[1] == "plugin-tasks" {
				return homeLifecycleBulk(
					`[{"id":8,"operation":"delete","plugin_id":"plugin-b",` +
						`"target_node_type":"cpa","target_node_id":` +
						`"synthetic-home-node"}]`,
				)
			}
			if strings.EqualFold(args[0], "RPUSH") && args[1] == "plugin-status" {
				mu.Lock()
				defer mu.Unlock()
				reports = append(reports, args[2])
				if len(reports) == 1 {
					if err := os.WriteFile(path, []byte("replacement-must-survive"), 0o600); err != nil {
						t.Error(err)
					}
					return "-ERR synthetic status rejection\r\n"
				}
				return ":1\r\n"
			}
			return ""
		},
	)
	client := home.New(homeCfg)
	t.Cleanup(client.Close)
	cfg := &config.Config{Home: homeCfg, Plugins: config.PluginsConfig{Dir: root}}
	service := &Service{cfg: cfg}
	service.processHomePluginTasksWithClient(t.Context(), cfg, client)
	service.processHomePluginTasksWithClient(t.Context(), cfg, client)
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 || reports[0] != reports[1] {
		t.Fatalf("retry did not reuse the original serialized action report: %v", reports)
	}
	var report struct {
		TaskID  uint `json:"task_id"`
		Plugins []struct {
			InstallStatus string `json:"install_status"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(reports[1]), &report); err != nil {
		t.Fatal(err)
	}
	if report.TaskID != 8 || len(report.Plugins) != 1 || report.Plugins[0].InstallStatus != "deleted" {
		t.Fatalf("retry changed the original deletion outcome: %+v", report)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement-must-survive" {
		t.Fatalf("status retry executed deletion again: %q, %v", data, err)
	}
}

func homeLifecycleBulk(value string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(value), value)
}

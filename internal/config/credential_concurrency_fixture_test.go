package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"gopkg.in/yaml.v3"
)

func TestCredentialConcurrencyOriginalFixtureAndHomeValidation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "credential-concurrency-lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Defaults map[string]any `json:"defaults"`
		Invalid  []struct {
			NodeHeartbeatTimeout time.Duration  `json:"node_heartbeat_timeout"`
			Config               map[string]any `json:"config"`
		} `json:"invalid"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Invalid) != 2 || fixture.Invalid[0].NodeHeartbeatTimeout != 3*time.Second ||
		fixture.Invalid[1].NodeHeartbeatTimeout != 20*time.Second {
		t.Fatal("original node timing inputs changed")
	}
	want := config.CredentialConcurrencyConfig{
		LifecycleConfigRevision: 1, CPAHeartbeatTimeout: 3 * time.Second, CPACancelBound: 5 * time.Second,
		ReclaimGrace: 5 * time.Second, CleanupInterval: 5 * time.Second, ReleaseFlushInterval: 250 * time.Millisecond,
		ReleaseMaxBackoff: 2 * time.Second, BusyRetryMin: 250 * time.Millisecond, BusyRetryMax: time.Second,
		MaxLimit: 1_000_000,
	}
	for index, input := range []map[string]any{fixture.Defaults, fixture.Invalid[0].Config, fixture.Invalid[1].Config} {
		t.Run(
			fmt.Sprintf("wire_%d", index), func(t *testing.T) {
				values := maps.Clone(input)
				for _, key := range []string{
					"cpa-heartbeat-timeout", "cpa-cancel-bound", "reclaim-grace", "cleanup-interval",
				} {
					number, valid := values[key].(json.Number)
					if !valid {
						t.Fatalf("fixture %s is not a nanosecond integer", key)
					}
					nanoseconds, err := number.Int64()
					if err != nil {
						t.Fatal(err)
					}
					values[key] = time.Duration(nanoseconds).String()
				}
				for key, value := range values {
					if number, valid := value.(json.Number); valid {
						integer, err := number.Int64()
						if err != nil {
							t.Fatal(err)
						}
						values[key] = integer
					}
				}
				yamlBytes, err := yaml.Marshal(map[string]any{"credential-concurrency": values})
				if err != nil {
					t.Fatal(err)
				}
				var parsed config.Config
				if err := yaml.Unmarshal(yamlBytes, &parsed); err != nil {
					t.Fatal(err)
				}
				expected := want
				if index > 0 {
					expected.LifecycleConfigRevision = 0
				}
				if index == 2 {
					expected.CPAHeartbeatTimeout = 0
				}
				gotJSON, err := json.Marshal(parsed.CredentialConcurrency)
				if err != nil {
					t.Fatal(err)
				}
				wantJSON, err := json.Marshal(expected)
				if err != nil || !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("wire values=%s want=%s error=%v", gotJSON, wantJSON, err)
				}
				client := home.New(config.HomeConfig{Enabled: true})
				err = client.SetLifecycleConfig(parsed.CredentialConcurrency)
				// 节点心跳间隔由 Home 校验，CPA 验证自身配置中的值
				if (err != nil) != (index == 2) {
					t.Fatalf("Home.SetLifecycleConfig error=%v, intrinsic error expected=%t", err, index == 2)
				}
				if index < 2 {
					installed, err := json.Marshal(client.LimiterConfig())
					if err != nil || !bytes.Equal(installed, wantJSON) {
						t.Fatalf("installed lifecycle=%s want=%s error=%v", installed, wantJSON, err)
					}
				}
			},
		)
	}
	legacy := want
	legacy.LifecycleConfigRevision = 0
	defaults, err := json.Marshal((config.CredentialConcurrencyConfig{}).WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(legacy)
	if err != nil || !bytes.Equal(defaults, expected) {
		t.Fatalf("legacy defaults=%s want=%s error=%v", defaults, expected, err)
	}
}

package cliproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
)

func TestHomePluginTaskTargetsMatchLocalNode(t *testing.T) {
	for _, sample := range []struct {
		name       string
		targetType string
		targetID   string
		noNode     bool
		delete     bool
	}{
		{name: "local", targetType: "cpa", targetID: "synthetic-home-node", delete: true},
		{name: "normalized local", targetType: " CPA ", targetID: " synthetic-home-node ", delete: true},
		{name: "CPA broadcast", targetType: "cpa", targetID: "*", delete: true},
		{name: "node broadcast", targetType: "*", targetID: "synthetic-home-node", delete: true},
		{name: "explicit broadcast", targetType: "*", targetID: "*", delete: true},
		{name: "other type", targetType: "panel", targetID: "synthetic-home-node"},
		{name: "other node", targetType: "cpa", targetID: "another-node"},
		{name: "case-sensitive node", targetType: "cpa", targetID: "SYNTHETIC-HOME-NODE"},
		{name: "untargeted"},
		{name: "missing type", targetID: "synthetic-home-node"},
		{name: "missing node", targetType: "cpa"},
		{name: "missing local identity", targetType: "*", targetID: "*", noNode: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				root := t.TempDir()
				artifact := filepath.Join(root, "plugin-b-v1.0.0.so")
				if err := os.WriteFile(artifact, []byte("original-artifact"), 0o600); err != nil {
					t.Fatal(err)
				}
				task := home.PluginTask{
					ID: 8, Operation: "delete", PluginID: "plugin-b", TargetNodeType: sample.targetType,
					TargetNodeID: sample.targetID,
				}
				raw, err := json.Marshal([]home.PluginTask{task})
				if err != nil {
					t.Fatal(err)
				}
				var reports atomic.Int32
				homeCfg, _ := homeLifecycleRedis(
					t, func(args []string) string {
						if strings.EqualFold(args[0], "GET") && args[1] == "plugin-tasks" {
							return homeLifecycleBulk(string(raw))
						}
						if strings.EqualFold(args[0], "RPUSH") && args[1] == "plugin-status" {
							reports.Add(1)
							return ":1\r\n"
						}
						return ""
					},
				)
				client := home.New(homeCfg)
				t.Cleanup(client.Close)
				if sample.noNode {
					homeCfg.NodeID = ""
				}
				cfg := &config.Config{Home: homeCfg, Plugins: config.PluginsConfig{Dir: root}}
				service := &Service{cfg: cfg}
				service.processHomePluginTasksWithClient(t.Context(), cfg, client)
				if sample.delete {
					if _, err := os.Stat(artifact); !os.IsNotExist(err) {
						t.Fatalf("local task did not delete its artifact: %v", err)
					}
					if reports.Load() != 1 {
						t.Fatalf("local reports = %d, want 1", reports.Load())
					}
				} else {
					data, err := os.ReadFile(artifact)
					if err != nil || string(data) != "original-artifact" {
						t.Fatalf("unmatched task changed local artifact: %q, %v", data, err)
					}
					if reports.Load() != 0 {
						t.Fatalf("unmatched task reported a local result: %d", reports.Load())
					}
				}
				if service.homePluginTasks != nil {
					t.Fatal("completed task selection retained pending work")
				}
			},
		)
	}
}

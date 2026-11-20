package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestLoginConfigurationInitialization(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		commandMode bool
		primary     string
		backup      string
		primaryDir  bool
		wantErr     bool
		wantPort    int
		wantWarning bool
	}{
		{name: "login without configuration", commandMode: true},
		{name: "server requires configuration", wantErr: true},
		{name: "invalid login configuration remains an error", commandMode: true, primary: "port: [", wantErr: true},
		{name: "login keeps valid configuration", commandMode: true, primary: "port: 8345\n", wantPort: 8345},
		{
			name: "login uses last good configuration", commandMode: true, backup: "port: 8371\n", wantPort: 8371,
			wantWarning: true,
		},
		{
			name: "server keeps last good after invalid YAML", primary: "port: [\n", backup: "port: 8371\n",
			wantPort: 8371, wantWarning: true,
		},
		{
			name: "server keeps last good after invalid port", primary: "port: 70000\n", backup: "port: 8371\n",
			wantPort: 8371, wantWarning: true,
		},
		{name: "directory is not missing configuration", commandMode: true, primaryDir: true, wantErr: true},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("WRITABLE_PATH", dir)
				path := filepath.Join(dir, "config.yaml")
				if testCase.primaryDir {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				} else if testCase.primary != "" {
					if err := os.WriteFile(path, []byte(testCase.primary), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if testCase.backup != "" {
					if err := os.WriteFile(config.LastGoodPath(path), []byte(testCase.backup), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				var output bytes.Buffer
				previousOutput := log.StandardLogger().Out
				log.SetOutput(&output)
				t.Cleanup(func() { log.SetOutput(previousOutput) })
				result, err := initStoreAndConfig(
					dir, path, false, testCase.commandMode,
					func(_ ...string) (string, bool) { return "", false }, "", false, nil,
				)
				if (err != nil) != testCase.wantErr {
					t.Fatalf("configuration initialization error = %v, want error %v", err, testCase.wantErr)
				}
				if testCase.wantErr {
					return
				}
				if result == nil || result.cfg == nil || result.tokenStore == nil ||
					result.cfg.Port != testCase.wantPort {
					t.Fatalf("unexpected login configuration result: %#v", result)
				}
				if testCase.wantWarning && (!strings.Contains(output.String(), "using last-good backup") ||
					!strings.Contains(output.String(), config.LastGoodPath(path))) {
					t.Fatalf("startup did not report the restored configuration: %s", output.String())
				}
				if testCase.backup != "" {
					if data, err := os.ReadFile(config.LastGoodPath(path)); err != nil ||
						string(data) != testCase.backup {
						t.Fatalf("startup changed the last-good configuration: data=%q error=%v", data, err)
					}
				}
				if testCase.primary == "" && testCase.backup == "" {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("login initialization created a configuration file: %v", err)
					}
				}
			},
		)
	}
}

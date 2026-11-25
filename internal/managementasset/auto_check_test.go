package managementasset

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPrepareManagementHTMLOnlyInstallsMissingPanel(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		cfg                   config.Config
		before, during, after string
		cancel, restart       bool
		wantRequests          int
	}{
		{name: "first install with periodic checks disabled", after: "new panel", wantRequests: 2},
		{name: "operator installed during download", during: "manual panel", after: "manual panel", wantRequests: 2},
		{name: "already installed", before: "old panel", after: "old panel"},
		{name: "canceled startup", cancel: true},
		{name: "restart after canceled download", restart: true, after: "new panel", wantRequests: 4},
		{name: "cluster mode", cfg: config.Config{Home: config.HomeConfig{Enabled: true}}},
		{
			name: "panel disabled",
			cfg:  config.Config{RemoteManagement: config.RemoteManagement{DisableControlPanel: true}},
		},
		{
			name: "automatic installation disabled",
			cfg:  config.Config{RemoteManagement: config.RemoteManagement{DisableAutoUpdatePanel: true}},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("MANAGEMENT_STATIC_PATH", dir)
				path := filepath.Join(dir, managementAssetName)
				if tc.before != "" {
					if err := os.WriteFile(path, []byte(tc.before), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancel {
					cancel()
				}
				var requests atomic.Int32
				setManagementTestTransport(
					t, roundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							requests.Add(1)
							switch req.URL.Hostname() {
							case "api.github.com":
								return managementTestResponse(
									req, http.StatusOK, managementTestRelease("new panel", managementTestAssetURL, ""),
								), nil
							case "github.com":
								if tc.restart && requests.Load() == 2 {
									cancel()
								}
								if tc.during != "" {
									if err := os.WriteFile(path, []byte(tc.during), 0o600); err != nil {
										return nil, err
									}
								}
								return managementTestResponse(req, http.StatusOK, "new panel"), nil
							default:
								return nil, fmt.Errorf("unexpected fixture host %q", req.URL.Hostname())
							}
						},
					),
				)
				configPath := filepath.Join(dir, "config.yaml")
				PrepareManagementHTML(ctx, configPath, &tc.cfg)
				if tc.restart {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("canceled preparation published the panel: %v", err)
					}
					PrepareManagementHTML(t.Context(), configPath, &tc.cfg)
				}
				if got := requests.Load(); int(got) != tc.wantRequests {
					t.Fatalf("network requests = %d, want %d", got, tc.wantRequests)
				}
				data, err := os.ReadFile(path)
				if tc.after == "" {
					if !os.IsNotExist(err) {
						t.Fatalf("missing panel read = %q, %v", data, err)
					}
				} else if err != nil || string(data) != tc.after {
					t.Fatalf("prepared panel = %q, %v; want %q", data, err, tc.after)
				}
			},
		)
	}
}

func TestAutoUpdaterChecksReleaseWithoutInstallingPanel(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(
			fmt.Sprint(status), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, managementAssetName)
				if err := os.WriteFile(path, []byte("existing panel"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("MANAGEMENT_STATIC_PATH", dir)
				previous := currentConfigPtr.Load()
				t.Cleanup(func() { currentConfigPtr.Store(previous) })
				SetCurrentConfig(
					&config.Config{
						RemoteManagement: config.RemoteManagement{
							AutoCheckUpdate:        true,
							DisableAutoUpdatePanel: true,
							PanelGitHubRepository:  "https://github.com/fixture/panel",
							CheckInterval:          60,
						},
					},
				)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var releases, downloads atomic.Int32
				setManagementTestTransport(
					t, roundTripFunc(
						func(request *http.Request) (*http.Response, error) {
							if request.URL.String() == "https://api.github.com/repos/fixture/panel/releases/latest" {
								releases.Add(1)
								return managementTestResponse(
									request, status,
									managementTestRelease("new panel", managementTestAssetURL, ""),
								), nil
							}
							downloads.Add(1)
							return nil, fmt.Errorf("unexpected download %s", request.URL)
						},
					),
				)
				runAutoUpdater(ctx)
				if releases.Load() != 1 || downloads.Load() != 0 {
					t.Fatalf("check-only requests: releases=%d downloads=%d", releases.Load(), downloads.Load())
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != "existing panel" {
					t.Fatalf("check-only mode changed panel: data=%q err=%v", data, err)
				}
			},
		)
	}
}

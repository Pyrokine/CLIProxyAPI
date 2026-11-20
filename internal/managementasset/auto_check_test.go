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
									managementTestRelease("new panel", "https://assets.example/management.html"),
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

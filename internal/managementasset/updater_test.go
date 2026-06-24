package managementasset

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAutoUpdateSkipReason(t *testing.T) {
	disabled := true
	tests := []struct {
		name       string
		cfg        *config.Config
		wantReason string
		wantSkip   bool
	}{
		{
			name:       "nil config",
			cfg:        nil,
			wantReason: "config not yet available",
			wantSkip:   true,
		},
		{
			name: "cluster mode",
			cfg: &config.Config{
				Home: config.HomeConfig{Enabled: true},
			},
			wantReason: "cluster mode enabled",
			wantSkip:   true,
		},
		{
			name: "control panel disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableControlPanel: true},
			},
			wantReason: "control panel disabled",
			wantSkip:   true,
		},
		{
			name: "auto check disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{AutoCheckUpdate: false},
			},
			wantReason: "auto-check-update is false",
			wantSkip:   true,
		},
		{
			name: "auto update disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{
					AutoCheckUpdate:        true,
					DisableAutoUpdatePanel: &disabled,
				},
			},
			wantReason: "auto-update-panel is false",
			wantSkip:   true,
		},
		{
			name: "enabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{AutoCheckUpdate: true},
			},
			wantReason: "",
			wantSkip:   false,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				gotReason, gotSkip := autoUpdateSkipReason(tt.cfg)
				if gotReason != tt.wantReason || gotSkip != tt.wantSkip {
					t.Fatalf(
						"autoUpdateSkipReason() = (%q, %t), want (%q, %t)",
						gotReason,
						gotSkip,
						tt.wantReason,
						tt.wantSkip,
					)
				}
			},
		)
	}
}

func TestDownloadAssetRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(strings.Repeat("x", maxManagementAssetBytes+1)))
			},
		),
	)
	defer server.Close()

	_, _, err := downloadAsset(context.Background(), server.Client(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("downloadAsset() error = %v, want oversized error", err)
	}
}

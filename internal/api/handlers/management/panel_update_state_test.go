package management

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestGetPanelUpdateStateReadsActualAssetWithoutPublication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		local      string
		version    string
		remoteFail bool
		disabled   bool
		status     int
		matches    bool
	}{
		{name: "matching-latest", local: "abc", status: 200, matches: true},
		{name: "different-latest", local: "old", status: 200},
		{name: "missing-file", status: 200},
		{name: "explicit-version", local: "abc", version: "v1.2.3", status: 200, matches: true},
		{name: "remote-failure", local: "abc", remoteFail: true, status: 502},
		{name: "disabled", local: "abc", disabled: true, status: 400},
		{name: "invalid-version", local: "abc", version: "../invalid", status: 400},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("MANAGEMENT_STATIC_PATH", dir)
				t.Setenv("GITHUB_TOKEN", "")
				path := filepath.Join(dir, "management.html")
				if tc.local != "" {
					if err := os.WriteFile(path, []byte(tc.local), 0600); err != nil {
						t.Fatal(err)
					}
				}
				previous := http.DefaultTransport
				calls := 0
				http.DefaultTransport = releaseRoundTripper(
					func(request *http.Request) (*http.Response, error) {
						calls++
						if !strings.Contains(request.URL.Path, "/releases/") {
							t.Errorf("readback tried a non-release request: %s", request.URL.Path)
						}
						if tc.version != "" && !strings.HasSuffix(request.URL.Path, "/tags/"+tc.version) {
							t.Errorf("version path=%s", request.URL.Path)
						}
						status := 200
						body := `{"assets":[{"name":"management.html","browser_download_url":"https://github.` +
							`com/synthetic/panel/releases/download/v1.2.3/management.html","digest":"sha256:` +
							`ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}]}`
						if tc.remoteFail {
							status = 503
							body = "synthetic remote failure"
						}
						return &http.Response{
							StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
							Request: request,
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = previous })
				cfg := &config.Config{}
				cfg.RemoteManagement.DisableControlPanel = tc.disabled
				h := &Handler{cfg: cfg, configFilePath: filepath.Join(dir, "config.yaml")}
				engine := gin.New()
				engine.Use(h.MutationAuditMiddleware())
				engine.GET("/panel-update", h.GetPanelUpdateState)
				response := httptest.NewRecorder()
				engine.ServeHTTP(
					response, httptest.NewRequest(http.MethodGet, "/panel-update?version="+tc.version, nil),
				)
				if response.Code != tc.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if tc.status == 200 {
					var state struct {
						Installed     bool   `json:"installed"`
						Digest        string `json:"digest"`
						TargetDigest  string `json:"target_digest"`
						MatchesTarget bool   `json:"matches_target"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
						t.Fatal(err)
					}
					if state.Installed != (tc.local != "") || state.MatchesTarget != tc.matches ||
						state.TargetDigest !=
							("sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff6"+
								"1f20015ad") {
						t.Fatalf("readback=%+v", state)
					}
					if state.Installed && !strings.HasPrefix(state.Digest, "sha256:") {
						t.Fatalf("digest=%q", state.Digest)
					}
				}
				if response.Header().Get(MutationResultHeader) != "" {
					t.Fatal("readback created a mutation receipt")
				}
				data, err := os.ReadFile(path)
				if tc.local == "" {
					if !os.IsNotExist(err) {
						t.Fatalf("missing panel was created: %v", err)
					}
				} else if err != nil || string(data) != tc.local {
					t.Fatalf("readback changed asset=%q err=%v", data, err)
				}
				if (tc.disabled || tc.version == "../invalid") && calls != 0 {
					t.Fatalf("rejected readback made %d remote calls", calls)
				}
			},
		)
	}
}

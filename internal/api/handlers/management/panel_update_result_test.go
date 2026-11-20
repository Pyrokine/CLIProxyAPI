package management

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPostPanelUpdateReturnsFailureWhenReleaseFailsWithOldPanel(t *testing.T) {
	for _, version := range []string{"", "v1.2.3"} {
		t.Run(
			"version_"+version, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("MANAGEMENT_STATIC_PATH", dir)
				t.Setenv("GITHUB_TOKEN", "")
				path := filepath.Join(dir, "management.html")
				const original = "previous management panel\n"
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				previousTransport := http.DefaultTransport
				calls := 0
				http.DefaultTransport = releaseRoundTripper(
					func(request *http.Request) (*http.Response, error) {
						calls++
						return &http.Response{
							StatusCode: http.StatusServiceUnavailable, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader("synthetic release failure")), Request: request,
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = previousTransport })
				h := &Handler{cfg: &config.Config{}, configFilePath: filepath.Join(dir, "config.yaml")}
				recorder := runConfigListRequest(
					h.PostPanelUpdate, http.MethodPost, "/panel-update", `{"version":"`+version+`"}`,
				)
				if recorder.Code != http.StatusBadGateway || calls != 1 || !strings.Contains(
					recorder.Body.String(), "update_failed",
				) {
					t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != original {
					t.Fatalf("old panel=%q error=%v", data, err)
				}
			},
		)
	}
}

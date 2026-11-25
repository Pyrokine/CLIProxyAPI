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

func TestPostPanelUpdateSeparatesEmptyAndInvalidRequests(t *testing.T) {
	for _, tc := range []struct{ name, body, rejection string }{
		{name: "empty body"},
		{name: "whitespace", body: " \t\n"},
		{name: "empty object", body: `{}`},
		{name: "empty version", body: `{"version":""}`},
		{name: "explicit version", body: `{"version":"v1.2.3"}`},
		{"truncated object", `{"version":`, "invalid_request"},
		{"wrong type", `{"version":123}`, "invalid_request"},
		{"null field", `{"version":null}`, "invalid_request"},
		{"null document", `null`, "invalid_request"},
		{"unknown field", `{"label":"v1"}`, "invalid_request"},
		{"ambiguous version", `{"version":"v1","version":""}`, "invalid_request"},
		{"trailing document", `{} {}`, "invalid_request"},
		{"invalid version", `{"version":"not a tag"}`, "invalid_version"},
		{"oversized body", strings.Repeat(" ", 4097), "invalid_request"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("MANAGEMENT_STATIC_PATH", dir)
				t.Setenv("GITHUB_TOKEN", "")
				previous := http.DefaultTransport
				calls := 0
				http.DefaultTransport = releaseRoundTripper(
					func(req *http.Request) (*http.Response, error) {
						calls++
						return &http.Response{
							StatusCode: http.StatusServiceUnavailable, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader("synthetic release failure")), Request: req,
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = previous })
				h := &Handler{cfg: &config.Config{}, configFilePath: filepath.Join(dir, "config.yaml")}
				response := runConfigListRequest(h.PostPanelUpdate, http.MethodPost, "/panel-update", tc.body)
				status, wantCalls, code := http.StatusBadGateway, 1, "update_failed"
				if tc.rejection != "" {
					status, wantCalls, code = http.StatusBadRequest, 0, tc.rejection
				}
				if response.Code != status || calls != wantCalls || !strings.Contains(response.Body.String(), code) {
					t.Fatalf("status=%d, release calls=%d, body=%s", response.Code, calls, response.Body.String())
				}
			},
		)
	}
}

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

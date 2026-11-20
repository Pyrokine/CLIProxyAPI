package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/google/uuid"
)

type managementPanelRoundTripper func(*http.Request) (*http.Response, error)

func (f managementPanelRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestManagementPanelReadbackRouteReconcilesNoPublication(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	t.Setenv("GITHUB_TOKEN", "")
	dir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", dir)
	const content = "fixture installed management panel"
	if err := os.WriteFile(filepath.Join(dir, "management.html"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content)))
	var releaseRequests, downloads atomic.Int32
	previous := http.DefaultTransport
	http.DefaultTransport = managementPanelRoundTripper(
		func(req *http.Request) (*http.Response, error) {
			if req.URL.Hostname() != "api.github.com" {
				downloads.Add(1)
				return nil, fmt.Errorf("unexpected fixture host %q", req.URL.Hostname())
			}
			releaseRequests.Add(1)
			body := fmt.Sprintf(
				`{"assets":[{"name":"management.html","browser_download_url":"https://`+
					`assets.example/management.html","digest":`+
					`%q}]}`,
				digest,
			)
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
				Request: req,
			}, nil
		},
	)
	t.Cleanup(func() { http.DefaultTransport = previous })
	server := newTestServer(t)
	mutationID := uuid.NewString()
	request := func(method, body string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/v0/management/panel-update", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(management.MutationIDHeader, mutationID)
		if authorized {
			req.Header.Set("Authorization", "Bearer test-management-password")
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}
	if result := request(
		http.MethodGet, "", false,
	); result.Code != http.StatusUnauthorized || releaseRequests.Load() != 0 {
		t.Fatalf("unauthorized readback = %d, release requests=%d", result.Code, releaseRequests.Load())
	}
	updated := request(http.MethodPost, `{}`, true)
	if updated.Code != http.StatusOK {
		t.Fatalf("no-op update = %d: %s", updated.Code, updated.Body.String())
	}
	var receipt management.MutationResult
	if err := json.Unmarshal([]byte(updated.Header().Get(management.MutationResultHeader)), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != "uncertain" || receipt.ConfirmedEffect || len(receipt.Effects) != 0 ||
		receipt.MutationID != mutationID {
		t.Fatalf("no-publication receipt = %+v", receipt)
	}
	readback := request(http.MethodGet, "", true)
	var state managementasset.ManagementHTMLState
	if err := json.Unmarshal(readback.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if readback.Code != http.StatusOK || !state.Installed || !state.MatchesTarget || state.Digest != digest ||
		state.TargetDigest != digest {
		t.Fatalf("readback = %d: %+v", readback.Code, state)
	}
	if readback.Header().Get(management.MutationResultHeader) != "" || releaseRequests.Load() != 2 ||
		downloads.Load() != 0 {
		t.Fatalf(
			"readback side effects: receipt=%q, release requests=%d, downloads=%d",
			readback.Header().Get(management.MutationResultHeader), releaseRequests.Load(), downloads.Load(),
		)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "management.html")); err != nil || string(data) != content {
		t.Fatalf("readback changed asset: data=%q, error=%v", data, err)
	}
}

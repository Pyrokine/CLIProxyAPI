package management

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestCPAAutoUpdaterChecksWithoutInstalling(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	previousState := snapshotSelfUpdate()
	setSelfUpdate("idle", "", "", "", 0)
	t.Cleanup(
		func() {
			setSelfUpdate(
				previousState.status, previousState.message, previousState.version, previousState.attemptID,
				previousState.percent,
			)
		},
	)
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var checks, unexpected atomic.Int32
	var stopCallback context.CancelFunc
	http.DefaultTransport = releaseRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(request.URL.Path, "/releases/latest") {
				unexpected.Add(1)
			} else {
				checks.Add(1)
			}
			if stopCallback != nil {
				stopCallback()
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"tag_name":"v99.99.99"}`)), Request: request,
			}, nil
		},
	)
	cfg := &config.Config{}
	cfg.RemoteManagement.AutoCheckUpdate = true
	cfg.RemoteManagement.AutoUpdateCPA = false
	handler := &Handler{cfg: cfg, stateStore: store}
	handler.tryAutoUpdateCPA(context.Background(), cfg)
	if checks.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("checks=%d installation requests=%d", checks.Load(), unexpected.Load())
	}
	if session, err := store.loadUpdateSession(context.Background()); err != nil || session != nil {
		t.Fatalf("installation session=%+v error=%v", session, err)
	}
	if state := snapshotSelfUpdate(); state.status != "idle" {
		t.Fatalf("installation state=%s", state.status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopCallback = cancel
	handler.runCPAAutoUpdater(ctx)
	if checks.Load() != 2 || unexpected.Load() != 0 {
		t.Fatalf("callback checks=%d installation requests=%d", checks.Load(), unexpected.Load())
	}
	cfg.RemoteManagement.AutoCheckUpdate = false
	handler.tryAutoUpdateCPA(context.Background(), cfg)
	if checks.Load() != 2 {
		t.Fatal("disabled checks made an HTTP request")
	}
	cfg.RemoteManagement.AutoCheckUpdate = true
	setSelfUpdate("downloading", "", "", "", 0)
	handler.tryAutoUpdateCPA(context.Background(), cfg)
	if checks.Load() != 2 {
		t.Fatal("busy updater made an HTTP request")
	}
}

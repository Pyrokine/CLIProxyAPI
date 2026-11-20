package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestHandlerRuntimeKeepsPublishedConfigAndRelayTogether(t *testing.T) {
	first := &config.Config{}
	first.ProxyURL = "http://previous-proxy.invalid:8080"
	first.Codex.LiveMediaRelay.Enabled = true
	current := first
	handler := NewHandler(nil, func() *config.Config { return current })
	previousRelay, err := handler.currentMediaRelay()
	if err != nil || previousRelay == nil {
		t.Fatalf("initial relay=%T, error=%v", previousRelay, err)
	}
	current = &config.Config{}
	current.ProxyURL = "http://next-proxy.invalid:8080"
	before, relay, err := handler.currentRuntime()
	if err != nil || before == nil || before.ProxyURL != first.ProxyURL || !before.Codex.LiveMediaRelay.Enabled ||
		relay != previousRelay {
		t.Fatalf(
			"provider publication changed the existing runtime pair: config=%+v, relay=%T, error=%v",
			before,
			relay,
			err,
		)
	}
	if err := handler.UpdateConfig(current); err != nil {
		t.Fatal(err)
	}
	after, relay, err := handler.currentRuntime()
	if err != nil || after == nil || after.ProxyURL != current.ProxyURL || after.Codex.LiveMediaRelay.Enabled ||
		relay != nil {
		t.Fatalf("disabled runtime pair: config=%+v, relay=%T, error=%v", after, relay, err)
	}
	if before.ProxyURL != first.ProxyURL || !before.Codex.LiveMediaRelay.Enabled {
		t.Fatal("publication changed an earlier request snapshot")
	}
	unchangedRelay := *current
	unchangedRelay.ProxyURL = "http://unrelated-change.invalid:8080"
	current = &unchangedRelay
	if err := handler.UpdateConfig(current); err != nil {
		t.Fatal(err)
	}
	captured, relay, err := handler.currentRuntime()
	if err != nil || captured.ProxyURL != unchangedRelay.ProxyURL || relay != nil {
		t.Fatal("unchanged relay settings prevented other runtime configuration from updating")
	}
	current.ProxyURL = "http://caller-mutation.invalid:8080"
	if captured.ProxyURL != "http://unrelated-change.invalid:8080" {
		t.Fatal("caller mutation changed the published runtime configuration")
	}
	invalid := &config.Config{}
	invalid.Codex.LiveMediaRelay.Enabled = true
	invalid.Codex.LiveMediaRelay.UDPPortMin, invalid.Codex.LiveMediaRelay.UDPPortMax = 20000, 10000
	if err := handler.UpdateConfig(invalid); err == nil {
		t.Fatal("invalid relay configuration was accepted")
	}
	failed, _, err := handler.currentRuntime()
	if err == nil || failed == nil || failed.Codex.LiveMediaRelay.UDPPortMin != 20000 {
		t.Fatal("failed relay construction lost its matching configuration and error")
	}
	if err := handler.UpdateConfig(first); err != nil {
		t.Fatal(err)
	}
	recovered, relay, err := handler.currentRuntime()
	if err != nil || recovered.ProxyURL != first.ProxyURL || relay == nil {
		t.Fatal("valid configuration did not recover the runtime pair")
	}
}

func TestHandlerRuntimeSnapshotStaysPairedDuringPublication(t *testing.T) {
	first, second := &config.Config{}, &config.Config{}
	first.Codex.LiveMediaRelay.Enabled = true
	var provider atomic.Pointer[config.Config]
	provider.Store(first)
	handler := NewHandler(nil, provider.Load)
	var writers sync.WaitGroup
	writers.Go(
		func() {
			for index := range 40 {
				cfg := first
				if index%2 == 0 {
					cfg = second
				}
				provider.Store(cfg)
				if err := handler.UpdateConfig(cfg); err != nil {
					t.Errorf("update runtime: %v", err)
					return
				}
			}
		},
	)
	for range 1000 {
		cfg, relay, err := handler.currentRuntime()
		if err != nil || cfg == nil || cfg.Codex.LiveMediaRelay.Enabled != (relay != nil) {
			t.Errorf("mixed runtime publication: config=%+v, relay=%T, error=%v", cfg, relay, err)
			break
		}
	}
	writers.Wait()
}

func TestHandlerBootstrapUsesPublishedRuntimeProxyWithItsRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &captureExecutor{
		responseBody: &trackedResponseBody{Reader: strings.NewReader("v=0\r\no=upstream-answer\r\n")},
	}
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerCredential(
		t, manager, &auth.Auth{
			ID: "runtime-pair", Provider: "codex", Status: auth.StatusActive,
			Metadata: map[string]any{"access_token": "fixture-token"},
		},
	)
	first := &config.Config{}
	first.ProxyURL = "http://published-proxy.invalid:8080"
	current := first
	handler := NewHandler(manager, func() *config.Config { return current })
	t.Cleanup(
		func() {
			if err := handler.Close(); err != nil {
				t.Errorf("close handler: %v", err)
			}
		},
	)
	relay := &fakeMediaRelay{
		upstreamOffer: "v=0\r\no=gateway-offer\r\n",
		session:       &fakeMediaSession{downstreamSDP: "v=0\r\no=downstream-answer\r\n"},
	}
	installFakeMediaRelay(handler, relay)
	current = &config.Config{}
	current.ProxyURL = "http://pending-proxy.invalid:8080"
	router := gin.New()
	router.POST("/v1/live", handler.Handle)
	request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader("v=0\r\no=desktop-offer\r\n"))
	request.Header.Set("Content-Type", "application/sdp")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || relay.route.proxyURL != first.ProxyURL {
		t.Fatalf(
			"bootstrap status=%d, relay proxy=%q, want %q; body=%s", recorder.Code, relay.route.proxyURL,
			first.ProxyURL, recorder.Body.String(),
		)
	}
}

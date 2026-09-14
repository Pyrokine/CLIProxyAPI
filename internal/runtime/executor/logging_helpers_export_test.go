package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestExportedAPIWebsocketLoggingWrappersUseExistingTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	cfg := &internalconfig.Config{RequestLog: true}
	headers := http.Header{
		"Authorization":     []string{"Bearer websocket-secret"},
		"X-Oai-Attestation": []string{"[REDACTED]"},
	}

	RecordAPIWebsocketRequest(
		ctx,
		cfg,
		"wss://api.openai.com/v1/live/call-1",
		"WEBSOCKET",
		headers,
		nil,
		"codex",
		&cliproxyauth.Auth{ID: "auth-1", Provider: "codex"},
	)
	RecordAPIWebsocketHandshake(ctx, cfg, http.StatusSwitchingProtocols, http.Header{"Upgrade": []string{"websocket"}})
	RecordAPIWebsocketError(ctx, cfg, "relay", errors.New("relay failed"))

	value, ok := ginCtx.Get(apiWebsocketTimelineKey)
	if !ok {
		t.Fatal("exported wrappers did not use the API WebSocket timeline")
	}
	timeline := value.(*logging.BoundedLogBuffer).String()
	for _, want := range []string{
		"Event: api.websocket.request",
		"Event: api.websocket.handshake",
		"Event: api.websocket.error",
		"Stage: relay",
		"X-Oai-Attestation: [REDACTED]",
	} {
		if !strings.Contains(timeline, want) {
			t.Fatalf("timeline missing %q: %s", want, timeline)
		}
	}
	if strings.Contains(timeline, "websocket-secret") {
		t.Fatalf("timeline leaked Authorization: %s", timeline)
	}
	if got := headers.Get("Authorization"); got != "Bearer websocket-secret" {
		t.Fatalf("source Authorization changed to %q", got)
	}
	if got := headers.Get("X-Oai-Attestation"); got != "[REDACTED]" {
		t.Fatalf("source attestation changed to %q", got)
	}
}

package grokbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/gin-gonic/gin"
)

func TestIsGrokShellUserAgent(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want bool
	}{
		{name: "shell", ua: "grok-shell/0.2.120 (linux; amd64)", want: true},
		{name: "pager and shell", ua: "grok-pager/0.2.120 grok-shell/0.2.120", want: true},
		{name: "pager only", ua: "grok-pager/1.0.5", want: true},
		{name: "case insensitive", ua: "GROK-SHELL/0.2.120", want: true},
		{name: "ordinary client", ua: "curl/8.7.1", want: false},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := IsGrokShellUserAgent(tt.ua); got != tt.want {
					t.Fatalf("IsGrokShellUserAgent(%q) = %t, want %t", tt.ua, got, tt.want)
				}
			},
		)
	}
}

func TestIsGrokClientContext(t *testing.T) {
	//lint:ignore SA1012 This assertion verifies nil context handling.
	if IsGrokClientContext(nil, nil) {
		t.Fatal("nil context and headers identified as Grok client")
	}
	headers := http.Header{"User-Agent": {"grok-shell/1.0"}}
	if !IsGrokClientContext(context.Background(), headers) {
		t.Fatal("explicit headers did not identify Grok Shell")
	}

	gin.SetMode(gin.TestMode)
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginContext.Request.Header.Set("User-Agent", "grok-pager/1.0")
	ctx := util.WithGinContext(context.Background(), ginContext)
	if !IsGrokClientContext(ctx, nil) {
		t.Fatal("Gin request headers did not identify Grok Pager")
	}
}

func TestTransformKeepaliveSSEFrame(t *testing.T) {
	frame := []byte("event: keepalive\ndata: {\"type\":\"keepalive\",\"sequence_number\":3}\n\n")
	transformed, ok := TransformKeepaliveSSEFrame(frame, true)
	if !ok || !bytes.Equal(transformed, keepaliveSSEComment) {
		t.Fatalf("Grok keepalive = %q, %t, want %q, true", transformed, ok, keepaliveSSEComment)
	}
	if got, changed := TransformKeepaliveSSEFrame(frame, false); changed || !bytes.Equal(got, frame) {
		t.Fatalf("ordinary client keepalive = %q, %t, want unchanged", got, changed)
	}
	normal := []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	if got, changed := TransformKeepaliveSSEFrame(normal, true); changed || !bytes.Equal(got, normal) {
		t.Fatalf("normal frame = %q, %t, want unchanged", got, changed)
	}
}

func TestBuildResponse(t *testing.T) {
	response := BuildResponse(
		[]ModelInfo{
			{ID: "grok-4.5", DisplayName: "Grok 4.5", ContextLength: 256000, ReasoningLevels: []string{" high ", ""}},
			{ID: "plain-model"},
		},
	)
	if response.Object != "list" || len(response.Data) != 2 {
		t.Fatalf("response = %#v", response)
	}
	entry := response.Data[0]
	if entry.ID != "grok-4.5" || entry.Model != entry.ID || entry.Name != "Grok 4.5" {
		t.Fatalf("identity = %#v", entry)
	}
	if entry.ContextWindow != 256000 || entry.APIBackend != "responses" || !entry.SupportedInAPI {
		t.Fatalf("routing metadata = %#v", entry)
	}
	if len(entry.ReasoningEfforts) != 1 || entry.ReasoningEfforts[0].Value != "high" {
		t.Fatalf("reasoning efforts = %#v", entry.ReasoningEfforts)
	}
	if response.Data[1].Name != "plain-model" {
		t.Fatalf("fallback name = %#v", response.Data[1])
	}

	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := decoded.Data[1]["context_window"]; ok {
		t.Fatalf("zero context_window was not omitted: %s", wire)
	}
	if _, ok := decoded.Data[1]["reasoning_efforts"]; ok {
		t.Fatalf("empty reasoning_efforts was not omitted: %s", wire)
	}
}

package executor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// fixtureAuth builds a cloaked direct-Anthropic API-key auth for header tests.
// The key value is a placeholder that never reaches a real upstream.
func fixtureAuth() *cliproxyauth.Auth {
	attrs := map[string]string{}
	attrs[cliproxyauth.AttributeAPIKey] = strings.Join([]string{"test", "fixture", "key"}, "-")
	attrs["fingerprint_profile"] = "claude-code-cli"
	return &cliproxyauth.Auth{Attributes: attrs}
}

// A caller newer than the pinned profile keeps unknown betas, while the proxy
// assembles recognized feature betas in the current Claude Code wire order.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "message-threads-2026-08-12")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-fable-5-1"}`), &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.Contains(betas, "message-threads-2026-08-12") {
		t.Fatalf("Anthropic-Beta = %q, want unknown caller beta forwarded", betas)
	}
	wantOrder := claudeMidConvSystemBeta + "," + claudePerTurnControlBeta + "," +
		claudeMidConvToolChangesBeta + "," + claudeMidConvSystemClearAtBeta + "," + claudeEffortBeta
	if !strings.Contains(betas, wantOrder) {
		t.Fatalf("Anthropic-Beta = %q, want feature beta order %q", betas, wantOrder)
	}
}

func TestApplyClaudeHeadersPreservesNativeGatewayHintsOnly(t *testing.T) {
	for _, test := range []struct {
		name         string
		requestClass string
		confirmed    bool
	}{
		{name: "native main", requestClass: "main", confirmed: true},
		{name: "native auxiliary", requestClass: "auxiliary", confirmed: true},
		{name: "unconfirmed", requestClass: "main"},
	} {
		t.Run(test.name, func(t *testing.T) {
			incoming := http.Header{
				"x-claude-code-request-class":       {test.requestClass},
				"x-claude-code-agent-type":          {"builtin"},
				"x-claude-code-prev-tool-durations": {"5"},
				"x-claude-code-compaction":          {"false"},
				"x-claude-code-context-compacted":   {"false"},
			}
			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			auth := fixtureAuth()
			if err := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil,
				[]byte(`{"model":"claude-opus-5-5"}`), &config.Config{}, incoming, test.confirmed); err != nil {
				t.Fatal(err)
			}
			for header, values := range incoming {
				got := req.Header.Get(header)
				if test.confirmed && got != values[0] || !test.confirmed && got != "" {
					t.Errorf("confirmed=%t: %s = %q", test.confirmed, header, got)
				}
			}
		})
	}
}

// Managed betas stay governed by the assembled baseline on direct Anthropic:
// one whose gating excludes the request (effort on a Haiku model) is not
// reinstated just because the caller asked for it.
func TestApplyClaudeHeaders_StillGatesManagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "effort-2025-11-24")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-haiku-4-5"}`), &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if betas := req.Header.Get("Anthropic-Beta"); strings.Contains(betas, "effort-2025-11-24") {
		t.Fatalf("Anthropic-Beta = %q, want gated effort beta kept off the Haiku request", betas)
	}
}

// OAuth credentials preserve unknown caller betas alongside model-gated betas.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{
		ID:       "claude-oauth-fixture",
		Metadata: map[string]any{"access_token": "sk-ant-oat-fixture"},
	}
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "message-threads-2026-08-12,mid-conversation-system-2026-04-07")
	body := []byte(`{
		"model": "claude-fable-5-1",
		"max_tokens": 16,
		"messages": [
			{"role": "user",   "content": "hi"},
			{"role": "system", "content": [], "output_config": {"effort": "low"}},
			{"role": "user",   "content": "say ok"}
		]
	}`)
	if errHeaders := applyClaudeHeaders(req, auth, "sk-ant-oat-fixture", false, nil, body, &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.Contains(betas, "message-threads-2026-08-12") ||
		!strings.Contains(betas, claudeMidConvSystemBeta+","+claudePerTurnControlBeta+","+claudeMidConvToolChangesBeta) {
		t.Fatalf("Anthropic-Beta = %q, want unknown beta plus OAuth model-gated betas", betas)
	}
}

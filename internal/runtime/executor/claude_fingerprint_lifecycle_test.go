package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestClaudeFingerprintSelectedRouteCloaking(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "synthetic-api-key", FingerprintProfile: "claude-code-cli", Cloak: &config.CloakConfig{Mode: "never"}},
		{APIKey: "synthetic-api-key", BaseURL: "https://gateway.invalid", FingerprintProfile: "claude-code-cli", Cloak: &config.CloakConfig{Mode: "always"}},
		{APIKey: "synthetic-api-key", BaseURL: "https://gateway.invalid", FingerprintProfile: "claude-code-cli", Cloak: &config.CloakConfig{Mode: "never"}},
	}}
	for i, entry := range cfg.ClaudeKey {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			a := fingerprintAuth(entry.FingerprintProfile, entry.BaseURL)
			a.Attributes[cliproxyauth.AttributeConfigIndex] = strconv.Itoa(i)
			if got := resolveClaudeKeyConfig(cfg, a); got != &cfg.ClaudeKey[i] {
				t.Error("resolved a different route")
			}
			cr, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), a, fingerprintRequest(fingerprintPayload), fingerprintOptions(), false)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(cr.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := bytes.Contains(body, []byte(claudeCodeCLIIdentity)); got != (entry.Cloak.Mode == "always") {
				t.Fatalf("selected cloak=%s, wire cloaked=%v", entry.Cloak.Mode, got)
			}
		})
	}
}

func TestClaudeFingerprintRouteIdentityMismatch(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "synthetic-api-key", BaseURL: "https://gateway.invalid", FingerprintProfile: "claude-code-cli"},
	}}
	for _, test := range []struct{ name, field, value string }{
		{"key case", "api_key", "SYNTHETIC-API-KEY"},
		{"base missing", "base_url", ""},
		{"base path", "base_url", "https://gateway.invalid/other"},
		{"profile", "fingerprint_profile", ""},
		{"invalid profile", "fingerprint_profile", "unknown"},
		{"invalid index", cliproxyauth.AttributeConfigIndex, "bad"},
		{"stale index", cliproxyauth.AttributeConfigIndex, "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := fingerprintAuth("claude-code-cli", "https://gateway.invalid")
			a.Attributes[cliproxyauth.AttributeConfigIndex] = "0"
			a.Attributes[test.field] = test.value
			if resolveClaudeKeyConfig(cfg, a) != nil {
				t.Fatal("mismatched identity selected a configured route")
			}
		})
	}
	a := fingerprintAuth("claude-code-cli", "https://gateway.invalid")
	if resolveClaudeKeyConfig(cfg, a) != &cfg.ClaudeKey[0] {
		t.Fatal("unique unindexed route not found")
	}
	cfg.ClaudeKey = append(cfg.ClaudeKey, cfg.ClaudeKey[0])
	cfg.ClaudeKey[1].Cloak = &config.CloakConfig{Mode: "never"}
	if resolveClaudeKeyConfig(cfg, a) != nil {
		t.Fatal("ambiguous unindexed route selected the first match")
	}
}

func TestClaudeFingerprintPayloadRulesReclassifyHelpers(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "claude-opus-4-7", Protocol: "claude"}}
	for _, test := range []struct {
		name, payload       string
		params              map[string]any
		filters             []string
		helper, diagnostics bool
	}{
		{"normal to probe", `{"max_tokens":100,"messages":[{"role":"user","content":"probe"}]}`, map[string]any{"max_tokens": 1}, nil, true, false},
		{"probe to normal", `{"max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`, map[string]any{"max_tokens": 100}, nil, false, true},
		{"probe receives tools", `{"max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`, map[string]any{"tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}}, nil, false, true},
		{"tools removed creates probe", `{"max_tokens":1,"messages":[{"role":"user","content":"probe"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, nil, []string{"tools"}, true, false},
		{"probe keeps caller diagnostics", `{"max_tokens":1,"diagnostics":{"caller":true},"messages":[{"role":"user","content":"probe"}]}`, nil, nil, true, true},
		{"probe keeps rule diagnostics", `{"max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`, map[string]any{"diagnostics": map[string]any{"rule": true}}, nil, true, true},
		{"normal respects deleted diagnostics", fingerprintPayload, nil, []string{"diagnostics"}, false, false},
		{"title helper survives system relocation", `{"system":"Return a short title for this coding session","max_tokens":100,"messages":[{"role":"user","content":"example session"}]}`, nil, nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			if test.params != nil {
				cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: test.params}}
			}
			if test.filters != nil {
				cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: test.filters}}
			}
			cr, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), fingerprintAuth("claude-code-cli", "https://api.anthropic.com"), fingerprintRequest(test.payload), fingerprintOptions(), false)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(cr.httpReq.Body)
			if (cr.diagnosticsState.key == "") != test.helper {
				t.Fatalf("helper state=%+v body=%s", cr.diagnosticsState, body)
			}
			if gjson.GetBytes(body, "diagnostics").Exists() != test.diagnostics {
				t.Fatalf("diagnostics=%s body=%s", gjson.GetBytes(body, "diagnostics").Raw, body)
			}
			if test.helper && strings.Contains(gjson.GetBytes(body, "system.0.text").String(), "cc_prompt_id=") {
				t.Fatal("helper received prompt continuity tag")
			}
		})
	}
}

func TestClaudeFingerprintNativeCallerOwnsCloaking(t *testing.T) {
	payload, _ := sjson.SetBytes([]byte(fingerprintPayload), "metadata.user_id", `{"device_id":"`+strings.Repeat("a", 64)+`","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`)
	opts := fingerprintOptions()
	opts.Headers = http.Header{"User-Agent": {"claude-cli/2.1.258 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}
	a := fingerprintAuth("claude-code-cli", "https://gateway.invalid")
	a.Attributes["cloak_mode"] = "always"
	cr, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(context.Background(), a, fingerprintRequest(string(payload)), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(cr.httpReq.Body)
	if gjson.GetBytes(body, "system").String() != "caller system" || gjson.GetBytes(body, "tools.0.name").String() != "lookup" {
		t.Fatalf("native was cloaked: %s", body)
	}
	if cr.diagnosticsState.key != "" {
		t.Fatal("native caller diagnostics overwritten")
	}
}

func TestClaudeFingerprintPreparedOAuthAndTransportAuth(t *testing.T) {
	e := NewClaudeExecutor(&config.Config{})
	a := fingerprintAuth("", "https://api.anthropic.com")
	a.Attributes["api_key"] = "sk-ant-oat-synthetic"
	if !e.ShouldPrepareRequestAuth(a) {
		t.Fatal("OAuth missing identity was not flagged")
	}
	if _, err := e.prepareClaudeRequest(context.Background(), a, fingerprintRequest(fingerprintPayload), fingerprintOptions(), false); err == nil {
		t.Fatal("missing OAuth identity was synthesized")
	}
	a.Metadata = claudeOAuthWireTestMetadata()
	if e.ShouldPrepareRequestAuth(a) {
		t.Fatal("prepared OAuth identity requested profile again")
	}
	cr, err := e.prepareClaudeRequest(context.Background(), a, fingerprintRequest(fingerprintPayload), fingerprintOptions(), false)
	if err != nil {
		t.Fatal(err)
	}
	if cr.httpReq.Header.Get("Authorization") != "Bearer sk-ant-oat-synthetic" || cr.httpReq.Header.Get("X-Api-Key") != "" {
		t.Fatal("OAuth became API-key auth")
	}
	r, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	if err = e.PrepareRequest(r, a); err != nil || r.Header.Get("Authorization") != "Bearer sk-ant-oat-synthetic" {
		t.Fatalf("PrepareRequest auth=%v err=%v", r.Header, err)
	}
}

func TestClaudeFingerprintCCHPreservesCallerBuffer(t *testing.T) {
	body := []byte(`{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli; cch=abcde;"}],"messages":[{"role":"user","content":"hello"}]}`)
	before := bytes.Clone(body)
	view := gjson.GetBytes(body, "system.0.text").String()
	signed, err := signAnthropicMessagesBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, before) || gjson.GetBytes(body, "system.0.text").String() != view {
		t.Fatal("signing changed caller-owned bytes or views")
	}
	if bytes.Equal(body, signed) {
		t.Fatal("synthetic CCH was not regenerated")
	}
	unsigned := stripClaudeBillingCCH(signed)
	if bytes.Contains(unsigned, []byte("cch=")) {
		t.Fatal("gateway retained CCH")
	}
	if !bytes.Equal(body, before) {
		t.Fatal("removing CCH changed caller-owned bytes")
	}
}

package executor

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var claudeCode220WireHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
}

func newClaudeWireProbeRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	auth := &cliproxyauth.Auth{ID: "wire", Metadata: map[string]any{"access_token": "sk-ant-oat01-wire"}}
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader("{}"))
	req.Header = http.Header{}
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	if errHeaders := applyClaudeHeaders(
		req,
		auth,
		"sk-ant-oat01-wire",
		false,
		nil,
		body,
		nil,
		nil,
		false,
	); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders: %v", errHeaders)
	}
	applyClaudeWireHeaderCasing(req)
	return req
}

func serializedClaudeHeaderNames(t *testing.T, req *http.Request) []string {
	t.Helper()
	var output bytes.Buffer
	if errWrite := req.Write(&output); errWrite != nil {
		t.Fatalf("write request: %v", errWrite)
	}
	var names []string
	scanner := bufio.NewScanner(&output)
	scanner.Scan()
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		name, _, found := strings.Cut(line, ":")
		if !found {
			t.Fatalf("malformed header line %q", line)
		}
		names = append(names, name)
	}
	return names
}

func TestApplyClaudeHeadersLeavesHeadersCanonicalBeforeSend(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "wire", Metadata: map[string]any{"access_token": "sk-ant-oat01-wire"}}
	req := httptest.NewRequest(
		http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader("{}"),
	)
	req.Header = http.Header{}
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	if errHeaders := applyClaudeHeaders(
		req,
		auth,
		"sk-ant-oat01-wire",
		false,
		nil,
		body,
		nil,
		nil,
		false,
	); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders: %v", errHeaders)
	}
	for canonical := range claudeWireHeaderCasing {
		if req.Header.Get(canonical) == "" {
			t.Fatalf("%s is unreadable through Header.Get before send", canonical)
		}
	}
}

func TestClaudeWireHeaderCasingMatches220Capture(t *testing.T) {
	req := newClaudeWireProbeRequest(t, "https://api.anthropic.com/v1/messages?beta=true")
	got := serializedClaudeHeaderNames(t, req)
	transportOwned := map[string]bool{
		"Host":            true,
		"Content-Length":  true,
		"Connection":      true,
		"Accept-Encoding": true,
		"User-Agent":      true,
	}
	var sdkNames []string
	for _, name := range got {
		if !transportOwned[name] {
			sdkNames = append(sdkNames, name)
		}
	}
	if len(sdkNames) != len(claudeCode220WireHeaderOrder) {
		t.Fatalf("header count = %d, want %d\n got %v", len(sdkNames), len(claudeCode220WireHeaderOrder), sdkNames)
	}
	for index := range claudeCode220WireHeaderOrder {
		if sdkNames[index] != claudeCode220WireHeaderOrder[index] {
			t.Fatalf(
				"wire header %d = %q, want %q\n got  %v\n want %v",
				index,
				sdkNames[index],
				claudeCode220WireHeaderOrder[index],
				sdkNames,
				claudeCode220WireHeaderOrder,
			)
		}
	}
}

func TestClaudeWireHeaderUserAgentRemainsTransportOwned(t *testing.T) {
	req := newClaudeWireProbeRequest(t, "https://api.anthropic.com/v1/messages?beta=true")
	names := serializedClaudeHeaderNames(t, req)
	userAgentIndex, acceptIndex := -1, -1
	for index, name := range names {
		switch name {
		case "User-Agent":
			userAgentIndex = index
		case "Accept":
			acceptIndex = index
		}
	}
	if userAgentIndex == -1 || acceptIndex == -1 {
		t.Fatalf("missing User-Agent or Accept: %v", names)
	}
	if userAgentIndex > acceptIndex {
		t.Fatal("User-Agent now sorts with the SDK header block")
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "claude-cli/") {
		t.Fatalf("User-Agent = %q, want Claude Code identity", got)
	}
}

func TestClaudeWireHeaderOrderIsBytewiseSorted(t *testing.T) {
	sorted := append([]string(nil), claudeCode220WireHeaderOrder...)
	sort.Strings(sorted)
	for index := range sorted {
		if sorted[index] != claudeCode220WireHeaderOrder[index] {
			t.Fatalf(
				"captured order is not bytewise sorted at %d: %q vs %q",
				index,
				claudeCode220WireHeaderOrder[index],
				sorted[index],
			)
		}
	}
}

func TestClaudeWireHeaderCasingIsAnthropicOnly(t *testing.T) {
	req := newClaudeWireProbeRequest(t, "https://api.moonshot.cn/v1/messages")
	for _, name := range serializedClaudeHeaderNames(t, req) {
		if name == "anthropic-beta" || name == "x-app" || name == "X-Stainless-OS" {
			t.Fatalf("Anthropic wire casing leaked to a third-party gateway: %q", name)
		}
	}
	if req.Header.Get("Anthropic-Version") == "" {
		t.Fatal("third-party gateway lost canonical headers")
	}
}

func TestClaudeWireHeaderCasingPreservesValues(t *testing.T) {
	req := newClaudeWireProbeRequest(t, "https://api.anthropic.com/v1/messages?beta=true")
	for canonical, wire := range claudeWireHeaderCasing {
		if _, stillCanonical := req.Header[canonical]; stillCanonical {
			t.Fatalf("%s was not rewritten to %s", canonical, wire)
		}
		if len(req.Header[wire]) == 0 || req.Header[wire][0] == "" {
			t.Fatalf("%s lost its value during rewrite", wire)
		}
	}
}

func TestClaudeExecutorHasSingleUpstreamSendBoundary(t *testing.T) {
	source, errRead := os.ReadFile("claude_executor.go")
	if errRead != nil {
		t.Fatalf("read claude_executor.go: %v", errRead)
	}
	text := string(source)
	if strings.Contains(text, "httpClient.Do(") {
		t.Fatal("claude_executor.go bypasses doClaudeUpstreamRequest")
	}
	if got := strings.Count(text, "doClaudeUpstreamRequest("); got != 3 {
		t.Fatalf("doClaudeUpstreamRequest call sites = %d, want 3", got)
	}
}

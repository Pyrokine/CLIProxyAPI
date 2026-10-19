package helps

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func claudeTitle280TestRequest() (http.Header, []byte) {
	profile := defaultClaudeDeviceProfile(&config.Config{})
	beta := claudeCodeHelperBetaProfile(
		true,
		"structured-outputs-2025-12-15",
		"server-side-fallback-2026-06-01",
		"fallback-credit-2026-06-01",
		"cache-diagnosis-2026-04-07",
	)
	headers := make(http.Header)
	for name, value := range map[string]string{
		"Accept":            "application/json",
		"Accept-Encoding":   "gzip, deflate, br, zstd",
		"Content-Type":      "application/json",
		"User-Agent":        profile.UserAgent,
		"X-App":             "cli",
		"Anthropic-Beta":    beta,
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"X-Claude-Code-Session-Id":                  "11111111-2222-4333-8444-555555555555",
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Package-Version":               profile.PackageVersion,
		"X-Stainless-Runtime-Version":               profile.RuntimeVersion,
		"X-Stainless-OS":                            profile.OS,
		"X-Stainless-Arch":                          profile.Arch,
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Timeout":                       "600",
	} {
		headers.Set(name, value)
	}
	encodedUserID, _ := json.Marshal(validClaudeCodeMetadataUserID)
	payload := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":80,"messages":[{"role":"user","content":"generate title"}],"metadata":{"user_id":` + string(encodedUserID) + `},"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`)
	return headers, payload
}

func TestDetectClaudeCodeRequestAcceptsMeasuredTitle280Helper(t *testing.T) {
	headers, payload := claudeTitle280TestRequest()
	detection := DetectClaudeCodeRequest(headers, payload, false)
	if !detection.Confirmed || !detection.HelperProfile || detection.BetasPresent {
		t.Fatalf("title helper detection = %#v", detection)
	}
}

func TestDetectClaudeCodeRequestUsesMeasuredPatchBoundaries(t *testing.T) {
	for _, tc := range []struct {
		version     string
		standard    bool
		titleHelper bool
	}{
		{version: "2.1.279"},
		{version: "2.1.280", standard: true, titleHelper: true},
		{version: "2.1.281", standard: true},
		{version: "2.2.0"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			userAgent := "claude-cli/" + tc.version + " (external, cli)"
			standardHeaders := confirmedClaudeCodeHeaders()
			standardHeaders.Set("User-Agent", userAgent)
			standard := DetectClaudeCodeRequest(standardHeaders, claudeCodeDetectionPayload(validClaudeCodeMetadataUserID), false)
			if standard.Confirmed != tc.standard {
				t.Fatalf("standard detection = %#v, want confirmed %t", standard, tc.standard)
			}
			helperHeaders, payload := claudeTitle280TestRequest()
			helperHeaders.Set("User-Agent", userAgent)
			helper := DetectClaudeCodeRequest(helperHeaders, payload, false)
			if helper.Confirmed != tc.titleHelper || helper.HelperProfile != tc.titleHelper {
				t.Fatalf("title helper detection = %#v, want confirmed %t", helper, tc.titleHelper)
			}
		})
	}
}

func TestDetectClaudeCodeRequestRejectsTitleBetaWithLegacyStructuredBody(t *testing.T) {
	headers, _ := claudeTitle280TestRequest()
	encodedUserID, err := json.Marshal(validClaudeCodeMetadataUserID)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":[{"type":"text","text":"helper probe"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.258; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Return a short title."}],"tools":[],"metadata":{"user_id":` + string(encodedUserID) + `},"max_tokens":32000,"thinking":{"type":"disabled"},"temperature":1,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)
	legacyHeaders := headers.Clone()
	legacyHeaders.Set("Anthropic-Beta", claudeCodeHelperBetaProfile(true, "structured-outputs-2025-12-15"))
	if detection := DetectClaudeCodeRequest(legacyHeaders, body, false); !detection.Confirmed || !detection.HelperProfile {
		t.Fatalf("legacy helper fixture not recognized: %#v", detection)
	}
	if detection := DetectClaudeCodeRequest(headers, body, false); detection.Confirmed || detection.HelperProfile {
		t.Fatalf("title beta confirmed legacy stream body: %#v", detection)
	}
}

func TestDetectClaudeCodeRequestRejectsUnmeasuredTitleHelpers(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(http.Header, []byte) []byte
		count  bool
	}{
		{name: "extra beta", mutate: func(h http.Header, p []byte) []byte {
			h.Set("Anthropic-Beta", h.Get("Anthropic-Beta")+",unknown-beta")
			return p
		}},
		{name: "wrong beta order", mutate: func(h http.Header, p []byte) []byte {
			h.Set("Anthropic-Beta", strings.Replace(h.Get("Anthropic-Beta"), "server-side-fallback-2026-06-01,fallback-credit-2026-06-01", "fallback-credit-2026-06-01,server-side-fallback-2026-06-01", 1))
			return p
		}},
		{name: "legacy beta", mutate: func(h http.Header, p []byte) []byte {
			h.Set("Anthropic-Beta", claudeCodeHelperBetaProfile(true, "structured-outputs-2025-12-15"))
			return p
		}},
		{name: "wrong token cap", mutate: func(_ http.Header, p []byte) []byte {
			return []byte(strings.Replace(string(p), `"max_tokens":80`, `"max_tokens":81`, 1))
		}},
		{name: "extra schema key", mutate: func(_ http.Header, p []byte) []byte {
			return []byte(strings.Replace(string(p), `"schema":{"type":"object"}`, `"schema":{"type":"object","additionalProperties":false}`, 1))
		}},
		{name: "array content", mutate: func(_ http.Header, p []byte) []byte {
			return []byte(strings.Replace(string(p), `"content":"generate title"`, `"content":[{"type":"text","text":"generate title"}]`, 1))
		}},
		{name: "count_tokens endpoint", count: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers, payload := claudeTitle280TestRequest()
			if test.mutate != nil {
				payload = test.mutate(headers, payload)
			}
			if detection := DetectClaudeCodeRequest(headers, payload, test.count); detection.Confirmed || detection.HelperProfile {
				t.Fatalf("unmeasured helper confirmed: %#v", detection)
			}
		})
	}
}

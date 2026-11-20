package logging

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsMediaForwardingFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 7, 25, 7, 36, 4, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "codex live remote media forwarding started"
	entry.Data["auth_index"] = "auth-index"
	entry.Data["auth_id"] = "auth-fixture\nsecondary"
	entry.Data["credential"] = "Voice credential\nsecondary"
	entry.Data["connection"] = "via socks5 proxy"
	entry.Data["proxy_scheme"] = "socks5"
	entry.Data["remote_transport"] = "tcp"
	entry.Data["media_session_id"] = "media-session-id"
	entry.Data["call_id"] = "call-id"
	entry.Data["peer"] = "remote"
	entry.Data["state"] = "connected"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		`auth_index="auth-index"`,
		`auth_id="auth-fixture\nsecondary"`,
		`credential="Voice credential\nsecondary"`,
		`connection="via socks5 proxy"`,
		`proxy_scheme="socks5"`,
		`remote_transport="tcp"`,
		`media_session_id="media-session-id"`,
		`call_id="call-id"`,
		`peer="remote"`,
		`state="connected"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %s", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("formatted line contains an unescaped newline: %q", line)
	}
}

func TestLogFormatterPrintsPluginFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Data = log.Fields{
		"plugin_id":       "sample-provider",
		"plugin_name":     "Sample Provider",
		"version":         "0.2.0",
		"active_version":  "0.1.0",
		"retired_version": "0.2.0",
		"path":            "plugins/windows/amd64/sample-provider-v0.2.0.dll",
		"active_path":     "plugins/windows/amd64/sample-provider-v0.1.0.dll",
		"retired_path":    "plugins/windows/amd64/sample-provider-v0.2.0.dll",
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range entry.Data {
		if want := key + "=" + value.(string); !strings.Contains(string(formatted), want) {
			t.Fatalf("formatted line %q missing %q", formatted, want)
		}
	}
}

func TestLogFormatterOmitsGenericPathFields(t *testing.T) {
	for _, pluginID := range []any{nil, "", " \t", 12} {
		entry := log.NewEntry(log.New())
		entry.Data = log.Fields{
			"plugin_id":    pluginID,
			"path":         "generic-path",
			"active_path":  "generic-active-path",
			"retired_path": "generic-retired-path",
		}
		formatted, err := (&LogFormatter{}).Format(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(formatted), "path=") {
			t.Fatalf("generic paths were exposed for plugin ID %v: %s", pluginID, formatted)
		}
	}
}

func TestLogFormatterEscapesPluginFieldControls(t *testing.T) {
	for _, field := range []string{
		"plugin_id", "plugin_name", "version", "active_version", "retired_version", "path", "active_path",
		"retired_path",
	} {
		t.Run(
			field, func(t *testing.T) {
				value := "plugin\nsecondary\r\t\x1b\x00" + string([]rune{0x2028, 0x202e})
				entry := log.NewEntry(log.New())
				entry.Data["plugin_id"] = "sample-provider"
				entry.Data[field] = value
				formatted, err := (&LogFormatter{}).Format(entry)
				if err != nil {
					t.Fatal(err)
				}
				line := string(formatted)
				if strings.Count(line, "\n") != 1 || !strings.Contains(line, field+"="+strconv.Quote(value)) {
					t.Fatalf("plugin field contains unescaped controls: %q", line)
				}
			},
		)
	}
}

func TestLogFormatterUsesExplicitUTCForLocalTimes(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 11, 6, 8, 0, 0, 123456789, time.FixedZone("fixture-local", 8*60*60))
	entry.Message = "fixture log"
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "[2026-11-06T00:00:00.123456789Z]"
	if !strings.HasPrefix(string(formatted), prefix) {
		t.Fatalf("local timestamp was not converted to explicit UTC: %s", formatted)
	}
	parsed, err := time.Parse(time.RFC3339Nano, string(formatted[1:len(prefix)-1]))
	if err != nil || !parsed.Equal(entry.Time) {
		t.Fatalf("formatted timestamp changed the instant: parsed=%v err=%v", parsed, err)
	}
}

func TestLogFormatterPrintsSignatureDiagnosticsWithoutPayload(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Data = log.Fields{
		"component": "signature_sanitizer", "target_provider": "gemini",
		"actions": "drop_signature=2", "contents_path": "contents",
		"sanitized_count": 2, "dropped_signatures": 2, "replaced_signatures": 0,
		"positions": "contents[0].parts[1],contents[1].parts[0]", "omitted_positions": 0,
		"signature": "fixture-private-signature", "payload": "fixture-private-payload",
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	text := string(formatted)
	for _, key := range []string{
		"component", "target_provider", "actions", "contents_path", "sanitized_count",
		"dropped_signatures", "replaced_signatures", "positions", "omitted_positions",
	} {
		if !strings.Contains(text, key+"="+fmt.Sprint(entry.Data[key])) {
			t.Errorf("signature diagnostics missing %s: %s", key, text)
		}
	}
	if strings.Contains(text, "fixture-private-") {
		t.Fatalf("signature payload was exposed: %s", text)
	}
}

func TestLogFormatterEscapesControlsInTypedFields(t *testing.T) {
	for _, value := range []any{errors.New("fixture\nforged\r\x1b"), []string{"contents[0]\nforged"}} {
		entry := log.NewEntry(log.New())
		entry.Data["error"] = value
		formatted, err := (&LogFormatter{}).Format(entry)
		if err != nil {
			t.Fatal(err)
		}
		text := string(formatted)
		if strings.Count(text, "\n") != 1 || strings.ContainsAny(text, "\r\x1b") ||
			!strings.Contains(text, "error="+strconv.Quote(fmt.Sprint(value))) {
			t.Fatalf("typed field contains unescaped controls: %q", text)
		}
	}
}

func TestLogFormatterPreservesFullRequestID(t *testing.T) {
	const requestID = "018f3a5b-1234-7abc-def0-12345678abcd"
	entry := log.NewEntry(log.New())
	entry.Data["request_id"] = requestID
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), "["+requestID+"]") {
		t.Fatalf("full request ID was changed: %s", formatted)
	}
}

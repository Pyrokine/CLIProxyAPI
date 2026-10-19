package logging

import (
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
	for _, field := range []string{"plugin_id", "plugin_name", "version", "active_version", "retired_version", "path", "active_path", "retired_path"} {
		t.Run(field, func(t *testing.T) {
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
		})
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

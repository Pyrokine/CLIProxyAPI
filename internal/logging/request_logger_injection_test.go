package logging

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEscapeLogField(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{name: "empty"},
		{name: "normal", input: "/v1/responses?model=demo&limit=10", want: "/v1/responses?model=demo&limit=10"},
		{name: "controls", input: "a\r\n\x1b\t\x00b", want: `a\r\n\x1b\t\x00b`},
		{name: "quotes and backslashes", input: "\"\\\\n", want: `\"\\\\n`},
		{name: "invalid UTF-8", input: string([]byte{0xff, '\n'}), want: `\xff\n`},
		{name: "long", input: strings.Repeat("long-", 100) + "tail", want: strings.Repeat("long-", 100) + "tail"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if got := escapeLogField(test.input); got != test.want {
					t.Fatalf("escapeLogField(%q) = %q, want %q", test.input, got, test.want)
				}
			},
		)
	}
}

func TestWriteRequestInfoWithBodyEscapesFields(t *testing.T) {
	const payload = "forged\r\n=== RESPONSE ===\nStatus: 200\x1b[2J\"\\"
	const escaped = `forged\r\n=== RESPONSE ===\nStatus: 200\x1b[2J\"\\`
	for _, field := range []string{"URL", "Method", "header name", "header value"} {
		t.Run(
			field, func(t *testing.T) {
				url, method := "/v1/responses", http.MethodPost
				key, value := "X-Trace", "normal"
				var want string
				switch field {
				case "URL":
					url, want = payload, "URL: "+escaped+"\n"
				case "Method":
					method, want = payload, "Method: "+escaped+"\n"
				case "header name":
					key, want = payload, escaped+": normal\n"
				case "header value":
					value, want = payload, "X-Trace: "+escaped+"\n"
				}
				var output bytes.Buffer
				if err := writeRequestInfoWithBody(
					&output, url, method, map[string][]string{key: {value}}, []byte(`{"model":"demo"}`), time.Now(),
				); err != nil {
					t.Fatal(err)
				}
				text := output.String()
				if strings.ContainsAny(text, "\r\x1b") || strings.Contains(
					text, "\n=== RESPONSE ===\n",
				) || !strings.Contains(text, want) {
					t.Fatalf("request %s contains unescaped data or lost the field: %q", field, text)
				}
				for _, section := range []string{
					"=== REQUEST INFO ===\n", "=== HEADERS ===\n", "=== REQUEST BODY ===\n",
				} {
					if strings.Count(text, section) != 1 {
						t.Fatalf("section %q changed: %q", section, text)
					}
				}
			},
		)
	}
}

func TestWriteRequestInfoWithBodyPreservesLongURLAndRedaction(t *testing.T) {
	url := "/v1/responses?model=" + strings.Repeat("model-", 70) + "tail"
	var output bytes.Buffer
	if err := writeRequestInfoWithBody(
		&output, url, http.MethodPost, map[string][]string{
			"Authorization": {"Bearer header-secret-value"},
			"X-Api-Key":     {"api-key-secret-value"},
			"Cookie":        {"cookie-secret-value"},
			"X-Trace":       {"normal"},
		}, []byte(`{"model":"demo","api_key":"body-secret-value"}`), time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	//goland:noinspection SpellCheckingInspection
	for _, want := range []string{
		"URL: " + url + "\n", "Method: POST\n", "X-Trace: normal\n", "Authorization: Bearer head...alue\n",
		//goland:noinspection SpellCheckingInspection
		"X-Api-Key: api-...alue\n", "Cookie: cook...alue\n", `"model": "demo"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("request log missing %q: %q", want, text)
		}
	}
	for _, secret := range []string{
		"header-secret-value", "api-key-secret-value", "cookie-secret-value", "body-secret-value",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("request log leaked %q", secret)
		}
	}
}

func TestWriteResponseSectionEscapesHeaders(t *testing.T) {
	var output bytes.Buffer
	if err := writeResponseSection(
		&output, http.StatusBadGateway, true, map[string][]string{
			"X-Upstream\r\n=== REQUEST INFO ===\x1b": {"value\r\n=== RESPONSE ===\nStatus: 200\x1b\"\\"},
			"Set-Cookie":                             {"cookie-secret-value"},
			"Content-Type":                           {"application/json"},
		}, strings.NewReader(`{"error":"upstream failure"}`), nil, true,
	); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.ContainsAny(text, "\r\x1b") || strings.Count(text, "=== RESPONSE ===\n") != 1 || strings.Contains(
		text, "\n=== REQUEST INFO ===",
	) {
		t.Fatalf("response headers injected log structure: %q", text)
	}
	//goland:noinspection SpellCheckingInspection
	for _, want := range []string{
		`X-Upstream\r\n=== REQUEST INFO ===\x1b: value\r\n=== RESPONSE ===\nStatus: 200\x1b\"\\`, "Status: 502\n",
		"Content-Type: application/json\n", "Set-Cookie: cook...alue\n", `"error": "upstream failure"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("response log missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "cookie-secret-value") {
		t.Fatal("response log leaked cookie")
	}
}

package helps

import (
	"net/url"
	"testing"
)

func TestIsAnthropicUpstreamURL(t *testing.T) {
	tests := []struct {
		name      string
		targetURL string
		want      bool
	}{
		{name: "default HTTPS port", targetURL: "https://api.anthropic.com/v1/messages", want: true},
		{name: "explicit HTTPS port", targetURL: "https://api.anthropic.com:443/v1/messages", want: true},
		{name: "case insensitive host", targetURL: "https://API.ANTHROPIC.COM/v1/messages", want: true},
		{name: "HTTP", targetURL: "http://api.anthropic.com/v1/messages", want: false},
		{name: "custom port", targetURL: "https://api.anthropic.com:8443/v1/messages", want: false},
		{name: "userinfo", targetURL: "https://caller@api.anthropic.com/v1/messages", want: false},
		{name: "lookalike host", targetURL: "https://api.anthropic.com.example/v1/messages", want: false},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				parsed, errParse := url.Parse(test.targetURL)
				if errParse != nil {
					t.Fatal(errParse)
				}
				if got := IsAnthropicUpstreamURL(parsed); got != test.want {
					t.Fatalf("IsAnthropicUpstreamURL(%q) = %t, want %t", test.targetURL, got, test.want)
				}
			},
		)
	}

	if IsAnthropicUpstreamURL(nil) {
		t.Fatal("IsAnthropicUpstreamURL(nil) = true")
	}
}

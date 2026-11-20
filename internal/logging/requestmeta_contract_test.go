package logging

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
)

func TestExtractClientRequestMetadataPreservesResolvedIPTrust(t *testing.T) {
	for _, test := range []struct {
		name      string
		trusted   []string
		forwarded string
		wantIP    string
	}{
		{name: "direct", wantIP: "127.0.0.1"},
		{
			name: "trusted proxy", trusted: []string{"127.0.0.1/32"},
			forwarded: "203.0.113.5, 198.51.100.8", wantIP: "198.51.100.8",
		},
		{name: "untrusted forwarding", forwarded: "203.0.113.5", wantIP: "127.0.0.1"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				request.RemoteAddr = "127.0.0.1:4321"
				if test.forwarded != "" {
					request.Header.Set("X-Forwarded-For", test.forwarded)
				}
				request.Header.Set("User-Agent", "metadata-client/1.0")
				client := access.ResolveClientAddress(request, test.trusted)
				request = request.WithContext(access.WithClientAddress(request.Context(), client))
				metadata := ExtractClientRequestMetadata(request)
				if metadata.ClientIP != test.wantIP || metadata.ResolvedClientIP != test.wantIP {
					t.Fatalf(
						"client metadata = %+v, want resolved IP %q in both compatibility fields",
						metadata,
						test.wantIP,
					)
				}
				if metadata.XForwardedFor != test.forwarded || metadata.UserAgent != "metadata-client/1.0" {
					t.Fatalf("request metadata = %+v", metadata)
				}
				ctx := WithClientRequestMetadata(request.Context(), metadata)
				metadata.ResolvedClientIP = "changed"
				request.Header.Set("X-Forwarded-For", "changed")
				if got := GetClientRequestMetadata(ctx); got.ResolvedClientIP != test.wantIP ||
					got.XForwardedFor != test.forwarded {
					t.Fatalf("stored metadata changed: %+v", got)
				}
			},
		)
	}
}

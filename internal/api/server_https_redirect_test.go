package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPSRedirectPreservesRequestAuthorityAndURI(t *testing.T) {
	for _, test := range []struct {
		name, listen, host, target string
		status                     int
	}{
		{
			name: "wildcard IPv4", listen: "0.0.0.0:8317", host: "public.fixture.invalid:8080",
			target: "https://public.fixture.invalid:8317/v1/models?filter=a%2Fb", status: http.StatusMovedPermanently,
		},
		{
			name: "default HTTPS port", listen: ":443", host: "public.fixture.invalid:80",
			target: "https://public.fixture.invalid:443/v1/models?filter=a%2Fb", status: http.StatusMovedPermanently,
		},
		{
			name: "wildcard IPv6", listen: "[::]:8317", host: "[2001:db8::2]:8080",
			target: "https://[2001:db8::2]:8317/v1/models?filter=a%2Fb", status: http.StatusMovedPermanently,
		},
		{
			name: "specific bind", listen: "127.0.0.1:8317", host: "public.fixture.invalid",
			target: "https://public.fixture.invalid:8317/v1/models?filter=a%2Fb", status: http.StatusMovedPermanently,
		},
		{
			name: "HTTP1.0 specific fallback", listen: "127.0.0.1:8317",
			target: "https://127.0.0.1:8317/v1/models?filter=a%2Fb", status: http.StatusMovedPermanently,
		},
		{
			name: "HTTP1.0 IPv6 fallback", listen: "[::1]:8317", target: "https://[::1]:8317/v1/models?filter=a%2Fb",
			status: http.StatusMovedPermanently,
		},
		{name: "missing public host", listen: "0.0.0.0:8317", status: http.StatusBadRequest},
		{
			name: "invalid host path", listen: "0.0.0.0:8317", host: "public.fixture.invalid/other",
			status: http.StatusBadRequest,
		},
		{
			name: "userinfo is not a host", listen: "0.0.0.0:8317", host: "fixture-user@public.fixture.invalid",
			status: http.StatusBadRequest,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/v1/models?filter=a%2Fb", nil)
				request.Host = test.host
				request.Header.Set("X-Forwarded-Host", "untrusted.fixture.invalid")
				response := httptest.NewRecorder()
				httpsRedirectHandler(test.listen).ServeHTTP(response, request)
				if response.Code != test.status || response.Header().Get("Location") != test.target {
					t.Fatalf(
						"redirect status=%d location=%q, want status=%d location=%q", response.Code,
						response.Header().Get("Location"), test.status, test.target,
					)
				}
			},
		)
	}
}

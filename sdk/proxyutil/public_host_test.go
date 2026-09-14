package proxyutil

import (
	"net/http"
	"testing"
)

type publicHostTestRoundTripper func(*http.Request) (*http.Response, error)

type customPublicHostRoundTripper struct{}

func (f publicHostTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (*customPublicHostRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func TestIsRestrictedCustomHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{name: "Authorization", want: true},
		{name: " authorization ", want: true},
		{name: "Cookie", want: true},
		{name: "Host", want: true},
		{name: "X-API-Key", want: true},
		{name: "X-Custom", want: false},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				if got := IsRestrictedCustomHeader(tt.name); got != tt.want {
					t.Fatalf("IsRestrictedCustomHeader(%q) = %v, want %v", tt.name, got, tt.want)
				}
			},
		)
	}
}

func TestPublicHostTransportBlocksPrivateHost(t *testing.T) {
	t.Parallel()

	called := false
	transport := PublicHostTransport(
		publicHostTestRoundTripper(
			func(*http.Request) (*http.Response, error) {
				called = true
				return nil, nil
			},
		),
	)
	req, errRequest := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if errRequest != nil {
		t.Fatalf("NewRequest returned error: %v", errRequest)
	}
	_, errRoundTrip := transport.RoundTrip(req)
	if errRoundTrip == nil {
		t.Fatal("expected private host error")
	}
	if called {
		t.Fatal("base transport should not be called")
	}
}

func TestPinnedTransportNonTransport(t *testing.T) {
	t.Parallel()

	base := &customPublicHostRoundTripper{}
	got := PinnedTransport(base, "example.com", "1.2.3.4")
	if got != base {
		t.Fatal("non-transport base should be returned unchanged")
	}
}

package quota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestLocalAPICallContextRestoresUpstreamBodyFromWireString(t *testing.T) {
	const upstreamBody = `{"rate_limit":{"allowed":true}}`
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/v0/management/api-call" {
					t.Errorf("request path = %q", req.URL.Path)
				}
				var call APICallRequest
				if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if call.URL != "https://provider.example/quota" {
					t.Errorf("upstream url = %q", call.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(
					localAPICallWireResponse{
						StatusCode: http.StatusOK,
						Header:     map[string][]string{"Content-Type": {"application/json"}},
						Body:       json.RawMessage(strconv.Quote(upstreamBody)),
					},
				)
			},
		),
	)
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	resp, err := NewLocalAPICallContextFunc(port, "")(
		context.Background(), APICallRequest{
			URL:    "https://provider.example/quota",
			Method: http.MethodGet,
		},
	)
	if err != nil {
		t.Fatalf("local API call: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != upstreamBody {
		t.Fatalf("response = %#v, body = %q", resp, resp.Body)
	}
}

func TestLocalAPICallContextStopsWaitingOnCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(_ http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/v0/management/api-call" {
					t.Errorf("request path = %q", req.URL.Path)
				}
				if req.Header.Get("X-Management-Key") != "internal-key" {
					t.Errorf("management key = %q", req.Header.Get("X-Management-Key"))
				}
				started <- struct{}{}
				<-release
			},
		),
	)
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	call := NewLocalAPICallContextFunc(port, "internal-key")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, callErr := call(ctx, APICallRequest{URL: "https://provider.example/quota", Method: http.MethodGet})
		result <- callErr
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("local API call did not start")
	}
	cancel()
	select {
	case callErr := <-result:
		if !errors.Is(callErr, context.Canceled) {
			t.Fatalf("call error = %v, want context cancellation", callErr)
		}
	case <-time.After(time.Second):
		t.Fatal("local API call did not stop after cancellation")
	}
}

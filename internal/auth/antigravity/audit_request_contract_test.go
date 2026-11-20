package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAuditProjectDiscoveryKeepsDistinctRequestShapes(t *testing.T) {
	calls := 0
	client := &http.Client{
		Transport: roundTripperFunc(
			func(req *http.Request) (*http.Response, error) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				metadata, _ := body["metadata"].(map[string]any)
				response := `{"done":true,"response":{"cloudaicompanionProject":"synthetic-project"}}`
				if strings.HasSuffix(req.URL.Path, ":loadCodeAssist") {
					if len(metadata) != 1 || metadata["ideType"] != "ANTIGRAVITY" ||
						req.Header.Get("Accept") != "*/*" ||
						req.Header.Get("X-Goog-Api-Client") != "" {
						t.Fatalf("loadCodeAssist body=%v headers=%v", body, req.Header)
					}
					response = `{"allowedTiers":[{"id":"synthetic-tier","isDefault":true}]}`
				} else {
					if body["tier_id"] !=
						"synthetic-tier" ||
						body["tierId"] !=
							nil ||
						metadata["ide_type"] !=
							"ANTIGRAVITY" ||
						metadata["ide_name"] !=
							"antigravity" {
						t.Fatalf("onboardUser body=%v", body)
					}
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(response)), Request: req,
				}, nil
			},
		),
	}
	project, err := NewAuth(nil, client).FetchProjectID(t.Context(), "synthetic")
	if err != nil || project != "synthetic-project" || calls != 2 {
		t.Fatalf("project=%s calls=%d error=%v", project, calls, err)
	}
}

func TestAuditOnboardPollingObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &http.Client{
		Transport: roundTripperFunc(
			func(req *http.Request) (*http.Response, error) {
				cancel()
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"done":false}`)), Request: req,
				}, nil
			},
		),
	}
	started := time.Now()
	_, err := NewAuth(nil, client).onboardUser(ctx, "synthetic", "synthetic-tier")
	if !errors.Is(err, context.Canceled) || time.Since(started) > time.Second {
		t.Fatalf("elapsed=%s err=%v", time.Since(started), err)
	}
}

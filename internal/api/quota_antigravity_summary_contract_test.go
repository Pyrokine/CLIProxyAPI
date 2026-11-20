package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAntigravityQuotaProjectIDUsesReadOnlyProviderScopedAuth(t *testing.T) {
	for _, test := range []struct {
		name       string
		provider   string
		metadata   map[string]any
		attributes map[string]string
		want       string
	}{
		{
			name: "metadata", provider: "antigravity", metadata: map[string]any{"project_id": " project-meta "},
			want: "project-meta",
		},
		{
			name: "metadata alias", provider: "antigravity", metadata: map[string]any{"projectId": "project-alias"},
			want: "project-alias",
		},
		{
			name: "attributes", provider: "antigravity",
			attributes: map[string]string{"project_id": " project-attribute "}, want: "project-attribute",
		},
		{
			name: "virtual project", provider: "antigravity",
			attributes: map[string]string{"gemini_virtual_project": "virtual-project"}, want: "virtual-project",
		},
		{
			name: "blank metadata", provider: "antigravity", metadata: map[string]any{"project_id": " "},
			attributes: map[string]string{"project_id": "attribute"}, want: "attribute",
		},
		{
			name: "non-string metadata", provider: "antigravity", metadata: map[string]any{"project_id": 42},
			attributes: map[string]string{"project_id": "attribute"}, want: "attribute",
		},
		{
			name: "metadata precedence", provider: "antigravity", metadata: map[string]any{"project_id": "meta"},
			attributes: map[string]string{"project_id": "attribute"}, want: "meta",
		},
		{name: "missing field", provider: "antigravity"},
		{name: "other provider", provider: "codex", metadata: map[string]any{"project_id": "foreign"}},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				auth, err := manager.Register(
					t.Context(), &coreauth.Auth{
						ID: "fixture-project", Provider: test.provider, Metadata: test.metadata,
						Attributes: test.attributes,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				before := manager.List()
				if got := antigravityQuotaProjectID(manager, auth.Index); got != test.want {
					t.Errorf("project=%q, want %q", got, test.want)
				}
				if !reflect.DeepEqual(before, manager.List()) {
					t.Fatal("project lookup mutated auth state")
				}
				if got := antigravityQuotaProjectID(manager, "missing-index"); got != "" {
					t.Errorf("missing target returned %q", got)
				}
			},
		)
	}
	if got := antigravityQuotaProjectID(nil, "index"); got != "" {
		t.Errorf("nil manager returned %q", got)
	}
}

func TestAntigravitySummaryHTTPPreservesGroupedWindowsAndPartialTier(t *testing.T) {
	const grouped = `{"groups":[{"displayName":"Models","description":"Shared limits","buck` +
		`ets":[{"bucketId":"weekly-id","window":"weekly","displayName":"Weekly"` +
		`,"remainingFraction":0,"resetTime":"2026-11-13T00:00:00Z"},{"bucket_id` +
		`":"five-id","window":"5h","remaining_fraction":0.75,"reset_time":"2026` +
		`-11-06T05:00:00Z"}]}],"opaque_integer":9007199254740993}`
	const flat = `{"buckets":[{"modelId":"model","remainingFraction":0.5}]}`
	for _, test := range []struct {
		name      string
		firstBody string
		firstCode int
		quotaBody string
		badTier   bool
		wantCalls int
	}{
		{name: "grouped", quotaBody: grouped, wantCalls: 2},
		{
			name: "secondary after HTTP error", firstCode: http.StatusServiceUnavailable, quotaBody: grouped,
			wantCalls: 3,
		},
		{name: "secondary after invalid schema", firstBody: `{"groups":{}}`, quotaBody: grouped, wantCalls: 3},
		{name: "secondary after invalid JSON", firstBody: `{invalid`, quotaBody: grouped, wantCalls: 3},
		{name: "legacy flat response", quotaBody: flat, wantCalls: 2},
		{name: "invalid optional tier", quotaBody: grouped, badTier: true, wantCalls: 2},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							calls++
							if r.Method != http.MethodPost {
								t.Errorf("method=%s", r.Method)
							}
							if r.URL.Path == "/v1internal:loadCodeAssist" {
								if test.badTier {
									_, _ = io.WriteString(w, `{invalid`)
								} else {
									_, _ = io.WriteString(w, `{"paidTier":{"id":"g1-ultra-tier","name":"Ultra"}}`)
								}
								return
							}
							if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
								t.Errorf("unexpected quota path %s", r.URL.Path)
							}
							var body map[string]string
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
								body["project"] != `fixture-"project` {
								t.Errorf("project request = %v, error=%v", body, err)
							}
							if r.Header.Get("X-Test-Upstream-Host") == "daily-cloudcode-pa.googleapis.com" {
								if test.firstCode != 0 {
									w.WriteHeader(test.firstCode)
									return
								}
								if test.firstBody != "" {
									_, _ = io.WriteString(w, test.firstBody)
									return
								}
							}
							_, _ = io.WriteString(w, test.quotaBody)
						},
					),
				)
				defer server.Close()
				apiCall := func(ctx context.Context, request quota.APICallRequest) (*quota.APICallResponse, error) {
					if request.AuthIndex != "fixture-auth" || request.Header["Authorization"] != "Bearer $TOKEN$" {
						t.Errorf("auth binding or token placeholder changed")
					}
					original, err := url.Parse(request.URL)
					if err != nil {
						return nil, err
					}
					req, err := http.NewRequestWithContext(
						ctx, request.Method, server.URL+original.Path, strings.NewReader(request.Data),
					)
					if err != nil {
						return nil, err
					}
					req.Header.Set("X-Test-Upstream-Host", original.Host)
					response, err := server.Client().Do(req)
					if err != nil {
						return nil, err
					}
					defer func(closeResource func() error) {
						if err := closeResource(); err != nil {
							t.Errorf("close quota fixture response: %v", err)
						}
					}(response.Body.Close)
					body, err := io.ReadAll(response.Body)
					return &quota.APICallResponse{
						StatusCode: response.StatusCode, Header: response.Header, Body: body,
					}, err
				}
				result, err := fetchAntigravityQuotaResultContext(
					t.Context(), apiCall, "fixture-auth", `fixture-"project`,
				)
				if err != nil || calls != test.wantCalls {
					t.Fatalf("calls=%d, error=%v", calls, err)
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(result.Data, &payload); err != nil {
					t.Fatal(err)
				}
				if string(payload["quota"]) != test.quotaBody || len(payload["server_time_offset_ms"]) == 0 {
					t.Fatalf("quota body or server-time metadata changed: %s", result.Data)
				}
				if test.badTier {
					if result.Completeness != quota.FetchPartial || result.PartialError == "" ||
						len(payload["codeAssist"]) != 0 {
						t.Fatalf("optional tier failure discarded quota or hid partial state: %+v", result)
					}
				} else if result.Completeness != quota.FetchComplete || len(payload["codeAssist"]) == 0 {
					t.Fatalf("complete snapshot lost tier: %+v", result)
				}
			},
		)
	}
}

func TestAntigravitySummaryMissingProjectCancellationAndPriorityStatus(t *testing.T) {
	calls := 0
	apiCall := func(context.Context, quota.APICallRequest) (*quota.APICallResponse, error) {
		calls++
		status := http.StatusServiceUnavailable
		if calls == 1 {
			status = http.StatusForbidden
		}
		return &quota.APICallResponse{StatusCode: status}, nil
	}
	if _, err := fetchAntigravityQuotaResultContext(t.Context(), apiCall, "fixture-auth"); err == nil || calls != 0 {
		t.Fatalf("missing project made %d requests, error=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fetchAntigravityQuotaResultContext(ctx, apiCall, "fixture-auth", "project"); !errors.Is(
		err, context.Canceled,
	) || calls != 0 {
		t.Fatalf("cancelled request made %d calls, error=%v", calls, err)
	}
	if _, err := fetchAntigravityQuotaResultContext(
		t.Context(), apiCall, "fixture-auth", "project",
	); err == nil || !strings.Contains(err.Error(), "403") || calls != 3 {
		t.Fatalf("priority failure/candidate count changed: calls=%d, error=%v", calls, err)
	}
}

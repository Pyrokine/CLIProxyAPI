package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
)

func TestClaudeQuotaOptionalProfilePreservesUsageThroughLocalAPICall(t *testing.T) {
	const usage = `{"five_hour":{"utilization":72},"precise":9007199254740993}`
	for _, tc := range []struct {
		name       string
		profile    string
		status     int
		objectWire bool
		badUsage   bool
		partial    bool
	}{
		{name: "valid encoded profile", profile: `{"organization":{"name":"synthetic"}}`, status: http.StatusOK},
		{
			name: "valid legacy object profile", profile: `{"organization":{"name":"synthetic"}}`,
			status: http.StatusOK, objectWire: true,
		},
		{name: "valid null profile", profile: `null`, status: http.StatusOK},
		{name: "HTML profile", profile: `<html>synthetic maintenance</html>`, status: http.StatusOK, partial: true},
		{name: "plain text profile", profile: `synthetic profile unavailable`, status: http.StatusOK, partial: true},
		{name: "truncated profile", profile: `{"organization":`, status: http.StatusOK, partial: true},
		{name: "empty profile", status: http.StatusOK, partial: true},
		{name: "no-content profile", status: http.StatusNoContent, partial: true},
		{name: "rejected profile", profile: `synthetic rejection`, status: http.StatusForbidden, partial: true},
		{name: "invalid usage remains failure", profile: `{}`, status: http.StatusOK, badUsage: true},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							if r.Method != http.MethodPost || r.URL.Path != "/v0/management/api-call" ||
								r.Header.Get("X-Management-Key") != "synthetic-local-key" {
								t.Errorf("unexpected local API-call request: %s %s", r.Method, r.URL.Path)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							var req quota.APICallRequest
							if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
								t.Error(err)
								return
							}
							if req.Method != http.MethodGet || req.AuthIndex != "synthetic-claude" ||
								req.Data != "" {
								t.Errorf("quota request changed identity or method: %+v", req)
							}
							calls.Add(1)
							status, body := http.StatusOK, usage
							switch req.URL {
							case claudeUsageURL:
								if tc.badUsage {
									body = `invalid usage`
								}
							case claudeProfileURL:
								status, body = tc.status, tc.profile
							default:
								t.Errorf("unexpected upstream descriptor: %s", req.URL)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							var wireBody any = body
							if tc.objectWire {
								wireBody = json.RawMessage(body)
							}
							w.Header().Set("Content-Type", "application/json")
							if err := json.NewEncoder(w).Encode(
								map[string]any{
									"status_code": status, "body": wireBody,
								},
							); err != nil {
								t.Error(err)
							}
						},
					),
				)
				defer server.Close()
				address, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				port, err := strconv.Atoi(address.Port())
				if err != nil {
					t.Fatal(err)
				}
				result, err := fetchClaudeQuotaResultContext(
					t.Context(),
					quota.NewLocalAPICallContextFunc(port, "synthetic-local-key"),
					"synthetic-claude",
				)
				if calls.Load() != 2 {
					t.Fatalf("local API calls=%d, want 2", calls.Load())
				}
				if tc.badUsage {
					if err == nil || len(result.Data) != 0 {
						t.Fatalf("invalid required usage accepted: result=%+v error=%v", result, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("optional profile discarded successful usage: %v", err)
				}
				var payload map[string]json.RawMessage
				if err = json.Unmarshal(result.Data, &payload); err != nil {
					t.Fatal(err)
				}
				if string(payload["usage"]) != usage || result.Source != "claude_usage" ||
					result.ObservedAt.IsZero() {
					t.Fatalf("successful usage or observation changed: %+v", result)
				}
				if tc.partial {
					if result.Completeness != quota.FetchPartial || result.PartialError == "" ||
						len(payload["profile"]) != 0 ||
						strings.Contains(
							result.PartialError, tc.profile,
						) && tc.profile != "" {
						t.Fatalf("optional failure is missing or exposes response text: %+v", result)
					}
				} else if result.Completeness !=
					quota.FetchComplete ||
					result.PartialError !=
						"" ||
					string(payload["profile"]) !=
						tc.profile {
					t.Fatalf("valid profile was changed: %+v", result)
				}
			},
		)
	}
}

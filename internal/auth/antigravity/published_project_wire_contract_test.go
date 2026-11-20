package antigravity

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
)

func TestPublishedAntigravityProjectDiscoveryPreservesSuccessfulWire(t *testing.T) {
	for _, tc := range []struct {
		name, loadResponse string
		onboard            bool
	}{
		{"loadCodeAssist", `{"cloudaicompanionProject":"cogent-snow-4mnnp"}`, false},
		{"daily onboardUser", `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`, true},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				calls := 0
				client := &http.Client{
					Transport: roundTripperFunc(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if req.Method !=
								http.MethodPost ||
								req.Header.Get("Authorization") !=
									("Bearer access-"+
										"token") ||
								req.Header.Get("Content-Type") != "application/json" ||
								req.Header.Get("Accept") != "*/*" {
								t.Fatalf("method=%s URL=%s headers=%v", req.Method, req.URL, req.Header)
							}
							var body map[string]any
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							metadata, _ := body["metadata"].(map[string]any)
							response := tc.loadResponse
							switch calls {
							case 1:
								if req.URL.String() != "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
									t.Fatalf("loadCodeAssist URL=%s", req.URL)
								}
								if len(metadata) != 1 || metadata["ideType"] != "ANTIGRAVITY" ||
									req.Header.Get("X-Goog-Api-Client") != "" ||
									req.Header.Get(
										"User-Agent",
									) != misc.AntigravityLoadCodeAssistUserAgent(
										"",
									) {
									t.Fatalf("loadCodeAssist body=%v headers=%v", body, req.Header)
								}
							case 2:
								if !tc.onboard ||
									req.URL.String() !=
										("https://daily-cloudcode-pa.googleapis.com/"+
											"v1internal:onboardUser") {
									t.Fatalf("unexpected onboarding URL=%s", req.URL)
								}
								version, versionOK := metadata["ide_version"].(string)
								if body["tier_id"] != "free-tier" || body["tierId"] != nil || !versionOK ||
									metadata["ide_type"] != "ANTIGRAVITY" || metadata["ide_name"] != "antigravity" ||
									version != misc.AntigravityVersionFromUserAgent(
										req.Header.Get("User-Agent"),
									) {
									t.Fatalf("onboardUser body=%v", body)
								}
								if req.Header.Get("X-Goog-Api-Client") != "gl-node/22.21.1" ||
									req.Header.Get("User-Agent") != misc.AntigravityOnboardUserUserAgent("") {
									t.Fatalf("onboardUser headers=%v", req.Header)
								}
								response = `{"done":true,"response":{"cloudaicompanionProject":{
						"id":"cogent-snow-4mnnp","name":"cogent-snow-4mnnp","projectNumber":"22597072101"
					}}}`
							default:
								t.Fatalf("unexpected project-discovery request %d", calls)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(response)), Request: req,
							}, nil
						},
					),
				}
				project, err := NewAuth(nil, client).FetchProjectID(t.Context(), "access-token")
				wantCalls := 1
				if tc.onboard {
					wantCalls = 2
				}
				if err != nil || project != "cogent-snow-4mnnp" || calls != wantCalls {
					t.Fatalf("project=%q calls=%d want=%d error=%v", project, calls, wantCalls, err)
				}
			},
		)
	}
}

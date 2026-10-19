package management

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestQuotaProbeSummaryCanonicalKeyPrecedence(t *testing.T) {
	payload := []byte(`{"subscription":{"plan":"ProbePro"},"summary":[{"key":"balance","label":"Balance","value":42}],"Summary":"usage text"}`)
	for _, tc := range []struct {
		name string
		load func([]byte) (pluginapi.QuotaFetchResponse, error)
	}{
		{"normalized", normalizeQuotaProbeResponse},
		{"mapped", func(data []byte) (pluginapi.QuotaFetchResponse, error) {
			return mapQuotaProbeResponse(data, map[string]any{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing := 0
			for i := 0; i < 256; i++ {
				response, err := tc.load(payload)
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				if len(response.Summary) != 1 || response.Summary[0].Key != "balance" || response.Summary[0].Value != 42 {
					missing++
				}
			}
			t.Logf("canonical summary omitted in %d/256 decodes", missing)
			if missing != 0 {
				t.Fatalf("canonical lowercase summary must remain authoritative; omitted=%d", missing)
			}
		})
	}
}

func TestQuotaProbeQuotaProbeFinalSummary(t *testing.T) {
	h := &Handler{}
	calls := 0
	missing := 0
	for i := 0; i < 256; i++ {
		response, handled, err := h.executeQuotaProbeWithHTTPDo(context.Background(), &coreauth.Auth{}, map[string]any{
			"url": "https://8.8.8.8/quota",
		}, func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.String() != "https://8.8.8.8/quota" || request.Method != http.MethodGet {
				t.Fatalf("unexpected synthetic request: %v", request)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"subscription":{"plan":"ProbePro"},"summary":[{"key":"balance","label":"Balance","value":42}],"Summary":"usage text"}`))}, nil
		})
		if !handled || err != nil || response.Subscription == nil || response.Subscription.Plan != "ProbePro" {
			t.Fatalf("quota probe handled=%v err=%v response=%#v", handled, err, response)
		}
		if len(response.Summary) != 1 || response.Summary[0].Key != "balance" || response.Summary[0].Value != 42 {
			missing++
		}
	}
	t.Logf("synthetic HTTP calls=%d; final summary omitted=%d/256", calls, missing)
	if missing != 0 {
		t.Fatalf("successful quota probe discarded canonical summary %d times", missing)
	}
}

func TestQuotaSummaryAliasOrdering(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		count   int
		value   float64
	}{
		{"canonical", `{"summary":[{"key":"balance","label":"Balance","value":42}],"Summary":"text"}`, 1, 42},
		{"uppercase", `{"SUMMARY":[{"key":"balance","label":"Balance","value":7}]}`, 1, 7},
		{"canonical-null", `{"summary":null,"Summary":[{"key":"balance","label":"Balance","value":7}]}`, 0, 0},
		{"canonical-empty", `{"summary":[],"Summary":[{"key":"balance","label":"Balance","value":7}]}`, 0, 0},
		{"alias-first-order", `{"Summary":[{"key":"balance","label":"Balance","value":7}],"SUMMARY":[{"key":"balance","label":"Balance","value":3}]}`, 1, 3},
		{"alias-reverse-order", `{"SUMMARY":[{"key":"balance","label":"Balance","value":3}],"Summary":[{"key":"balance","label":"Balance","value":7}]}`, 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 256 {
				got := filterUsableQuotaSummary([]byte(tc.payload))
				if len(got) != tc.count || (tc.count != 0 && got[0].Value != tc.value) {
					t.Fatalf("summary=%#v, want count=%d/value=%v", got, tc.count, tc.value)
				}
			}
		})
	}
}

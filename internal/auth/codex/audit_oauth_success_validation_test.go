package codex

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
)

func TestAuditOAuthSuccessRequiresAccessToken(t *testing.T) {
	for _, flow := range []string{"exchange", "refresh"} {
		for i, payload := range []string{
			`{}`, `null`, `{"refresh_token":"synthetic-new-refresh","expires_in":3600}`, `{"access_token":""}`,
			`{"access_token":"   "}`, `{"error":"invalid_grant"}`,
			`{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`,
		} {
			t.Run(
				fmt.Sprintf("%s/%d", flow, i), func(t *testing.T) {
					calls := 0
					auth := &Auth{
						httpClient: &http.Client{
							Transport: roundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									calls++
									if req.URL.String() != tokenURL {
										return nil, fmt.Errorf("unexpected token route %s", req.URL)
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header),
										Body: io.NopCloser(strings.NewReader(payload)), Request: req,
									}, nil
								},
							),
						},
					}
					var token *TokenData
					var err error
					if flow == "refresh" {
						token, err = auth.RefreshTokensWithRetry(
							t.Context(), fmt.Sprintf("synthetic-%s-%d", flow, i), 3,
						)
					} else {
						var bundle *AuthBundle
						bundle, err = auth.ExchangeCodeForTokensWithRedirect(
							t.Context(), "synthetic-code", redirectURI,
							&misc.PKCECodes{CodeVerifier: "synthetic-verifier"},
						)
						if bundle != nil {
							token = &bundle.TokenData
						}
					}
					if calls != 1 {
						t.Fatalf("token calls=%d, want 1", calls)
					}
					if i < 6 {
						if err == nil || token != nil {
							t.Fatalf("invalid 200 accepted: token=%#v error=%v", token, err)
						}
						if strings.Contains(err.Error(), "synthetic-new-refresh") || strings.Contains(
							err.Error(), payload,
						) {
							t.Fatalf("error exposed invalid response: %v", err)
						}
					} else if err !=
						nil ||
						token ==
							nil ||
						token.AccessToken !=
							"synthetic-new-access" ||
						token.RefreshToken !=
							("synthetic-new-"+
								"refresh") ||
						token.PlanType !=
							DefaultPlanType {
						t.Fatalf("valid control=%#v error=%v", token, err)
					}
				},
			)
		}
	}
}

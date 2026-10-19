package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexClientMetadata159(t *testing.T) {
	if defaultClientVersion != "0.159.0" {
		t.Fatalf("defaultClientVersion = %q", defaultClientVersion)
	}
	if !strings.Contains(defaultCodexUserAgent, "codex_cli_rs/0.159.0") {
		t.Fatalf("defaultCodexUserAgent = %q", defaultCodexUserAgent)
	}
}

func TestRefreshedCodexPlanUpdatesAuthMetadata(t *testing.T) {
	makeIDToken := func(plan string) string {
		claims := `{"https://api.openai.com/auth":{"chatgpt_plan_type":"` + plan + `"}}`
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	}
	for _, tc := range []struct {
		name, tokenPlan, idToken, want string
	}{
		{name: "refresh plan", tokenPlan: " team ", idToken: makeIDToken("plus"), want: "team"},
		{name: "id token claim", idToken: makeIDToken("plus"), want: "plus"},
		{name: "missing claim defaults to free", idToken: makeIDToken(""), want: codexauth.DefaultPlanType},
		{name: "invalid token defaults to free", idToken: "invalid", want: codexauth.DefaultPlanType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &coreauth.Auth{Metadata: map[string]any{"plan_type": "stale-plus"}}
			applyRefreshedCodexPlan(auth, &codexauth.TokenData{PlanType: tc.tokenPlan, IDToken: tc.idToken})
			if got := auth.Metadata["plan_type"]; got != tc.want {
				t.Fatalf("metadata plan_type = %v, want %q", got, tc.want)
			}
			if got := auth.Attributes["plan_type"]; got != tc.want {
				t.Fatalf("attribute plan_type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexModelsURL(t *testing.T) {
	got, err := codexModelsURL(" 0.154.0 ")
	if err != nil {
		t.Fatalf("codexModelsURL() error = %v", err)
	}
	want := "https://chatgpt.com/backend-api/codex/models?client_version=0.154.0"
	if got != want {
		t.Fatalf("codexModelsURL() = %q, want %q", got, want)
	}
}

func TestFindCodexAuthSkipsDisabledAndTokenlessCredentials(t *testing.T) {
	disabled := &coreauth.Auth{
		Provider: "codex", Disabled: true,
		Metadata: map[string]any{"access_token": "disabled-token"},
	}
	tokenless := &coreauth.Auth{Provider: "codex"}
	enabled := &coreauth.Auth{Provider: " CODEX ", Metadata: map[string]any{"refresh_token": "refresh-token"}}
	if got := findCodexAuth([]*coreauth.Auth{disabled, tokenless, enabled}); got != enabled {
		t.Fatalf("findCodexAuth() = %p, want %p", got, enabled)
	}
}

func TestFetchModelsUses159MetadataAndFakeTransport(t *testing.T) {
	const accessToken = "codex-access-token-for-test"
	const accountID = "account-test"
	fixture := `{"models":[{"slug":"gpt-6-astra"},{"slug":"gpt-5.6-sol"}]}`
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Query().Get("client_version"); got != defaultClientVersion {
			t.Fatalf("client_version = %q", got)
		}
		if got := req.Header.Get("User-Agent"); got != defaultCodexUserAgent {
			t.Fatalf("User-Agent = %q", got)
		}
		if got := req.Header.Get("Originator"); got != defaultCodexOriginator {
			t.Fatalf("Originator = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("Chatgpt-Account-Id"); got != accountID {
			t.Fatalf("Chatgpt-Account-Id = %q", got)
		}
		return response(http.StatusOK, fixture), nil
	})}
	auth := &coreauth.Auth{Metadata: map[string]any{"account_id": accountID}}

	raw, count, err := fetchModelsWithClient(context.Background(), auth, accessToken, defaultClientVersion, client)
	if err != nil {
		t.Fatalf("fetchModelsWithClient() error = %v", err)
	}
	if count != 2 || string(raw) != fixture {
		t.Fatalf("result = (%s, %d)", raw, count)
	}
}

func TestFetchModelsRejectsCredentialMaterialInOutput(t *testing.T) {
	const accessToken = "codex-secret-token-for-test"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"models":[],"debug":"`+accessToken+`"}`), nil
	})}
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": accessToken}}

	raw, count, err := fetchModelsWithClient(context.Background(), auth, accessToken, defaultClientVersion, client)
	if err == nil {
		t.Fatal("fetchModelsWithClient() error = nil")
	}
	if raw != nil || count != 0 {
		t.Fatalf("result = (%q, %d), want empty", raw, count)
	}
	if strings.Contains(err.Error(), accessToken) {
		t.Fatalf("error leaked access token: %q", err)
	}
}

func TestFetchModelsRedactsTransportAndStatusFailures(t *testing.T) {
	const accessToken = "codex-secret-token-for-test"
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": accessToken}}

	transportFailure := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport rejected " + accessToken)
	})}
	if _, _, err := fetchModelsWithClient(
		context.Background(), auth, accessToken, defaultClientVersion, transportFailure,
	); err == nil {
		t.Fatal("transport error = nil")
	} else if strings.Contains(err.Error(), accessToken) {
		t.Fatalf("transport error leaked access token: %q", err)
	}

	statusFailure := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusUnauthorized, `{"authorization":"Bearer `+accessToken+`"}`), nil
	})}
	if _, _, err := fetchModelsWithClient(
		context.Background(), auth, accessToken, defaultClientVersion, statusFailure,
	); err == nil {
		t.Fatal("status error = nil")
	} else if strings.Contains(err.Error(), accessToken) {
		t.Fatalf("status error leaked access token: %q", err)
	}
}

func TestCountModels(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		raw   string
		count int
	}{
		{name: "models", raw: `{"models":[{"slug":"a"},{"slug":"b"}]}`, count: 2},
		{name: "incomplete model", raw: `{"models":[{"slug":"gpt-5.6-sol"}]}`, count: 1},
		{name: "empty", raw: `{"models":[]}`, count: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			count, err := countModels([]byte(testCase.raw))
			if err != nil {
				t.Fatalf("countModels() error = %v", err)
			}
			if count != testCase.count {
				t.Fatalf("countModels() = %d, want %d", count, testCase.count)
			}
		})
	}
	if _, err := countModels([]byte(`{"models":`)); err == nil {
		t.Fatal("countModels(malformed) error = nil")
	}
	if _, err := countModels([]byte(`{}`)); err == nil {
		t.Fatal("countModels(missing models) error = nil")
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

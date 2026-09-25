package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDefaultAntigravityFetchBaseURLs(t *testing.T) {
	want := []string{
		antigravityBaseURLDaily,
		antigravityBaseURLProd,
		antigravitySandboxBaseURLDaily,
	}
	if got := defaultAntigravityFetchBaseURLs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultAntigravityFetchBaseURLs() = %#v, want %#v", got, want)
	}
}

func TestFindAntigravityAuthSkipsDisabledCredentials(t *testing.T) {
	disabled := &coreauth.Auth{Provider: "antigravity", Disabled: true}
	enabled := &coreauth.Auth{Provider: " Antigravity "}
	if got := findAntigravityAuth([]*coreauth.Auth{nil, disabled, enabled}); got != enabled {
		t.Fatalf("findAntigravityAuth() = %p, want %p", got, enabled)
	}
}

func TestFetchModelsRetriesEndpointBeforeFallback(t *testing.T) {
	const accessToken = "test-antigravity-token"
	baseURLs := []string{"https://daily.invalid", "https://prod.invalid", "https://sandbox.invalid"}
	var mu sync.Mutex
	var calls []string
	dailyAttempts := 0

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, req.URL.Host)
		mu.Unlock()
		if got := req.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Fatalf("Authorization = %q", got)
		}
		if req.URL.Host == "daily.invalid" {
			dailyAttempts++
			if dailyAttempts == 1 {
				return response(http.StatusServiceUnavailable, `{"error":"temporary"}`), nil
			}
			return response(http.StatusOK, antigravityFixture("gemini-3.6-flash")), nil
		}
		return nil, errors.New("unexpected endpoint")
	})}
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": accessToken, "project_id": "project-a"}}

	models, err := fetchModelsFromBaseURLs(context.Background(), auth, baseURLs, client)
	if err != nil {
		t.Fatalf("fetchModelsFromBaseURLs() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "gemini-3.6-flash" {
		t.Fatalf("models = %#v", models)
	}
	wantCalls := []string{"daily.invalid", "daily.invalid"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestFetchModelsUsesDailyProdSandboxOrder(t *testing.T) {
	const accessToken = "test-antigravity-token"
	baseURLs := []string{"https://daily.invalid", "https://prod.invalid", "https://sandbox.invalid"}
	var calls []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Host)
		if req.URL.Host == "sandbox.invalid" {
			return response(http.StatusOK, antigravityFixture("gemini-2.5-flash")), nil
		}
		return response(http.StatusBadGateway, `{"authorization":"Bearer `+accessToken+`"}`), nil
	})}
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": accessToken}}

	models, err := fetchModelsFromBaseURLs(context.Background(), auth, baseURLs, client)
	if err != nil {
		t.Fatalf("fetchModelsFromBaseURLs() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("models = %#v", models)
	}
	wantCalls := []string{
		"daily.invalid", "daily.invalid",
		"prod.invalid", "prod.invalid",
		"sandbox.invalid",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestFetchModelsPreservesAttemptErrorsWithoutCredentialLeak(t *testing.T) {
	const accessToken = "test-antigravity-secret"
	baseURLs := []string{"https://daily.invalid", "https://prod.invalid"}
	call := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call++
		if call == 1 {
			return nil, errors.New("transport rejected " + accessToken)
		}
		return response(http.StatusUnauthorized, `{"token":"`+accessToken+`"}`), nil
	})}
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": accessToken}}

	models, err := fetchModelsFromBaseURLs(context.Background(), auth, baseURLs, client)
	if err == nil {
		t.Fatal("fetchModelsFromBaseURLs() error = nil")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil", models)
	}
	message := err.Error()
	for _, want := range []string{
		"https://daily.invalid attempt 1",
		"https://daily.invalid attempt 2",
		"https://prod.invalid attempt 1",
		"https://prod.invalid attempt 2",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, accessToken) {
		t.Fatalf("error leaked access token: %q", message)
	}
}

func TestFetchModelsRejectsCredentialMaterialInSuccessBody(t *testing.T) {
	const accessToken = "test-antigravity-secret"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"models":{"safe-model":{"displayName":"`+accessToken+`"}}}`), nil
	})}
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": accessToken}}

	models, err := fetchModelsFromBaseURLs(context.Background(), auth, []string{"https://daily.invalid"}, client)
	if err == nil {
		t.Fatal("fetchModelsFromBaseURLs() error = nil")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil", models)
	}
	if strings.Contains(err.Error(), accessToken) {
		t.Fatalf("error leaked access token: %q", err)
	}
}

func TestParseAntigravityModelsFiltersAndSorts(t *testing.T) {
	raw := []byte(`{
		"models": {
			"z-model": {"displayName":"Z", "maxTokens":100, "maxOutputTokens":20},
			"gemini-2.5-pro": {"displayName":"filtered"},
			"a-model": {}
		}
	}`)
	models, err := parseAntigravityModels(raw)
	if err != nil {
		t.Fatalf("parseAntigravityModels() error = %v", err)
	}
	if len(models) != 2 || models[0].ID != "a-model" || models[1].ID != "z-model" {
		t.Fatalf("models = %#v", models)
	}
	if models[0].DisplayName != "a-model" || models[1].ContextLength != 100 || models[1].MaxCompletionTokens != 20 {
		t.Fatalf("model metadata = %#v", models)
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func antigravityFixture(modelID string) string {
	return `{"models":{"` + modelID + `":{"displayName":"Fixture Model","maxTokens":1048576,"maxOutputTokens":8192}}}`
}

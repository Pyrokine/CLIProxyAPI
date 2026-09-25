package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"google.golang.org/protobuf/encoding/protowire"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnabledDevinCredentialsFiltersDisabledAndKeepsStableIdentity(t *testing.T) {
	disabled := devinAuth("disabled.json", "disabled-token-for-test", "disabled-seed", true)
	authA := devinAuth("devin-user-a.json", "enabled-token-a-for-test", "stable-user-a", false)
	authB := devinAuth("devin-user-b.json", "enabled-token-b-for-test", "stable-user-b", false)
	disabledCopy := devinAuth("disabled.json", "disabled-token-for-test", "disabled-seed", true)
	authACopy := devinAuth("devin-user-a.json", "enabled-token-a-for-test", "stable-user-a", false)
	authBCopy := devinAuth("devin-user-b.json", "enabled-token-b-for-test", "stable-user-b", false)

	first, err := enabledDevinCredentials([]*coreauth.Auth{disabled, authB, authA})
	if err != nil {
		t.Fatalf("enabledDevinCredentials() error = %v", err)
	}
	second, err := enabledDevinCredentials([]*coreauth.Auth{authACopy, disabledCopy, authBCopy})
	if err != nil {
		t.Fatalf("enabledDevinCredentials(reordered) error = %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("credential counts = %d/%d, want 2/2", len(first), len(second))
	}
	for index := range first {
		if first[index].authIndex == "" || first[index].authIndex != second[index].authIndex {
			t.Fatalf("auth_index[%d] = %q/%q", index, first[index].authIndex, second[index].authIndex)
		}
		if first[index].identity != second[index].identity {
			t.Fatalf("identity[%d] = %q/%q", index, first[index].identity, second[index].identity)
		}
		if first[index].identity == "disabled.json" {
			t.Fatal("disabled credential was retained")
		}
	}
}

func TestEnabledDevinCredentialsRejectsEnabledTokenlessCredential(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "devin",
		FileName: "tokenless.json",
		Attributes: map[string]string{
			coreauth.AttributeAuthIndexSeed: "tokenless-user",
		},
	}
	credentials, err := enabledDevinCredentials([]*coreauth.Auth{auth})
	if err == nil {
		t.Fatal("enabledDevinCredentials() error = nil")
	}
	if credentials != nil {
		t.Fatalf("credentials = %#v, want nil", credentials)
	}
	if !strings.Contains(err.Error(), "auth_index=") || !strings.Contains(err.Error(), "identity=tokenless.json") {
		t.Fatalf("error = %q", err)
	}
}

func TestFetchAllRawDevinModelsAggregatesEnabledCredentialsWithFakeTransport(t *testing.T) {
	const tokenA = "enabled-token-a-for-test"
	const tokenB = "enabled-token-b-for-test"
	credentials, err := enabledDevinCredentials([]*coreauth.Auth{
		devinAuth("devin-user-b.json", tokenB, "stable-user-b", false),
		devinAuth("disabled.json", "disabled-token-for-test", "disabled-user", true),
		devinAuth("devin-user-a.json", tokenA, "stable-user-a", false),
	})
	if err != nil {
		t.Fatalf("enabledDevinCredentials() error = %v", err)
	}

	var mu sync.Mutex
	var authorizations []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "server.codeium.com" || req.URL.Path != devinGetCliModelConfigsPath {
			t.Fatalf("request URL = %s", req.URL)
		}
		if req.Header.Get("Content-Type") != "application/proto" || req.Header.Get("Connect-Protocol-Version") != "1" {
			t.Fatalf("Connect headers = %#v", req.Header)
		}
		authorization := req.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()

		switch authorization {
		case "Basic " + tokenA + "-" + tokenA:
			return response(http.StatusOK, devinFixture(
				rawDevinModel{UID: "gpt-6-astra-low", Label: "GPT-6 Astra Low", ContextLength: 100000, VendorID: 2},
			)), nil
		case "Basic " + tokenB + "-" + tokenB:
			return response(http.StatusOK, devinFixture(
				rawDevinModel{
					UID: "gpt-6-astra-high", Label: "GPT-6 Astra High",
					ContextLength: 200000, Multimodal: true, VendorID: 2,
				},
				rawDevinModel{
					UID: "glm-5-2-none", Label: "GLM-5.2 None",
					ContextLength: 128000, VendorID: 9,
				},
			)), nil
		default:
			return nil, errors.New("unexpected credential")
		}
	})}
	models, err := fetchAllRawDevinModels(context.Background(), credentials, func(*coreauth.Auth) *http.Client {
		return client
	})
	if err != nil {
		t.Fatalf("fetchAllRawDevinModels() error = %v", err)
	}
	if len(models) != 3 || len(authorizations) != 2 {
		t.Fatalf("models/calls = %d/%d, want 3/2", len(models), len(authorizations))
	}
	for _, authorization := range authorizations {
		if strings.Contains(authorization, "disabled-token-for-test") {
			t.Fatalf("disabled credential was used: %q", authorization)
		}
	}

	aggregated := aggregateModels(models)
	byID := make(map[string]devinModelJSON, len(aggregated))
	for _, model := range aggregated {
		byID[model.ID] = model
	}
	astra := byID["gpt-6-astra"]
	if astra.ContextLength != 200000 || astra.MaxCompletionTokens != helps.DevinDefaultMaxTokens {
		t.Fatalf("Astra limits = %#v", astra)
	}
	if !reflect.DeepEqual(astra.SupportedInputModalities, []string{"text", "image"}) {
		t.Fatalf("Astra modalities = %#v", astra.SupportedInputModalities)
	}
	if astra.Thinking == nil || !reflect.DeepEqual(astra.Thinking.Levels, []string{"low", "high"}) {
		t.Fatalf("Astra thinking = %#v", astra.Thinking)
	}

	encoded, err := json.Marshal(devinCatalogOutput{Devin: aggregated})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, secret := range []string{tokenA, tokenB, "disabled-token-for-test", "Authorization"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("catalog leaked %q: %s", secret, encoded)
		}
	}
}

func TestFetchAllRawDevinModelsPreservesSafeCredentialErrors(t *testing.T) {
	const tokenA = "enabled-token-a-for-test"
	const tokenB = "enabled-token-b-for-test"
	credentials, err := enabledDevinCredentials([]*coreauth.Auth{
		devinAuth("devin-user-a.json", tokenA, "stable-user-a", false),
		devinAuth("devin-user-b.json", tokenB, "stable-user-b", false),
	})
	if err != nil {
		t.Fatalf("enabledDevinCredentials() error = %v", err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.Header.Get("Authorization"), tokenA) {
			return nil, errors.New("transport rejected " + tokenA)
		}
		return response(http.StatusUnauthorized, `{"token":"`+tokenB+`"}`), nil
	})}

	models, err := fetchAllRawDevinModels(context.Background(), credentials, func(*coreauth.Auth) *http.Client {
		return client
	})
	if err == nil {
		t.Fatal("fetchAllRawDevinModels() error = nil")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil", models)
	}
	message := err.Error()
	for _, credential := range credentials {
		if !strings.Contains(message, "auth_index="+credential.authIndex) ||
			!strings.Contains(message, "identity="+credential.identity) {
			t.Errorf("error %q missing credential %s/%s", message, credential.authIndex, credential.identity)
		}
	}
	if strings.Contains(message, tokenA) || strings.Contains(message, tokenB) {
		t.Fatalf("error leaked credential: %q", message)
	}
}

func TestParseRawDevinModelsProtoRejectsMalformedFixture(t *testing.T) {
	if _, err := parseRawDevinModelsProto([]byte{0xff}); err == nil {
		t.Fatal("parseRawDevinModelsProto() error = nil")
	}
}

func TestDevinModelsURL(t *testing.T) {
	got, err := devinModelsURL("https://server.codeium.com/")
	if err != nil {
		t.Fatalf("devinModelsURL() error = %v", err)
	}
	want := "https://server.codeium.com" + devinGetCliModelConfigsPath
	if got != want {
		t.Fatalf("devinModelsURL() = %q, want %q", got, want)
	}
	if _, err = devinModelsURL("not-a-url"); err == nil {
		t.Fatal("devinModelsURL(invalid) error = nil")
	}
}

func devinAuth(fileName, token, seed string, disabled bool) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "devin",
		Disabled: disabled,
		Attributes: map[string]string{
			"session_token":                 token,
			"base_url":                      "https://server.codeium.com",
			coreauth.AttributeAuthIndexSeed: seed,
		},
		Metadata: map[string]any{"type": "devin", "session_token": token, "disabled": disabled},
	}
}

func devinFixture(models ...rawDevinModel) string {
	var payload []byte
	for _, model := range models {
		var entry []byte
		entry = appendStringField(entry, 1, model.Label)
		if model.Multimodal {
			entry = appendVarintField(entry, 5, 1)
		}
		entry = appendVarintField(entry, 10, model.VendorID)
		entry = appendVarintField(entry, 18, uint64(model.ContextLength))
		entry = appendStringField(entry, 22, model.UID)
		payload = protowire.AppendTag(payload, 1, protowire.BytesType)
		payload = protowire.AppendBytes(payload, entry)
	}
	return string(payload)
}

func appendStringField(dst []byte, number protowire.Number, value string) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendString(dst, value)
}

func appendVarintField(dst []byte, number protowire.Number, value uint64) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

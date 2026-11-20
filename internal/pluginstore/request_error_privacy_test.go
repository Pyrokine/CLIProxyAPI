package pluginstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type pluginPrivacyRoundTripper func(*http.Request) (*http.Response, error)

func (f pluginPrivacyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPluginStoreRequestFailureHidesURLAndPreservesCause(t *testing.T) {
	transportFailure := errors.New("fixture network failure")
	client := Client{
		RegistryURL: "https://fixture-user:fixture-password@registry.example/" +
			"catalog.json?signature=fixture-signature#fixture-fragment",
		HTTPClient: &http.Client{
			Transport: pluginPrivacyRoundTripper(
				func(*http.Request) (*http.Response, error) {
					return nil, transportFailure
				},
			),
		},
	}
	_, err := client.FetchRegistry(t.Context())
	if err == nil || !errors.Is(err, transportFailure) {
		t.Fatalf("request error = %v, want preserved network cause", err)
	}
	if _, ok := errors.AsType[*url.Error](err); !ok {
		t.Fatalf("url.Error cause was lost: %T", err)
	}
	for _, secret := range []string{"fixture-user", "fixture-password", "fixture-signature", "fixture-fragment"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("request error disclosed URL component %q: %s", secret, err)
		}
	}
	if !strings.Contains(err.Error(), transportFailure.Error()) {
		t.Fatalf("request error lost its safe diagnostic: %v", err)
	}
}

func TestPluginStoreURLCredentialsUseAuthenticatedResponsePolicy(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "anonymous", true: "URL Basic credentials"}[credentials],
			func(t *testing.T) {
				requestURL := "https://registry.example/catalog.json"
				if credentials {
					requestURL = "https://fixture-user:fixture-password@registry.example/catalog.json"
				}
				const diagnostic = "fixture response contains private credential echo"
				client := Client{
					RegistryURL: requestURL, HTTPClient: &http.Client{
						Transport: pluginPrivacyRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								user, password, authenticated := request.BasicAuth()
								if authenticated != credentials ||
									credentials && (user != "fixture-user" || password != "fixture-password") {
									t.Errorf(
										"actual Basic authentication = %q/%q/%t",
										user,
										password,
										authenticated,
									)
								}
								return &http.Response{
									StatusCode: http.StatusUnauthorized, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(diagnostic)), Request: request,
								}, nil
							},
						),
					},
				}
				_, err := client.FetchRegistry(context.Background())
				if err == nil || !strings.Contains(err.Error(), "unexpected status 401") {
					t.Fatalf("HTTP status diagnostic = %v", err)
				}
				if strings.Contains(err.Error(), diagnostic) == credentials {
					t.Fatalf(
						"credential-aware error body policy mismatch: credentials=%t, error=%v",
						credentials,
						err,
					)
				}
			},
		)
	}
}

func TestPluginStoreURLCredentialsParticipateInEffectiveAuthIdentity(t *testing.T) {
	client := Client{}
	firstURL := "https://fixture-user:first-fixture-password@registry.example/catalog.json"
	secondURL := "https://fixture-user:second-fixture-password@registry.example/catalog.json"
	first, firstAuthenticated, err := client.authHeaders(firstURL, RequestKindRegistry)
	if err != nil {
		t.Fatal(err)
	}
	second, secondAuthenticated, err := client.authHeaders(secondURL, RequestKindRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !firstAuthenticated || !secondAuthenticated || !AuthConfigured(
		nil, firstURL, RequestKindRegistry,
	) || first.Get("Authorization") == "" {
		t.Fatal("URL credentials were not included in effective authentication")
	}
	if requestIdentity("fixture", first, firstAuthenticated) == requestIdentity(
		"fixture", second, secondAuthenticated,
	) {
		t.Fatal("distinct URL credentials shared a request identity")
	}
	t.Setenv("FIXTURE_PLUGIN_STORE_TOKEN", "synthetic-fixture-token")
	client.Auth = []AuthConfig{
		{
			Match: "https://registry.example/", Type: AuthTypeBearer, TokenEnv: "FIXTURE_PLUGIN_STORE_TOKEN",
		},
	}
	headers, authenticated, err := client.authHeaders(firstURL, RequestKindRegistry)
	if err != nil || !authenticated || headers.Get("Authorization") != "Bearer synthetic-fixture-token" {
		t.Fatalf(
			"explicit authentication precedence: authenticated=%t, headers=%v, error=%v",
			authenticated,
			headers,
			err,
		)
	}
}

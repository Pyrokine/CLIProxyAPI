package registry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type catalogFixtureRoundTripper func(*http.Request) (*http.Response, error)

func (transport catalogFixtureRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func newCatalogTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	local, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	publicHost := "8.8.8.8:" + local.Port()
	//goland:noinspection HttpUrlsUsage
	server.URL = "http://" + publicHost
	base := http.DefaultTransport
	http.DefaultTransport = catalogFixtureRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != publicHost {
				return base.RoundTrip(request)
			}
			localRequest := request.Clone(request.Context())
			localRequest.URL.Host = local.Host
			return base.RoundTrip(localRequest)
		},
	)
	t.Cleanup(func() { http.DefaultTransport = base })
	return server
}

func TestCatalogFetchRejectsPrivateInitialAndRedirectTargets(t *testing.T) {
	base := http.DefaultTransport
	originalModels, originalDevin, originalCodex := modelsURLs, devinModelsURLs, codexClientModelsURLs
	t.Cleanup(
		func() {
			http.DefaultTransport = base
			modelsURLs, devinModelsURLs, codexClientModelsURLs = originalModels, originalDevin, originalCodex
		},
	)
	for _, target := range []string{
		"http://127.0.0.1/catalog", "http://10.0.0.1/catalog", "http://169.254.169.254/latest/meta-data",
		"http://[::1]/catalog",
	} {
		t.Run(
			target, func(t *testing.T) {
				calls := 0
				http.DefaultTransport = catalogFixtureRoundTripper(
					func(request *http.Request) (*http.Response, error) {
						calls++
						if request.URL.Hostname() != "8.8.8.8" {
							t.Errorf("private destination reached transport: %s", request.URL)
						}
						return &http.Response{
							StatusCode: http.StatusFound, Header: http.Header{"Location": []string{target}},
							Body: io.NopCloser(strings.NewReader("")),
						}, nil
					},
				)
				if response, err := catalogHTTPClient().Get(target); err == nil {
					_ = response.Body.Close()
					t.Fatalf("private initial target accepted: %s", target)
				}
				if calls != 0 {
					t.Fatalf("private initial target reached base transport %d times", calls)
				}
				modelsURLs = []string{"http://8.8.8.8/models"}
				devinModelsURLs = []string{"http://8.8.8.8/devin"}
				codexClientModelsURLs = []string{"http://8.8.8.8/codex"}
				if data, _ := fetchModelsFromRemote(context.Background(), getModels()); data != nil {
					t.Fatal("model catalog followed private redirect")
				}
				if data, _, err := fetchDevinModelsFromRemote(context.Background()); err == nil || data != nil {
					t.Fatal("Devin catalog followed private redirect")
				}
				if data, _ := fetchCodexClientModelsFromRemote(context.Background()); data != nil {
					t.Fatal("Codex catalog followed private redirect")
				}
				if calls != 3 {
					t.Fatalf("transport calls = %d, want one public request per updater", calls)
				}
			},
		)
	}
}

func TestCatalogDevinInvalidPrimaryUsesValidSecondary(t *testing.T) {
	invalid := newCatalogTestServer(
		t, http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`{"devin":[]}`))
			},
		),
	)
	defer invalid.Close()
	body := `{"devin":[{"id":"valid-secondary","context_length":4096,"max_completion_tokens":1024}]}`
	valid := newCatalogTestServer(
		t, http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(body))
			},
		),
	)
	defer valid.Close()
	original := devinModelsURLs
	devinModelsURLs = []string{invalid.URL, valid.URL}
	t.Cleanup(func() { devinModelsURLs = original })
	data, source, err := fetchDevinModelsFromRemote(context.Background())
	if err != nil || string(data) != body || source != valid.URL {
		t.Fatalf("secondary catalog was not used: data=%s source=%s error=%v", data, source, err)
	}
}

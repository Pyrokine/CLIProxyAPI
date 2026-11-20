package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/gin-gonic/gin"
)

func TestSVGMediaDownloadIsAuthenticatedAttachment(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders(
		[]sdkaccess.Provider{
			&stubProvider{
				keys: map[string]struct{}{
					"svg-owner": {}, "svg-other": {},
				},
			},
		},
	)
	data := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(document.domain)</script><rect width="10" height="10"/></svg>`)
	record, err := server.imageArtifacts.PutBytes(t.Context(), data, "image/svg+xml")
	if err != nil {
		t.Fatal(err)
	}
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodPost, "https://cpa.test/v1/images/generations", nil)
	context.Set("apiKey", "svg-owner")
	contentURL, err := server.ImageArtifactContentURL(context, toImageArtifactRecord(record))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(contentURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key, method string
		status      int
	}{
		{"", http.MethodGet, 401}, {"svg-other", http.MethodGet, 403}, {"svg-owner", http.MethodGet, 200},
		{"svg-owner", http.MethodHead, 200},
	} {
		t.Run(
			test.key+"/"+test.method, func(t *testing.T) {
				request := httptest.NewRequest(test.method, parsed.RequestURI(), nil)
				request.RemoteAddr = "10.0.0.1:12345"
				if test.key != "" {
					request.Header.Set("Authorization", "Bearer "+test.key)
				}
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if test.status != 200 {
					return
				}
				if response.Header().Get("Content-Type") != "image/svg+xml" || response.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(
					response.Header().Get("Content-Disposition"), "attachment;",
				) || !strings.Contains(
					response.Header().Get("Content-Security-Policy"), "sandbox; default-src 'none'",
				) {
					t.Fatalf("unsafe SVG headers: %v", response.Header())
				}
				if test.method == http.MethodGet && !bytes.Equal(response.Body.Bytes(), data) {
					t.Fatal("SVG response changed model output")
				}
			},
		)
	}
}

func TestMetaTranscriptionRouteUsesPublicAuthentication(t *testing.T) {
	server := newTestServer(t)
	server.accessManager.SetProviders([]sdkaccess.Provider{&stubProvider{keys: map[string]struct{}{"voice-client": {}}}})
	for _, test := range []struct {
		key    string
		status int
	}{{"", 401}, {"voice-client", 400}} {
		request := httptest.NewRequest(http.MethodPost, "/v1/asr/transcribe", strings.NewReader("not multipart"))
		request.RemoteAddr = "10.0.0.1:12345"
		if test.key != "" {
			request.Header.Set("Authorization", "Bearer "+test.key)
		}
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("key=%q status=%d body=%s", test.key, response.Code, response.Body.String())
		}
	}
}

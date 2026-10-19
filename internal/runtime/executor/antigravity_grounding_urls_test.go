package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type groundingURLRoundTripper func(*http.Request) (*http.Response, error)

func (f groundingURLRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestResolveAntigravityGroundingURLsResolvesVertexRedirects(t *testing.T) {
	t.Parallel()

	const redirectURL = "https://vertexaisearch.cloud.google.com/grounding-api-redirect/example-token"
	const resolvedURL = "https://example.com/weather"

	var sawRedirectRequest bool
	ctx := util.WithRoundTripper(
		context.Background(), groundingURLRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodHead {
					t.Fatalf("method = %s, want HEAD", req.Method)
				}
				if req.URL.String() != redirectURL {
					t.Fatalf("url = %s, want %s", req.URL.String(), redirectURL)
				}
				sawRedirectRequest = true
				return &http.Response{
					StatusCode: http.StatusFound,
					Header: http.Header{
						"Location": []string{resolvedURL},
					},
					Body: io.NopCloser(strings.NewReader("")),
				}, nil
			},
		),
	)

	input := []byte(`{
		"response": {
			"candidates": [{
				"groundingMetadata": {
					"groundingChunks": [
						{"web": {"uri": "` + redirectURL + `", "title": "Weather"}},
						{"web": {"uri": "https://already.example/source", "title": "Existing"}}
					]
				}
			}]
		}
	}`)

	output := resolveAntigravityGroundingURLs(ctx, nil, nil, input)
	if !sawRedirectRequest {
		t.Fatal("expected resolver to request the vertex redirect")
	}
	if got := gjson.GetBytes(
		output, "response.candidates.0.groundingMetadata.groundingChunks.0.web.uri",
	).String(); got != resolvedURL {
		t.Fatalf("resolved uri = %q, want %q; output=%s", got, resolvedURL, output)
	}
	if got := gjson.GetBytes(
		output, "response.candidates.0.groundingMetadata.groundingChunks.1.web.uri",
	).String(); got != "https://already.example/source" {
		t.Fatalf("non-vertex uri = %q", got)
	}
}

func TestAntigravityTypedSearchResolvesGroundingAtConsumer(t *testing.T) {
	for _, test := range []struct {
		name      string
		format    sdktranslator.Format
		tool      string
		wantCalls int
	}{
		{"claude", sdktranslator.FormatClaude, "web_search_20250305", 1},
		{"responses", sdktranslator.FormatOpenAIResponse, "web_search", 1},
		{"responses_dated", sdktranslator.FormatOpenAIResponse, "web_search_2025_08_26", 1},
		{"preview", sdktranslator.FormatOpenAIResponse, "web_search_preview", 1},
		{"preview_dated", sdktranslator.FormatOpenAIResponse, "web_search_preview_2025_03_11", 1},
		{"ordinary_function", sdktranslator.FormatOpenAI, "function", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), groundingURLRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodHead || req.URL.String() != "https://vertexaisearch.cloud.google.com/grounding-api-redirect/synthetic" {
					return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL)
				}
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://example.com/article"}}, Body: http.NoBody, Request: req}, nil
			}))
			original := []byte(fmt.Sprintf(`{"tools":[{"type":%q,"name":"web_search"}]}`, test.tool))
			response := []byte(`{"response":{"candidates":[{"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://vertexaisearch.cloud.google.com/grounding-api-redirect/synthetic"}}]}}]}}`)
			output := NewAntigravityExecutor(&config.Config{}).resolveWebSearchGroundingURLs(ctx, nil, test.format, original,
				[]byte(`{"requestType":"web_search","request":{"tools":[{"googleSearch":{}}]}}`), response)
			want := "https://vertexaisearch.cloud.google.com/grounding-api-redirect/synthetic"
			if test.wantCalls > 0 {
				want = "https://example.com/article"
			}
			if calls != test.wantCalls || gjson.GetBytes(output, "response.candidates.0.groundingMetadata.groundingChunks.0.web.uri").String() != want {
				t.Fatalf("grounding callbacks=%d, want %d; payload=%s", calls, test.wantCalls, output)
			}
		})
	}
}

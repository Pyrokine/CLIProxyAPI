package registry

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCatalogRefreshReportsItsOwnPublication(t *testing.T) {
	originalTransport, originalURLs := http.DefaultTransport, modelsURLs
	originalData, originalMeta := getModels(), GetCatalogMeta()
	refreshCallbackMu.Lock()
	originalCallback := refreshCallback
	originalPending := append([]string(nil), pendingRefreshChanges...)
	refreshCallback, pendingRefreshChanges = nil, nil
	refreshCallbackMu.Unlock()
	t.Cleanup(
		func() {
			http.DefaultTransport, modelsURLs = originalTransport, originalURLs
			modelsCatalogStore.mu.Lock()
			modelsCatalogStore.data = originalData
			modelsCatalogStore.mu.Unlock()
			modelsCatalogMetaStore.mu.Lock()
			modelsCatalogMetaStore.meta = originalMeta
			modelsCatalogMetaStore.mu.Unlock()
			refreshCallbackMu.Lock()
			refreshCallback, pendingRefreshChanges = originalCallback, originalPending
			refreshCallbackMu.Unlock()
		},
	)

	type failureContextKey struct{}
	modelsURLs = []string{"http://8.8.8.8/fixture-catalog"}
	calls := 0
	http.DefaultTransport = catalogFixtureRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Context().Value(failureContextKey{}) == true {
				return &http.Response{StatusCode: http.StatusBadGateway, Body: http.NoBody}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(
					strings.NewReader(
						`{"gemini":[{"id":"fixture-refresh-model","context_length":4096,` +
							`"max_completion_tokens":` +
							`1024}]}`,
					),
				),
			}, nil
		},
	)

	var nestedError error
	callbacks := 0
	SetModelRefreshCallback(
		func([]string) {
			callbacks++
			_, nestedError = TriggerCatalogRefresh(
				context.WithValue(context.Background(), failureContextKey{}, true),
			)
		},
	)
	meta, err := TriggerCatalogRefresh(context.Background())
	if err != nil {
		t.Errorf("published refresh inherited another attempt's error: %v", err)
	}
	published := getModels()
	if len(published.Gemini) != 1 || published.Gemini[0].ID != "fixture-refresh-model" ||
		published == originalData {
		t.Fatal("successful refresh did not publish the actual catalog")
	}
	if nestedError == nil || callbacks != 1 || meta.LastError == "" {
		t.Fatalf("nested refresh: error=%v callbacks=%d meta=%+v", nestedError, callbacks, meta)
	}

	SetModelRefreshCallback(nil)
	meta, err = TriggerCatalogRefresh(context.Background())
	unchanged := getModels()
	if err != nil || meta.LastError != "" || unchanged == published || len(unchanged.Gemini) != 1 ||
		unchanged.Gemini[0].ID != "fixture-refresh-model" {
		t.Fatalf("valid unchanged catalog did not publish successfully: error=%v meta=%+v", err, meta)
	}
	_, err = TriggerCatalogRefresh(context.WithValue(context.Background(), failureContextKey{}, true))
	if err == nil || getModels() != unchanged || calls != 4 {
		t.Fatalf("failed refresh changed catalog or hid failure: error=%v calls=%d", err, calls)
	}
}

package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestDevinRefreshNotifiesOnlyWhenCatalogChanges(t *testing.T) {
	originalURLs := devinModelsURLs
	devinCatalogStore.mu.Lock()
	originalModels := cloneModelInfos(devinCatalogStore.models)
	originalRaw := append([]byte(nil), devinCatalogStore.rawJSON...)
	originalRevision := devinCatalogStore.revision
	devinCatalogStore.mu.Unlock()
	refreshCallbackMu.Lock()
	originalCallback, originalPending := refreshCallback, pendingRefreshChanges
	var notifications [][]string
	refreshCallback = func(changed []string) {
		notifications = append(notifications, append([]string(nil), changed...))
	}
	pendingRefreshChanges = nil
	refreshCallbackMu.Unlock()
	t.Cleanup(func() {
		devinModelsURLs = originalURLs
		devinCatalogStore.mu.Lock()
		devinCatalogStore.models = originalModels
		devinCatalogStore.rawJSON = originalRaw
		devinCatalogStore.revision = originalRevision
		devinCatalogStore.mu.Unlock()
		refreshCallbackMu.Lock()
		refreshCallback, pendingRefreshChanges = originalCallback, originalPending
		refreshCallbackMu.Unlock()
	})

	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"devin":[{"id":"refresh-notification-test","context_length":100,"max_completion_tokens":20}]}`))
	}))
	defer valid.Close()
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"devin":[]}`))
	}))
	defer invalid.Close()

	devinModelsURLs = []string{valid.URL}
	tryRefreshDevinModels(context.Background(), "test")
	tryRefreshDevinModels(context.Background(), "test")
	devinModelsURLs = []string{invalid.URL}
	tryRefreshDevinModels(context.Background(), "test")
	if !reflect.DeepEqual(notifications, [][]string{{"devin"}}) {
		t.Fatalf("refresh notifications = %v, want one Devin change", notifications)
	}
}

package management

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestMetaKeyManagementCRUD(t *testing.T) {
	handler := newConfigListTestHandler(t, &config.Config{})
	post := runConfigListRequest(handler.PostMetaKey, http.MethodPost, "/meta-api-key", `{"api-key":"fixture-meta-key"}`)
	if post.Code != http.StatusCreated {
		t.Fatalf("POST Meta key: status %d body %s", post.Code, post.Body.String())
	}
	listed := runConfigListRequest(handler.GetMetaKeys, http.MethodGet, "/meta-api-key", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "meta-api-key") ||
		strings.Contains(listed.Body.String(), "fixture-meta-key") {
		t.Fatalf("GET Meta keys: status %d body %s", listed.Code, listed.Body.String())
	}

	patch := runConfigListRequest(handler.PatchMetaKey, http.MethodPatch, "/meta-api-key",
		`{"index":0,"value":{"priority":9,"weight":7,"prefix":"team","disable-cooling":true,"request-retry":2}}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH Meta key: status %d body %s", patch.Code, patch.Body.String())
	}
	updated := handler.configSnapshot().MetaKey
	if len(updated) != 1 || updated[0].APIKey != "fixture-meta-key" || updated[0].Priority != 9 ||
		updated[0].BaseURL != "https://api.meta.ai/v1" || updated[0].Prefix != "team" ||
		updated[0].Weight == nil || *updated[0].Weight != 7 ||
		updated[0].DisableCooling == nil || !*updated[0].DisableCooling ||
		updated[0].RequestRetry == nil || *updated[0].RequestRetry != 2 {
		t.Fatalf("PATCH Meta key lost identity or execution settings: %+v", updated)
	}

	put := runConfigListRequest(handler.PutMetaKeys, http.MethodPut, "/meta-api-key",
		`[{"api-key":"fixture-meta-key","base-url":"https://api.meta.ai/v1","priority":11}]`)
	if put.Code != http.StatusOK || len(handler.configSnapshot().MetaKey) != 1 ||
		handler.configSnapshot().MetaKey[0].Priority != 11 {
		t.Fatalf("PUT Meta keys: status %d body %s", put.Code, put.Body.String())
	}
	deleted := runConfigListRequest(handler.DeleteMetaKey, http.MethodDelete,
		"/meta-api-key?api-key="+url.QueryEscape("fixture-meta-key"), "")
	if deleted.Code != http.StatusOK || len(handler.configSnapshot().MetaKey) != 0 {
		t.Fatalf("DELETE Meta key: status %d body %s", deleted.Code, deleted.Body.String())
	}
}

func TestDeleteMetaKeyRejectsAmbiguousIdentity(t *testing.T) {
	handler := newConfigListTestHandler(t, &config.Config{MetaKey: []config.MetaKey{
		{APIKey: "fixture-shared-key", BaseURL: "https://api.meta.ai/v1"},
		{APIKey: "fixture-shared-key", BaseURL: "https://alternate.example.test/v1"},
	}})
	ambiguous := runConfigListRequest(handler.DeleteMetaKey, http.MethodDelete,
		"/meta-api-key?api-key=fixture-shared-key", "")
	if ambiguous.Code != http.StatusBadRequest || len(handler.configSnapshot().MetaKey) != 2 {
		t.Fatalf("ambiguous DELETE changed Meta credentials: status %d body %s", ambiguous.Code, ambiguous.Body.String())
	}
	scoped := runConfigListRequest(handler.DeleteMetaKey, http.MethodDelete,
		"/meta-api-key?api-key=fixture-shared-key&base-url="+url.QueryEscape("https://alternate.example.test/v1"), "")
	remaining := handler.configSnapshot().MetaKey
	if scoped.Code != http.StatusOK || len(remaining) != 1 || remaining[0].BaseURL != "https://api.meta.ai/v1" {
		t.Fatalf("scoped DELETE removed wrong Meta credential: status %d body %s remaining=%+v", scoped.Code, scoped.Body.String(), remaining)
	}
}

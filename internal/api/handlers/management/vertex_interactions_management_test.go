package management

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestVertexConfigInteractionsManagementPatch(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{
		VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "synthetic-key", BaseURL: "https://vertex-config.invalid"}},
	})
	for _, tc := range []struct {
		name   string
		patch  string
		want   string
		status int
	}{
		{name: "enable", patch: `{"interactions":true}`, want: "true", status: http.StatusOK},
		{name: "unrelated edit", patch: `{"priority":7}`, want: "true", status: http.StatusOK},
		{name: "disable", patch: `{"interactions":false}`, want: "false", status: http.StatusOK},
		{name: "inherit", patch: `{"interactions":null}`, status: http.StatusOK},
		{name: "invalid boolean", patch: `{"interactions":"invalid"}`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := runConfigListRequest(h.PatchVertexCompatKey, http.MethodPatch, "/vertex-api-key", `{"index":0,"value":`+tc.patch+`}`)
			if response.Code != tc.status {
				t.Fatalf("PATCH status = %d, want %d", response.Code, tc.status)
			}
			response = runConfigListRequest(h.GetVertexCompatKeys, http.MethodGet, "/vertex-api-key", "")
			if response.Code != http.StatusOK {
				t.Fatalf("GET status = %d", response.Code)
			}
			var body struct {
				Keys []map[string]json.RawMessage `json:"vertex-api-key"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Keys) != 1 {
				t.Fatalf("GET entries = %d, error = %v", len(body.Keys), err)
			}
			if got := string(body.Keys[0]["interactions"]); got != tc.want {
				t.Fatalf("GET interactions = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVertexConfigInteractionsManagementCreateReplace(t *testing.T) {
	for _, tc := range []struct {
		name string
		flag string
	}{
		{name: "enabled", flag: "true"},
		{name: "disabled", flag: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConfigListTestHandler(t, &config.Config{})
			entry := `{"api-key":"synthetic-key","base-url":"https://vertex-config.invalid","interactions":` + tc.flag + `}`
			response := runConfigListRequest(h.PostVertexCompatKey, http.MethodPost, "/vertex-api-key", entry)
			if response.Code != http.StatusCreated {
				t.Fatalf("POST status = %d", response.Code)
			}
			response = runConfigListRequest(h.PutVertexCompatKeys, http.MethodPut, "/vertex-api-key", `[`+entry+`]`)
			if response.Code != http.StatusOK {
				t.Fatalf("PUT status = %d", response.Code)
			}
			response = runConfigListRequest(h.GetVertexCompatKeys, http.MethodGet, "/vertex-api-key", "")
			var body struct {
				Keys []map[string]json.RawMessage `json:"vertex-api-key"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Keys) != 1 {
				t.Fatalf("GET entries = %d, error = %v", len(body.Keys), err)
			}
			if got := string(body.Keys[0]["interactions"]); got != tc.flag {
				t.Fatalf("GET interactions = %q, want %q", got, tc.flag)
			}
		})
	}
}

package management

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func TestC04XAIAPIKeyManagementCRUDAndValidation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	create := runConfigListRequest(
		h.PostXAIKey, http.MethodPost, "/xai-api-key", `{
		"api-key":"xai-secret","base-url":"https://api.x.ai/v1","websockets":true,
		"disable-cooling":true,"models":[{"name":"grok-4.5","alias":"grok-latest","force-mapping":true}]
	}`,
	)
	if create.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, body=%s", create.Code, create.Body.String())
	}
	if len(h.cfg.XAIKey) != 1 || !h.cfg.XAIKey[0].DisableCooling {
		t.Fatalf("created xAI config = %+v", h.cfg.XAIKey)
	}
	get := runConfigListRequest(h.GetXAIKeys, http.MethodGet, "/xai-api-key", "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", get.Code, get.Body.String())
	}
	if body := get.Body.String(); body == "" || strings.Contains(body, "xai-secret") {
		t.Fatalf("GET response exposed API key: %s", body)
	}

	patch := runConfigListRequest(
		h.PatchXAIKey, http.MethodPatch, "/xai-api-key", `{
		"index":0,"value":{"priority":7,"websockets":false,"disable-cooling":false}
	}`,
	)
	if patch.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body=%s", patch.Code, patch.Body.String())
	}
	if h.cfg.XAIKey[0].Priority != 7 || h.cfg.XAIKey[0].Websockets || h.cfg.XAIKey[0].DisableCooling {
		t.Fatalf("patched xAI config = %+v", h.cfg.XAIKey[0])
	}

	invalid := runConfigListRequest(
		h.PutXAIKeys, http.MethodPut, "/xai-api-key", `[
		{"api-key":"xai-secret","base-url":"ftp://api.x.ai/v1"}
	]`,
	)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d, body=%s", invalid.Code, invalid.Body.String())
	}
	if len(h.cfg.XAIKey) != 1 || h.cfg.XAIKey[0].BaseURL != "https://api.x.ai/v1" {
		t.Fatalf("invalid PUT changed config = %+v", h.cfg.XAIKey)
	}

	deleteRec := runConfigListRequest(
		h.DeleteXAIKey, http.MethodDelete, "/xai-api-key", `{
		"api-key":"`+util.HideAPIKey("xai-secret")+`","base-url":"https://api.x.ai/v1"
	}`,
	)
	if deleteRec.Code != http.StatusOK || len(h.cfg.XAIKey) != 0 {
		t.Fatalf("DELETE status = %d, config=%+v, body=%s", deleteRec.Code, h.cfg.XAIKey, deleteRec.Body.String())
	}
}

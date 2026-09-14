package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestInteractionsKeyManagementCRUDAndStableIdentity(t *testing.T) {
	cfg := &config.Config{
		InteractionsKey: []config.GeminiKey{
			{APIKey: "interactions-first", BaseURL: "https://first.example.com", Prefix: "first"},
			{APIKey: "interactions-second", BaseURL: "https://second.example.com", Prefix: "second"},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      cfg,
			Now:         time.Unix(1_700_000_000, 0),
			IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if err != nil {
		t.Fatalf("synthesize auths: %v", err)
	}
	for _, auth := range auths {
		if _, err = manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth: %v", err)
		}
	}

	h := newConfigListTestHandler(t, cfg)
	h.authManager = manager

	getRecorder := runConfigListRequest(h.GetInteractionsKeys, http.MethodGet, "/interactions-api-key", "")
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", getRecorder.Code, getRecorder.Body.String())
	}
	var getPayload struct {
		Items []geminiKeyWithAuthIndex `json:"interactions-api-key"`
	}
	if err = json.Unmarshal(getRecorder.Body.Bytes(), &getPayload); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(getPayload.Items) != 2 || getPayload.Items[1].AuthIndex == "" {
		t.Fatalf("GET items = %#v, want two entries with stable auth indexes", getPayload.Items)
	}
	if getPayload.Items[0].APIKey == "interactions-first" {
		t.Fatal("GET response exposed the Interactions API key")
	}
	secondAuthIndex := getPayload.Items[1].AuthIndex

	current := h.configSnapshot()
	current.InteractionsKey[0], current.InteractionsKey[1] = current.InteractionsKey[1], current.InteractionsKey[0]
	h.mu.Lock()
	h.setConfigLocked(current)
	h.mu.Unlock()
	patchRecorder := runConfigListRequest(
		h.PatchInteractionsKey,
		http.MethodPatch,
		"/interactions-api-key",
		`{"index":1,"auth-index":"`+secondAuthIndex+`","value":{"prefix":"updated","weight":3}}`,
	)
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", patchRecorder.Code, patchRecorder.Body.String())
	}
	updated := h.configSnapshot().InteractionsKey
	if updated[0].APIKey != "interactions-second" || updated[0].Prefix != "updated" ||
		updated[0].Weight == nil || *updated[0].Weight != 3 {
		t.Fatalf("stable PATCH updated wrong entry: %#v", updated)
	}

	postRecorder := runConfigListRequest(
		h.PostInteractionsKey,
		http.MethodPost,
		"/interactions-api-key",
		`{"api-key":"interactions-third","base-url":"https://third.example.com","weight":2}`,
	)
	if postRecorder.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201: %s", postRecorder.Code, postRecorder.Body.String())
	}
	if len(h.configSnapshot().InteractionsKey) != 3 {
		t.Fatalf("InteractionsKey length = %d, want 3", len(h.configSnapshot().InteractionsKey))
	}

	deleteRecorder := runConfigListRequest(
		h.DeleteInteractionsKey,
		http.MethodDelete,
		"/interactions-api-key?api-key=interactions-third&base-url=https://third.example.com",
		"",
	)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if len(h.configSnapshot().InteractionsKey) != 2 {
		t.Fatalf("InteractionsKey length after DELETE = %d, want 2", len(h.configSnapshot().InteractionsKey))
	}
}

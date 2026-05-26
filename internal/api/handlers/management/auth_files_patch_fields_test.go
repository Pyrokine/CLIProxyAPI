package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestPatchAuthFileFieldsUpdatesHeadersAndNote(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "auth-1",
		Provider:   "claude",
		FileName:   "claude.json",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"header:X-Keep": "old", "header:X-Remove": "old"},
		Metadata: map[string]any{
			"headers": map[string]any{"X-Keep": "old", "X-Remove": "old"},
			"note":    "old note",
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		bytes.NewBufferString(`{"name":"auth-1","headers":{"X-New":"new","X-Remove":""},"note":" updated note "}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	updated, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("updated auth not found")
	}
	headers := coreauth.ExtractCustomHeadersFromMetadata(updated.Metadata)
	if headers["X-Keep"] != "old" || headers["X-New"] != "new" {
		t.Fatalf("headers not merged: %#v", headers)
	}
	if _, exists := headers["X-Remove"]; exists {
		t.Fatalf("empty header was not removed: %#v", headers)
	}
	if updated.Attributes["header:X-Keep"] != "old" || updated.Attributes["header:X-New"] != "new" {
		t.Fatalf("attributes not synced: %#v", updated.Attributes)
	}
	if _, exists := updated.Attributes["header:X-Remove"]; exists {
		t.Fatalf("removed header attribute remains: %#v", updated.Attributes)
	}
	if updated.Metadata["note"] != "updated note" {
		t.Fatalf("note = %#v", updated.Metadata["note"])
	}
}

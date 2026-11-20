package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/google/uuid"
)

func TestAuthMultipartUploadConsumesAllSupportedFileFields(t *testing.T) {
	for _, name := range []string{"mixed fields", "secondary only", "multiple files per field", "partial failure"} {
		t.Run(
			name, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store, authManager: manager}
				engine := resourceMutationEngine(t, h)
				engine.POST("/auth-files", h.UploadAuthFile)
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				fields := []string{"file", "files", "upload"}
				if name == "secondary only" {
					fields = []string{"files", "upload"}
				} else if name == "multiple files per field" {
					fields = []string{"file", "file", "files", "files"}
				}
				var files []string
				for i, field := range fields {
					file := fmt.Sprintf("synthetic-upload-%d.json", i)
					part, err := writer.CreateFormFile(field, file)
					if err != nil {
						t.Fatal(err)
					}
					value := `{"type":"codex","access_token":"synthetic-upload-token"}`
					if name == "partial failure" && i == len(fields)-1 {
						value = `{"type":`
					} else {
						files = append(files, file)
					}
					if _, err := part.Write([]byte(value)); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/auth-files", &body)
				request.Header.Set("Content-Type", writer.FormDataContentType())
				request.Header.Set("X-CPA-Mutation-ID", uuid.NewString())
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				status, outcome := 200, "succeeded"
				if name == "partial failure" {
					status, outcome = 207, "partial"
				}
				if response.Code != status {
					t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
				}
				var payload struct {
					Uploaded int      `json:"uploaded"`
					Files    []string `json:"files"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Uploaded != len(files) || len(payload.Files) != len(files) ||
					len(manager.List()) != len(files) {
					t.Fatalf(
						"uploaded=%d files=%v auths=%d want=%v", payload.Uploaded, payload.Files, len(manager.List()),
						files,
					)
				}
				for _, file := range files {
					if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
						t.Fatalf("uploaded credential absent: %s: %v", file, err)
					}
				}
				if name == "partial failure" {
					failed := fmt.Sprintf("synthetic-upload-%d.json", len(fields)-1)
					if _, err := os.Stat(filepath.Join(dir, failed)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("invalid multipart member persisted: %v", err)
					}
				}
				result := decodeMutationHeader(t, response)
				if result.Outcome != outcome || !result.ConfirmedEffect || len(result.Effects) != 2 {
					t.Fatalf("multipart actual effects=%+v", result)
				}
			},
		)
	}
}

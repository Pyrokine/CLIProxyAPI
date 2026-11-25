package management

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	"github.com/gin-gonic/gin"
)

func TestImportVertexCredentialValidatesLocationAndTokenURL(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(
		pem.EncodeToMemory(
			&pem.Block{
				Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
			},
		),
	)
	for _, tc := range []struct {
		name, form, query, wantLocation, wantError string
		tokenURL                                   any
	}{
		{name: "default region and token URL", wantLocation: "us-central1"},
		{
			name: "query region", query: " europe-west12 ", wantLocation: "europe-west12",
			tokenURL: "https://oauth2.googleapis.com/token",
		},
		{name: "form precedes query", form: "global", query: "us-east4", wantLocation: "global"},
		{name: "invalid region", form: "regional", wantError: "invalid location"},
		{name: "invalid token URL type", tokenURL: 1, wantError: "invalid service account"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				dir := t.TempDir()
				h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: sdkAuth.NewFileTokenStore()}
				sa := map[string]any{
					"type": "service_account", "project_id": "fixture-project", "client_email": "svc@example.test",
					"private_key": privateKey,
				}
				if tc.tokenURL != nil {
					sa["token_uri"] = tc.tokenURL
				}
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				part, err := writer.CreateFormFile("file", "fixture.json")
				if err != nil {
					t.Fatal(err)
				}
				if err := json.NewEncoder(part).Encode(sa); err != nil {
					t.Fatal(err)
				}
				if err := writer.WriteField("location", tc.form); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(response)
				query := url.Values{"location": {tc.query}}.Encode()
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/vertex/import?"+query, &body)
				ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
				h.ImportVertexCredential(ctx)

				path := filepath.Join(dir, "vertex-fixture-project.json")
				if tc.wantError != "" {
					var reply struct{ Error string }
					if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
						t.Fatal(err)
					}
					if response.Code != http.StatusBadRequest || reply.Error != tc.wantError {
						t.Fatalf("import = %d %s", response.Code, response.Body)
					}
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("rejected credential file: %v", err)
					}
					return
				}
				if response.Code != http.StatusOK {
					t.Fatalf("import = %d %s", response.Code, response.Body)
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var saved struct{ Location string }
				if err := json.Unmarshal(raw, &saved); err != nil || saved.Location != tc.wantLocation {
					t.Fatalf("stored location = %q, error = %v; want %q", saved.Location, err, tc.wantLocation)
				}
			},
		)
	}
}

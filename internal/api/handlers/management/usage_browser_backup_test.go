package management

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/gin-gonic/gin"
)

func TestUsageBackupPasswordInBrowserSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	p.Start(t.Context())
	t.Cleanup(p.Stop)
	const seed = `[{"timestamp":"2026-10-15T15:14:00Z","model":"fixture","request_id":"req-backup","tokens":{"input_tokens":1,"total_tokens":1},"failed":false}]`
	if _, err := p.ImportContext(t.Context(), []byte(seed)); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, "", nil, withEnvSecret(roundtripManagementKey), withUsagePersister(p))
	router := gin.New()
	router.Use(h.MiddlewareWithConfig(nil))
	router.POST("/session", h.CreateManagementSession)
	router.GET("/usage/events", h.GetUsageEvents)
	router.GET("/usage/export", h.ExportUsageStatistics)
	router.POST("/usage/import", h.ImportUsageStatistics)

	var session struct {
		Token string `json:"session_token"`
	}
	login := roundtripRequest(router, http.MethodPost, "/session", nil, roundtripManagementKey)
	if err := json.Unmarshal(
		login.Body.Bytes(), &session,
	); err != nil || login.Code != http.StatusCreated || session.Token == "" {
		t.Fatalf("session status=%d err=%v", login.Code, err)
	}
	request := func(method, path string, body []byte, token, keyHeader, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		if token != "" {
			req.Header.Set(ManagementSessionTokenHeader, token)
		}
		if keyHeader != "" {
			req.Header.Set(keyHeader, password)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}
	const window = "/usage/events?from=2026-10-15T00:00:00Z&to=2026-10-16T00:00:00Z"
	const exportEvents = window + "&export=true"
	const password = "备份 пароль 2026"
	encoded := base64.StdEncoding.EncodeToString([]byte(password))
	if denied := request(
		http.MethodGet, "/usage/export", nil, "", usageBackupKeyBase64Header, encoded,
	); denied.Code != http.StatusUnauthorized ||
		strings.Contains(denied.Body.String(), "usage_backup_password_required") {
		t.Fatalf("backup password must not authenticate management requests: status=%d", denied.Code)
	}
	for _, path := range []string{"/usage/export", exportEvents} {
		missing := request(http.MethodGet, path, nil, session.Token, "", "")
		if missing.Code != http.StatusUnauthorized || !strings.Contains(
			missing.Body.String(), "usage_backup_password_required",
		) {
			t.Fatalf("missing backup password: %d %s", missing.Code, missing.Body.String())
		}
	}
	backup := request(http.MethodGet, "/usage/export", nil, session.Token, usageBackupKeyBase64Header, encoded)
	if backup.Code != http.StatusOK || !usage.IsUsageBackup(backup.Body.Bytes()) || strings.Contains(
		backup.Body.String(), password,
	) {
		t.Fatalf("portable backup: status=%d", backup.Code)
	}
	if decoded, err := usage.ParseUsageBackup(
		backup.Body.Bytes(), password,
	); err != nil || len(decoded) != 1 || decoded[0].RequestID != "req-backup" {
		t.Fatalf("decode portable backup: events=%d err=%v", len(decoded), err)
	}

	missing := request(http.MethodPost, "/usage/import", backup.Body.Bytes(), session.Token, "", "")
	if missing.Code != http.StatusUnauthorized || !strings.Contains(
		missing.Body.String(), "usage_backup_password_required",
	) {
		t.Fatalf("missing import password: %d %s", missing.Code, missing.Body.String())
	}
	wrong := request(
		http.MethodPost, "/usage/import", backup.Body.Bytes(), session.Token, usageBackupKeyHeader,
		"incorrect fixture passphrase",
	)
	if wrong.Code != http.StatusBadRequest || !strings.Contains(wrong.Body.String(), "decryption failed") {
		t.Fatalf("wrong import password: %d %s", wrong.Code, wrong.Body.String())
	}
	exported := request(http.MethodGet, exportEvents, nil, session.Token, usageBackupKeyBase64Header, encoded)
	var events struct {
		Events json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(exported.Body.Bytes(), &events); err != nil || exported.Code != http.StatusOK ||
		!bytes.Contains(events.Events, []byte(`"import_data":"usage-event-v1:`)) {
		t.Fatalf("sealed events: %d %s err=%v", exported.Code, exported.Body.String(), err)
	}
	target := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	target.Start(t.Context())
	t.Cleanup(target.Stop)
	h.usagePersister = target
	for i, body := range [][]byte{backup.Body.Bytes(), events.Events} {
		restore := request(http.MethodPost, "/usage/import", body, session.Token, usageBackupKeyBase64Header, encoded)
		want := `"added":1`
		if i != 0 {
			want = `"skipped":1`
		}
		if restore.Code != http.StatusOK || !strings.Contains(restore.Body.String(), want) {
			t.Fatalf("re-import: %d %s", restore.Code, restore.Body.String())
		}
	}
	if reads := request(http.MethodGet, window, nil, session.Token, "", ""); reads.Code != http.StatusOK ||
		!strings.Contains(reads.Body.String(), `"request_id":"req-backup"`) {
		t.Fatalf("same authenticated session read: %d %s", reads.Code, reads.Body.String())
	}
}

func TestUsageTrimPreviewAndCommittedCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: 1})
	h := &Handler{usagePersister: p}
	preview := func() *httptest.ResponseRecorder {
		return runConfigListRequest(h.TrimPreviewUsageStatistics, http.MethodGet, "/usage-retention/trim-preview", "")
	}
	if unavailable := preview(); unavailable.Code != http.StatusServiceUnavailable || strings.Contains(
		unavailable.Body.String(), "files_count",
	) {
		t.Fatalf("store not ready: %d %s", unavailable.Code, unavailable.Body.String())
	}
	p.Start(t.Context())
	t.Cleanup(p.Stop)
	if empty := preview(); empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"files_count":0`) {
		t.Fatalf("empty store: %d %s", empty.Code, empty.Body.String())
	}
	// Seed both rows with trimming disabled, then enable the requested policy for preview and commit.
	p.SetRetention(config.UsageRetention{Days: -1})
	now := time.Now().UTC()
	data, err := json.Marshal(
		[]usage.FlatDetail{
			{Timestamp: now.Add(-48 * time.Hour), Model: "expired"},
			{Timestamp: now.Add(-time.Hour), Model: "current"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := p.ImportContext(t.Context(), data); err != nil || result.Added != 2 {
		t.Fatalf("seed fixture: %+v err=%v", result, err)
	}
	p.SetRetention(config.UsageRetention{Days: 1})
	if predicted := preview(); predicted.Code != http.StatusOK || !strings.Contains(
		predicted.Body.String(), `"files_count":1`,
	) {
		t.Fatalf("retention preview: %d %s", predicted.Code, predicted.Body.String())
	}
	if count, _, err := p.QueryRequestCountsContext(t.Context()); err != nil || count != 2 {
		t.Fatalf("preview modified records: count=%d err=%v", count, err)
	}
	trim := runConfigListRequest(h.TrimUsageStatistics, http.MethodPost, "/usage-retention/trim", "{}")
	if trim.Code != http.StatusOK || !strings.Contains(trim.Body.String(), `"deleted":1`) {
		t.Fatalf("trim result: %d %s", trim.Code, trim.Body.String())
	}
	if count, _, err := p.QueryRequestCountsContext(t.Context()); err != nil || count != 1 {
		t.Fatalf("records after trim: count=%d err=%v", count, err)
	}
	if effective := runConfigListRequest(
		h.GetUsageRetention, http.MethodGet, "/usage-retention", "",
	); effective.Code != http.StatusOK ||
		!strings.Contains(effective.Body.String(), `"warning_threshold_pct":80`) {
		t.Fatalf("effective threshold: %d %s", effective.Code, effective.Body.String())
	}
}

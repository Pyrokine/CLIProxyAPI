package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGetRequestLogByID_MatchesFullUUIDAndShortID(t *testing.T) {
	dir := t.TempDir()
	h := newLogsTestHandler(dir, true)
	t.Cleanup(func() { _ = h.Close() })
	logPath := filepath.Join(dir, "v1_chat_completions-2026-09-28T100000-1234abcd.log")
	if err := os.WriteFile(logPath, []byte("uuid v7 request log content"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	for _, id := range []string{"1234abcd", "018f3a5b-1234-7abc-def0-00001234abcd"} {
		t.Run(
			id, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Params = gin.Params{{Key: "id", Value: id}}
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/logs/request/"+id, nil)
				h.GetRequestLogByID(ctx)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200, body: %s", recorder.Code, recorder.Body.String())
				}
				if body := recorder.Body.String(); body != "uuid v7 request log content" {
					t.Fatalf("body = %q, want %q", body, "uuid v7 request log content")
				}
			},
		)
	}
}

func TestRequestLogUUIDResolvesOnlyUniqueLegacyShortFile(t *testing.T) {
	const full = "12345678-1234-4abc-8def-123456789abc"
	const compact = "1234567812344abc8def123456789abc"
	for _, test := range []struct {
		name, id string
		files    []string
		status   int
		content  string
	}{
		{"full_uuid", full, []string{"error-old-56789abc.log"}, http.StatusOK, "file-0"},
		{"compact_uuid", compact, []string{"error-old-56789abc.log"}, http.StatusOK, "file-0"},
		{
			"ambiguous_short", full, []string{"error-one-56789abc.log", "error-two-56789abc.log"}, http.StatusConflict,
			"multiple",
		},
		{"uuid_prefix_is_not_a_legacy_match", full, []string{"error-old-12345678.log"}, http.StatusNotFound, "unknown"},
		{
			"new_full_id_wins", compact, []string{"error-old-56789abc.log", "request-new-" + compact + ".log"},
			http.StatusOK, "file-1",
		},
		{
			"malformed_id", "12345678-unrelated-custom-request", []string{"error-old-56789abc.log"},
			http.StatusNotFound, "unknown",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				handler, _, dir := newRequestLogTestHandler(t)
				for i, file := range test.files {
					data := []byte("file-" + string(rune('0'+i)) + "\n")
					if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				recorder := performRequestLogLookup(t, handler, test.id, "lines=10")
				if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.content) {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
			},
		)
	}
}

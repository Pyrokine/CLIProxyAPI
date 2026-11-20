package openai

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

func TestWebsocketTimelineCapturePreservesCompleteEventsAndSafeLimit(t *testing.T) {
	for _, storage := range []string{"memory", "file"} {
		for _, exceedsSafeLimit := range []bool{false, true} {
			name := storage + "/complete-event"
			if exceedsSafeLimit {
				name = storage + "/safe-capture-limit"
			}
			t.Run(
				name, func(t *testing.T) {
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					var source *internallogging.FileBodySource
					if storage == "file" {
						var err error
						source, err = internallogging.NewFileBodySourceInDir(t.TempDir(), "websocket-limit")
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(
							func() {
								if err := source.Cleanup(); err != nil {
									t.Error(err)
								}
							},
						)
						ctx.Set(internallogging.WebsocketTimelineSourceContextKey, source)
					}
					recorder := newWebsocketTimelineRecorder(ctx)
					size := 128 << 10
					if exceedsSafeLimit {
						size = 9 << 20
					}
					payload := bytes.Repeat([]byte("x"), size)
					appendWebsocketEvent(recorder, "response", payload)
					if exceedsSafeLimit {
						appendWebsocketEvent(recorder, "response", bytes.Repeat([]byte("y"), 2<<20))
						appendWebsocketEvent(recorder, "disconnect", []byte("later event"))
					}
					body := []byte(recorder.String())
					if source != nil {
						var err error
						body, err = source.Bytes()
						if err != nil {
							t.Fatal(err)
						}
					}
					if exceedsSafeLimit {
						marker := []byte(`"log_capture":"truncated`)
						if storage == "file" {
							marker = []byte("[payload omitted: exceeds safe redaction limit]")
						}
						if bytes.Count(body, marker) != 1 ||
							bytes.Contains(body, []byte("later event")) || bytes.Contains(body, payload[:1024]) {
							t.Fatalf("safe capture marker or privacy limit differs: bytes=%d", len(body))
						}
						return
					}
					expected := append([]byte("websocket.response\n"), payload...)
					expected = append(expected, '\n')
					if !bytes.Equal(body, expected) {
						t.Fatalf("complete event changed: bytes=%d, want %d", len(body), len(expected))
					}
				},
			)
		}
	}
}

func TestWebsocketTimelineCaptureMarksBudgetAfterDirectoryRecovery(t *testing.T) {
	const marker = "\n[websocket log truncated]\n"
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	source, err := internallogging.NewFileBodySourceInDir(t.TempDir(), "websocket-recovery-limit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			if err := source.Cleanup(); err != nil {
				t.Error(err)
			}
		},
	)
	ctx.Set(internallogging.WebsocketTimelineSourceContextKey, source)
	recorder := newWebsocketTimelineRecorder(ctx)
	appendWebsocketEvent(recorder, "request", bytes.Repeat([]byte("a"), 5<<20))
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths=%v", paths)
	}
	if err := os.RemoveAll(filepath.Dir(paths[0])); err != nil {
		t.Fatal(err)
	}
	retained := bytes.Repeat([]byte("b"), 4<<20)
	appendWebsocketEvent(recorder, "response", retained)
	remaining := (10 << 20) - len(marker) - recorder.size - len("\nwebsocket.response\n") - 1
	boundary := bytes.Repeat([]byte("d"), remaining)
	appendWebsocketEvent(recorder, "response", boundary)
	if recorder.size != (10<<20)-len(marker) {
		t.Fatalf("complete boundary event capture=%d", recorder.size)
	}
	rejected := bytes.Repeat([]byte("c"), 2<<20)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() { appendWebsocketEvent(recorder, "response", rejected) })
	}
	workers.Wait()
	appendWebsocketEvent(recorder, "disconnect", []byte("later event"))
	body, err := source.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(body, []byte(marker)) != 1 || !bytes.HasSuffix(body, []byte(marker)) {
		t.Fatalf("budget marker count=%d, bytes=%d", bytes.Count(body, []byte(marker)), len(body))
	}
	if !bytes.Contains(body, retained) || !bytes.Contains(body, boundary) ||
		bytes.Contains(body, rejected[:1024]) || bytes.Contains(body, []byte("later event")) {
		t.Fatalf("accepted event changed or rejected event was recorded: bytes=%d", len(body))
	}
	if recorder.size != 10<<20 {
		t.Fatalf("logical capture=%d, want exactly 10 MiB including the marker", recorder.size)
	}
}

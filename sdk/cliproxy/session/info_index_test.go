package session

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
)

func TestSessionObjectRetainsNestedDuplicateKeySemantics(t *testing.T) {
	root := gjson.Parse(`{"metadata":{"parent_session_id":"first"},"metadata":{"parent_session_id":"second"}}`)
	indexed := newSessionObject(root)
	const path = "metadata.parent_session_id"
	if actual, expected := indexed.Get(path).Raw, root.Get(path).Raw; actual != expected {
		t.Fatalf("duplicate field %s = %s, want %s", path, actual, expected)
	}
}

func TestDuplicateMetadataKeysPreserveSessionExtraction(t *testing.T) {
	tests := []struct {
		name, payload, sessionID, parentID string
		headers                            http.Header
	}{
		{name: "top-level", payload: `{"metadata":{},"metadata":{"session_id":"child"}}`, sessionID: "session:child"},
		{name: "nested request", payload: `{"request":{"metadata":{},"metadata":{"session_id":"child"}}}`, sessionID: "session:child"},
		{name: "header parent", headers: http.Header{"X-Session-Id": {"child"}},
			payload: `{"metadata":{},"metadata":{"parent_session_id":"parent"}}`, sessionID: "header:child", parentID: "header:parent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, ok := ExtractSessionInfo(test.headers, []byte(test.payload), nil)
			if !ok || info.SessionID != test.sessionID || info.ParentSessionID != test.parentID {
				t.Fatalf("session info = %+v, ok = %v", info, ok)
			}
			if !hasExplicitSession(test.headers, []byte(test.payload)) {
				t.Fatal("duplicate metadata key prevented explicit session detection")
			}
		})
	}
}

func TestExtractSessionInfoWithLargeRequestBody(t *testing.T) {
	payload := []byte(fmt.Sprintf(
		`{"messages":[{"role":"user","content":%q}],"metadata":{"parent_session_id":"parent"},"session_id":"child"}`,
		strings.Repeat("long request body", 4096),
	))
	info, ok := ExtractSessionInfo(nil, payload, nil)
	if !ok || info.SessionID != "session:child" || info.ParentSessionID != "session:parent" {
		t.Fatalf("large request session info = %+v, ok=%v", info, ok)
	}
}

func TestLargePayloadSessionExtractionAvoidsRepeatedScans(t *testing.T) {
	payload := []byte(`{"messages":["` + strings.Repeat("x", 1<<20) + `"]}`)
	baseline := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			if util.ParseGJSONBytesNoCopy(payload).Get("missing_session_id").Exists() {
				b.Fatal("unexpected session ID")
			}
		}
	})
	for _, test := range []struct {
		name   string
		lookup func() bool
	}{
		{name: "explicit", lookup: func() bool { return hasExplicitSession(nil, payload) }},
		{name: "extraction", lookup: func() bool { _, ok := ExtractSessionInfo(nil, payload, nil); return ok }},
	} {
		t.Run(test.name, func(t *testing.T) {
			detection := testing.Benchmark(func(b *testing.B) {
				for range b.N {
					if test.lookup() {
						b.Fatal("unexpected session ID")
					}
				}
			})
			if detection.NsPerOp() > baseline.NsPerOp()*12 {
				t.Fatalf("large payload rescanned: detection %d ns/op, single lookup %d ns/op", detection.NsPerOp(), baseline.NsPerOp())
			}
		})
	}
}

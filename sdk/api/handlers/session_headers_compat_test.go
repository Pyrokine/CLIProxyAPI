package handlers

import (
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
)

func TestExplicitTaskAndAmpHeadersPublishRequestSession(t *testing.T) {
	for _, test := range []struct{ header, want string }{
		{header: "X-Task-ID", want: "task:fixture-session"},
		{header: "X-Task_ID", want: "task:fixture-session"},
		{header: "X-Amp-Thread-Id", want: "amp:fixture-session"},
	} {
		t.Run(
			test.header, func(t *testing.T) {
				payload := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"fixture"}]}`)
				ctx := EnrichContextWithSessionHierarchy(
					t.Context(), http.Header{test.header: {"fixture-session"}}, payload, nil,
				)
				if metadata := logging.GetClientRequestMetadata(ctx); metadata.SessionID != test.want {
					t.Fatalf("published session=%q, want=%q", metadata.SessionID, test.want)
				}
			},
		)
	}
}

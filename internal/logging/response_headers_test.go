package logging

import (
	"context"
	"net/http"
	"testing"
)

func TestResponseHeadersHolderClonesHeaders(t *testing.T) {
	ctx := WithResponseHeadersHolder(context.Background())
	headers := http.Header{"X-Request-Id": []string{"req-1"}}
	SetResponseHeaders(ctx, headers)
	headers.Set("X-Request-Id", "mutated")

	got := GetResponseHeaders(ctx)
	if got.Get("X-Request-Id") != "req-1" {
		t.Fatalf("stored header = %q, want req-1", got.Get("X-Request-Id"))
	}
	got.Set("X-Request-Id", "mutated-again")
	if gotAgain := GetResponseHeaders(ctx).Get("X-Request-Id"); gotAgain != "req-1" {
		t.Fatalf("retrieved header mutated stored value: %q", gotAgain)
	}
}

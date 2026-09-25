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

func TestResponseHeadersHolderIsFreshForEachAttemptContext(t *testing.T) {
	parent := WithResponseHeadersHolder(context.Background())
	SetResponseHeaders(parent, http.Header{"X-Attempt": {"first"}})

	attempt := WithResponseHeadersHolder(parent)
	if inherited := GetResponseHeaders(attempt); len(inherited) != 0 {
		t.Fatalf("fresh attempt inherited response headers: %#v", inherited)
	}
	SetResponseHeaders(attempt, http.Header{"X-Attempt": {"second"}})
	if got := GetResponseHeaders(parent).Get("X-Attempt"); got != "first" {
		t.Fatalf("parent attempt headers = %q, want first", got)
	}
	if got := GetResponseHeaders(attempt).Get("X-Attempt"); got != "second" {
		t.Fatalf("child attempt headers = %q, want second", got)
	}
}

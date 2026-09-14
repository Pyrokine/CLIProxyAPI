package logging

import (
	"context"
	"testing"
)

func TestRequestOutcomeStatePreservesSpecificFailure(t *testing.T) {
	state := NewRequestOutcomeState()
	state.MarkFailure(RequestOutcomeUpstreamError, "usage", "upstream rejected request")
	state.MarkFailure(RequestOutcomeServerError, "http", "status 500")

	got := state.Snapshot()
	if !got.Failed {
		t.Fatal("outcome was not marked failed")
	}
	if got.Kind != RequestOutcomeUpstreamError {
		t.Fatalf("outcome kind = %q, want %q", got.Kind, RequestOutcomeUpstreamError)
	}
	if got.Stage != "usage" || got.Reason != "upstream rejected request" {
		t.Fatalf("outcome = %#v", got)
	}
}

func TestRequestOutcomeStateAllowsMoreSpecificFailure(t *testing.T) {
	state := NewRequestOutcomeState()
	state.MarkFailure(RequestOutcomeServerError, "http", "status 500")
	state.MarkFailure(RequestOutcomeDeadline, "executor", context.DeadlineExceeded.Error())

	got := state.Snapshot()
	if got.Kind != RequestOutcomeDeadline {
		t.Fatalf("outcome kind = %q, want %q", got.Kind, RequestOutcomeDeadline)
	}
	if got.Stage != "executor" || got.Reason != context.DeadlineExceeded.Error() {
		t.Fatalf("outcome = %#v", got)
	}
}

func TestRequestOutcomeStateSurvivesWithoutCancel(t *testing.T) {
	state := NewRequestOutcomeState()
	ctx := WithRequestOutcomeState(context.Background(), state)
	ctx = context.WithoutCancel(ctx)

	if got := RequestOutcomeStateFromContext(ctx); got != state {
		t.Fatalf("context state = %p, want %p", got, state)
	}
}

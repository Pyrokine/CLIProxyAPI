package cliproxy

import (
	"testing"
	"time"
)

func TestNewShutdownContextReturnsFreshContext(t *testing.T) {
	originalTimeout := serviceShutdownTimeout
	serviceShutdownTimeout = 20 * time.Millisecond
	defer func() {
		serviceShutdownTimeout = originalTimeout
	}()

	service := &Service{}
	ctx1, cancel1 := service.newShutdownContext()
	defer cancel1()
	<-ctx1.Done()

	ctx2, cancel2 := service.newShutdownContext()
	defer cancel2()
	if err := ctx2.Err(); err != nil {
		t.Fatalf("expected fresh shutdown context, got %v", err)
	}

	deadline1, ok1 := ctx1.Deadline()
	deadline2, ok2 := ctx2.Deadline()
	if !ok1 || !ok2 {
		t.Fatal("expected both shutdown contexts to have deadlines")
	}
	if !deadline2.After(deadline1) {
		t.Fatalf("expected second deadline after first, got first=%v second=%v", deadline1, deadline2)
	}
}

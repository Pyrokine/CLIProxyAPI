package usage_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestLegacyStreamDeliveryRetainsResultAfterCancellation(t *testing.T) {
	for _, result := range []error{nil, io.ErrClosedPipe} {
		synctest.Test(
			t, func(t *testing.T) {
				parent, cancel := context.WithCancel(t.Context())
				ctx, finish := usage.WithStreamDelivery(parent)
				if !usage.StreamDeliveryTracked(ctx) {
					t.Fatal("consumer delivery registration is missing")
				}
				cancel()
				out := make(chan error, 1)
				go func() {
					err, tracked := usage.WaitStreamDelivery(ctx)
					if !tracked {
						out <- errors.New("consumer delivery registration was lost")
						return
					}
					out <- err
				}()
				synctest.Wait()
				select {
				case err := <-out:
					t.Fatalf("request cancellation resolved consumer delivery: %v", err)
				default:
				}
				finish(result)
				finish(context.Canceled)
				if err := <-out; !errors.Is(err, result) {
					t.Fatalf("delivery result=%v, want %v", err, result)
				}
			},
		)
	}
}

func TestLegacyStreamDeliveryProducerReferences(t *testing.T) {
	ctx, finish := usage.WithStreamDelivery(t.Context())
	defer finish(nil)
	if usage.StreamDeliverySupported(ctx) {
		t.Fatal("consumer registration incorrectly registered producer support")
	}
	first := usage.SupportStreamDelivery(ctx)
	second := usage.SupportStreamDelivery(ctx)
	first()
	first()
	if !usage.StreamDeliverySupported(ctx) {
		t.Fatal("one producer release removed another producer's support")
	}
	second()
	if usage.StreamDeliverySupported(ctx) {
		t.Fatal("last producer release retained support")
	}
	usage.SupportStreamDelivery(context.Background())()
}

func TestLegacyStreamDeliveryUntracked(t *testing.T) {
	ctx := context.Background()
	if usage.StreamDeliveryTracked(ctx) || usage.StreamDeliverySupported(ctx) {
		t.Fatal("untracked context gained delivery ownership")
	}
	if err, tracked := usage.WaitStreamDelivery(ctx); err != nil || tracked {
		t.Fatalf("untracked delivery result=%v tracked=%t", err, tracked)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestLegacyStreamDeliveryNestedScope(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(t.Context(), key{}, "metadata"))
	defer cancel()
	outer, finishOuter := usage.WithStreamDelivery(parent)
	defer finishOuter(nil)
	stopOuter := usage.SupportStreamDelivery(outer)
	defer stopOuter()
	inner := usage.WithoutStreamDelivery(outer)
	if usage.StreamDeliveryTracked(inner) || usage.StreamDeliverySupported(inner) || inner.Value(key{}) != "metadata" {
		t.Fatal("nested execution retained delivery ownership or lost request metadata")
	}
	if err, tracked := usage.WaitStreamDelivery(inner); err != nil || tracked {
		t.Fatalf("untracked delivery result=%v tracked=%t", err, tracked)
	}
	nested, finishNested := usage.WithStreamDelivery(inner)
	finishNested(io.ErrClosedPipe)
	if err, tracked := usage.WaitStreamDelivery(nested); !errors.Is(err, io.ErrClosedPipe) || !tracked {
		t.Fatalf("independent nested delivery result=%v tracked=%t", err, tracked)
	}
	if !usage.StreamDeliverySupported(outer) {
		t.Fatal("nested scope removed the outer producer's support")
	}
	cancel()
	//lint:ignore SA1012 验证 nil context 按原合同返回 nil
	if inner.Err() != context.Canceled || usage.WithoutStreamDelivery(nil) != nil {
		t.Fatal("nested scope changed cancellation or nil context behavior")
	}
}

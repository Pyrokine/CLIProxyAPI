package usage

import (
	"context"
	"sync"
	"sync/atomic"
)

type streamDeliveryContextKey struct{}

type streamDeliveryState struct {
	resolved  chan struct{}
	once      sync.Once
	result    error
	producers atomic.Int64
}

func streamDeliveryFromContext(ctx context.Context) *streamDeliveryState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(streamDeliveryContextKey{}).(*streamDeliveryState)
	return state
}

// WithStreamDelivery 注册由消费方显式完成的整流交付结果，首次完成结果生效
func WithStreamDelivery(ctx context.Context) (context.Context, func(error)) {
	if ctx == nil {
		ctx = context.Background()
	}
	state := &streamDeliveryState{resolved: make(chan struct{})}
	finish := func(result error) {
		state.once.Do(
			func() {
				state.result = result
				close(state.resolved)
			},
		)
	}
	return context.WithValue(ctx, streamDeliveryContextKey{}, state), finish
}

// WithoutStreamDelivery 隔离内部执行的交付归属，保留取消和其他 context 值
func WithoutStreamDelivery(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, streamDeliveryContextKey{}, struct{}{})
}

// StreamDeliveryTracked 返回当前消费方是否会显式确认整流交付
func StreamDeliveryTracked(ctx context.Context) bool {
	return streamDeliveryFromContext(ctx) != nil
}

// WaitStreamDelivery 等待消费方确认，生产者须在等待前关闭输出通道，取消请求保留该等待
func WaitStreamDelivery(ctx context.Context) (error, bool) {
	state := streamDeliveryFromContext(ctx)
	if state == nil {
		return nil, false
	}
	<-state.resolved
	return state.result, true
}

// SupportStreamDelivery 注册一个理解整流确认的生产者，返回可重复调用的释放函数
func SupportStreamDelivery(ctx context.Context) func() {
	state := streamDeliveryFromContext(ctx)
	if state == nil {
		return func() {}
	}
	state.producers.Add(1)
	return sync.OnceFunc(func() { state.producers.Add(-1) })
}

// StreamDeliverySupported 返回当前是否仍有理解整流确认的生产者
func StreamDeliverySupported(ctx context.Context) bool {
	state := streamDeliveryFromContext(ctx)
	return state != nil && state.producers.Load() > 0
}

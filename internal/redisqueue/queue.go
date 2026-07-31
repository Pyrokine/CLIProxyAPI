package redisqueue

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRetentionSeconds int64 = 60
	maxRetentionSeconds     int64 = 3600
	usageSubscriberBuffer         = 256

	queueDisabled uint32 = 0
	queueEnabled  uint32 = 1
	queueClosed   uint32 = 2

	usageSupportRefreshPayload = `{"support_refresh":true}`
	usageRefreshPayload        = `{"refresh":true}`
)

type queueItem struct {
	enqueuedAt time.Time
	payload    []byte
}

// Queue owns one service's usage output buffer, subscribers and runtime flags.
type Queue struct {
	state                  atomic.Uint32
	usageStatisticsEnabled atomic.Bool
	retentionSeconds       atomic.Int64
	mu                     sync.Mutex
	items                  []queueItem
	head                   int
	subscribers            map[uint64]chan []byte
	nextSubscriberID       uint64
}

var defaultQueue = NewQueue()

// DefaultQueue returns the process-wide compatibility queue.
func DefaultQueue() *Queue { return defaultQueue }

func NewQueue() *Queue {
	q := &Queue{}
	q.retentionSeconds.Store(defaultRetentionSeconds)
	q.usageStatisticsEnabled.Store(true)
	return q
}

func (q *Queue) SetEnabled(value bool) {
	if q == nil {
		return
	}
	if value {
		for {
			state := q.state.Load()
			if state == queueClosed {
				return
			}
			if q.state.CompareAndSwap(state, queueEnabled) {
				return
			}
		}
	}
	for {
		state := q.state.Load()
		if state == queueClosed || q.state.CompareAndSwap(state, queueDisabled) {
			q.clear()
			return
		}
	}
}

func (q *Queue) Close() {
	if q == nil {
		return
	}
	q.state.Store(queueClosed)
	q.clear()
}

func (q *Queue) Enabled() bool {
	return q != nil && q.state.Load() == queueEnabled
}

func (q *Queue) SetRetentionSeconds(value int) {
	if q == nil {
		return
	}
	normalized := int64(value)
	if normalized <= 0 {
		normalized = defaultRetentionSeconds
	} else if normalized > maxRetentionSeconds {
		normalized = maxRetentionSeconds
	}
	q.retentionSeconds.Store(normalized)
}

func (q *Queue) SetUsageStatisticsEnabled(value bool) {
	if q == nil {
		return
	}
	q.usageStatisticsEnabled.Store(value)
}

func (q *Queue) UsageStatisticsEnabled() bool {
	return q != nil && q.usageStatisticsEnabled.Load()
}

func (q *Queue) Enqueue(payload []byte) {
	if q == nil || !q.Enabled() || len(payload) == 0 {
		return
	}
	if q.publishToSubscribers(payload) {
		return
	}
	q.enqueue(payload)
}

func (q *Queue) PopOldest(count int) [][]byte {
	if q == nil || !q.Enabled() || count <= 0 {
		return nil
	}
	return q.popOldest(count)
}

func (q *Queue) SubscribeUsage() (<-chan []byte, func()) {
	if q == nil {
		closed := make(chan []byte)
		close(closed)
		return closed, func() {}
	}
	return q.subscribeUsage([]byte(usageSupportRefreshPayload))
}

func (q *Queue) NotifyUsageRefresh() {
	if q == nil {
		return
	}
	q.publishToSubscribers([]byte(usageRefreshPayload))
}

func SetEnabled(value bool) { defaultQueue.SetEnabled(value) }

func Close() { defaultQueue.Close() }

func Enabled() bool { return defaultQueue.Enabled() }

func SetRetentionSeconds(value int) { defaultQueue.SetRetentionSeconds(value) }

func Enqueue(payload []byte) { defaultQueue.Enqueue(payload) }

func PopOldest(count int) [][]byte { return defaultQueue.PopOldest(count) }

func SubscribeUsage() (<-chan []byte, func()) { return defaultQueue.SubscribeUsage() }

func NotifyUsageRefresh() { defaultQueue.NotifyUsageRefresh() }

func (q *Queue) clear() {
	q.mu.Lock()

	subscribers := make([]chan []byte, 0, len(q.subscribers))
	for _, subscriber := range q.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	q.items = nil
	q.head = 0
	q.subscribers = nil
	q.mu.Unlock()

	for _, subscriber := range subscribers {
		close(subscriber)
	}
}

func (q *Queue) enqueue(payload []byte) {
	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.pruneLocked(now)
	q.items = append(
		q.items, queueItem{
			enqueuedAt: now,
			payload:    append([]byte(nil), payload...),
		},
	)
	q.maybeCompactLocked()
}

func (q *Queue) publishToSubscribers(payload []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.subscribers) == 0 {
		return false
	}

	for id, subscriber := range q.subscribers {
		cloned := append([]byte(nil), payload...)
		select {
		case subscriber <- cloned:
		default:
			delete(q.subscribers, id)
			close(subscriber)
		}
	}

	return true
}

func (q *Queue) subscribeUsage(initialPayload []byte) (<-chan []byte, func()) {
	subscriber := make(chan []byte, usageSubscriberBuffer)

	q.mu.Lock()
	if q.state.Load() != queueEnabled {
		q.mu.Unlock()
		close(subscriber)
		return subscriber, func() {}
	}
	if q.subscribers == nil {
		q.subscribers = make(map[uint64]chan []byte)
	}
	q.nextSubscriberID++
	id := q.nextSubscriberID
	q.subscribers[id] = subscriber
	if len(initialPayload) > 0 {
		subscriber <- append([]byte(nil), initialPayload...)
	}
	q.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(
			func() {
				q.unsubscribeUsage(id)
			},
		)
	}
	return subscriber, unsubscribe
}

func (q *Queue) unsubscribeUsage(id uint64) {
	q.mu.Lock()
	subscriber, ok := q.subscribers[id]
	if ok {
		delete(q.subscribers, id)
	}
	q.mu.Unlock()

	if ok {
		close(subscriber)
	}
}

func (q *Queue) popOldest(count int) [][]byte {
	now := time.Now()

	q.mu.Lock()
	defer q.mu.Unlock()

	q.pruneLocked(now)
	available := len(q.items) - q.head
	if available <= 0 {
		q.items = nil
		q.head = 0
		return nil
	}
	if count > available {
		count = available
	}

	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		item := q.items[q.head+i]
		out = append(out, item.payload)
	}
	q.head += count
	q.maybeCompactLocked()
	return out
}

func (q *Queue) pruneLocked(now time.Time) {
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}

	windowSeconds := q.retentionSeconds.Load()
	if windowSeconds <= 0 {
		windowSeconds = defaultRetentionSeconds
	}
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second)
	for q.head < len(q.items) && q.items[q.head].enqueuedAt.Before(cutoff) {
		q.head++
	}
}

func (q *Queue) maybeCompactLocked() {
	if q.head == 0 {
		return
	}
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}
	if q.head < 1024 && q.head*2 < len(q.items) {
		return
	}
	q.items = append([]queueItem(nil), q.items[q.head:]...)
	q.head = 0
}

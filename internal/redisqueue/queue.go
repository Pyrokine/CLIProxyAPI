package redisqueue

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRetentionSeconds int64 = 60
	maxRetentionSeconds     int64 = 3600
	usageSubscriberBuffer         = 8
	maxUsageSubscribers           = 8

	MaxPopCount       = 1000
	maxPopBytes       = 8 << 20
	maxQueueItems     = 10_000
	maxQueueBytes     = 32 << 20
	maxQueueItemBytes = 1 << 20

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
	itemsBytes             int64
	pruneTimer             *time.Timer
	pruneGeneration        uint64
	dropped                atomic.Uint64
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
	q.mu.Lock()
	q.pruneLocked(time.Now())
	q.schedulePruneLocked(time.Now())
	q.mu.Unlock()
}

func (q *Queue) SetUsageStatisticsEnabled(value bool) {
	if q == nil {
		return
	}
	q.usageStatisticsEnabled.Store(value)
}

func (q *Queue) usageStatisticsOn() bool {
	return q != nil && q.usageStatisticsEnabled.Load()
}

func (q *Queue) Enqueue(payload []byte) {
	if q == nil || !q.Enabled() || len(payload) == 0 {
		return
	}
	if len(payload) > maxQueueItemBytes {
		q.dropped.Add(1)
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
	if count > MaxPopCount {
		count = MaxPopCount
	}
	return q.popOldest(count)
}

func (q *Queue) DroppedCount() uint64 {
	if q == nil {
		return 0
	}
	return q.dropped.Load()
}

func (q *Queue) SubscribeUsage() (<-chan []byte, func()) {
	subscriber, unsubscribe, _ := q.TrySubscribeUsage()
	return subscriber, unsubscribe
}

func (q *Queue) TrySubscribeUsage() (<-chan []byte, func(), bool) {
	if q == nil {
		closed := make(chan []byte)
		close(closed)
		return closed, func() {}, false
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

func Enabled() bool { return defaultQueue.Enabled() }

func SetRetentionSeconds(value int) { defaultQueue.SetRetentionSeconds(value) }

func Enqueue(payload []byte) { defaultQueue.Enqueue(payload) }

func PopOldest(count int) [][]byte { return defaultQueue.PopOldest(count) }

func subscribeUsage() (<-chan []byte, func()) { return defaultQueue.SubscribeUsage() }

func NotifyUsageRefresh() { defaultQueue.NotifyUsageRefresh() }

func (q *Queue) clear() {
	q.mu.Lock()

	subscribers := make([]chan []byte, 0, len(q.subscribers))
	for _, subscriber := range q.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	q.items = nil
	q.head = 0
	q.itemsBytes = 0
	if q.pruneTimer != nil {
		q.pruneTimer.Stop()
		q.pruneTimer = nil
	}
	q.pruneGeneration++
	q.subscribers = nil
	q.mu.Unlock()

	for _, subscriber := range subscribers {
		close(subscriber)
	}
}

func (q *Queue) enqueue(payload []byte) {
	now := time.Now()
	cloned := append([]byte(nil), payload...)

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.state.Load() != queueEnabled {
		return
	}

	q.pruneLocked(now)
	for len(q.items)-q.head >= maxQueueItems || q.itemsBytes+int64(len(cloned)) > maxQueueBytes {
		if !q.dropOldestLocked() {
			q.dropped.Add(1)
			return
		}
		q.dropped.Add(1)
	}
	q.items = append(q.items, queueItem{enqueuedAt: now, payload: cloned})
	q.itemsBytes += int64(len(cloned))
	q.maybeCompactLocked()
	q.schedulePruneLocked(now)
}

func (q *Queue) publishToSubscribers(payload []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.state.Load() != queueEnabled {
		return true
	}

	if len(q.subscribers) == 0 {
		return false
	}

	delivered := false
	var dropped uint64
	for id, subscriber := range q.subscribers {
		cloned := append([]byte(nil), payload...)
		select {
		case subscriber <- cloned:
			delivered = true
		default:
			delete(q.subscribers, id)
			close(subscriber)
			dropped++
		}
	}
	if dropped > 0 {
		q.dropped.Add(dropped)
	}
	return delivered
}

func (q *Queue) subscribeUsage(initialPayload []byte) (<-chan []byte, func(), bool) {
	subscriber := make(chan []byte, usageSubscriberBuffer)

	q.mu.Lock()
	if q.state.Load() != queueEnabled || len(q.subscribers) >= maxUsageSubscribers {
		q.mu.Unlock()
		close(subscriber)
		return subscriber, func() {}, false
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
	return subscriber, unsubscribe, true
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
		q.itemsBytes = 0
		return nil
	}
	if count > available {
		count = available
	}

	out := make([][]byte, 0, count)
	var responseBytes int
	for len(out) < count && q.head < len(q.items) {
		item := q.items[q.head]
		if len(out) > 0 && responseBytes+len(item.payload) > maxPopBytes {
			break
		}
		out = append(out, item.payload)
		responseBytes += len(item.payload)
		q.itemsBytes -= int64(len(item.payload))
		q.head++
	}
	q.maybeCompactLocked()
	q.schedulePruneLocked(now)
	return out
}

func (q *Queue) pruneLocked(now time.Time) {
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		q.itemsBytes = 0
		return
	}

	windowSeconds := q.retentionSeconds.Load()
	if windowSeconds <= 0 {
		windowSeconds = defaultRetentionSeconds
	}
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second)
	for q.head < len(q.items) && !q.items[q.head].enqueuedAt.After(cutoff) {
		q.itemsBytes -= int64(len(q.items[q.head].payload))
		q.head++
	}
	q.maybeCompactLocked()
}

func (q *Queue) dropOldestLocked() bool {
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		q.itemsBytes = 0
		return false
	}
	q.itemsBytes -= int64(len(q.items[q.head].payload))
	q.head++
	q.maybeCompactLocked()
	return true
}

func (q *Queue) schedulePruneLocked(now time.Time) {
	if q.pruneTimer != nil {
		q.pruneTimer.Stop()
		q.pruneTimer = nil
	}
	q.pruneGeneration++
	if q.head >= len(q.items) {
		return
	}
	windowSeconds := q.retentionSeconds.Load()
	if windowSeconds <= 0 {
		windowSeconds = defaultRetentionSeconds
	}
	delay := max(q.items[q.head].enqueuedAt.Add(time.Duration(windowSeconds)*time.Second).Sub(now), 0)
	generation := q.pruneGeneration
	q.pruneTimer = time.AfterFunc(
		delay, func() {
			q.pruneExpired(generation)
		},
	)
}

func (q *Queue) pruneExpired(generation uint64) {
	if q == nil {
		return
	}
	now := time.Now()
	q.mu.Lock()
	if q.pruneGeneration != generation {
		q.mu.Unlock()
		return
	}
	q.pruneTimer = nil
	q.pruneLocked(now)
	q.schedulePruneLocked(now)
	q.mu.Unlock()
}

func (q *Queue) maybeCompactLocked() {
	if q.head == 0 {
		return
	}
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		q.itemsBytes = 0
		return
	}
	if q.head < 1024 && q.head*2 < len(q.items) {
		return
	}
	q.items = append([]queueItem(nil), q.items[q.head:]...)
	q.head = 0
}

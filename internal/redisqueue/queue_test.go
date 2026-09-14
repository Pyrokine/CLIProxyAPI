package redisqueue

import (
	"testing"
	"time"
)

func TestQueueInstancesAreIsolated(t *testing.T) {
	queueA := NewQueue()
	queueB := NewQueue()
	queueA.SetEnabled(true)
	queueB.SetEnabled(true)
	queueA.SetRetentionSeconds(1)
	queueB.SetRetentionSeconds(120)
	queueB.SetUsageStatisticsEnabled(false)
	if !queueA.usageStatisticsOn() || queueB.usageStatisticsOn() {
		t.Fatal("usage statistics toggle leaked between queue instances")
	}
	if queueA.retentionSeconds.Load() != 1 || queueB.retentionSeconds.Load() != 120 {
		t.Fatal("retention setting leaked between queue instances")
	}

	subscriberA, unsubscribeA := queueA.SubscribeUsage()
	defer unsubscribeA()
	subscriberB, unsubscribeB := queueB.SubscribeUsage()
	defer unsubscribeB()
	requireUsageSubscriberPayload(t, subscriberA, usageSupportRefreshPayload)
	requireUsageSubscriberPayload(t, subscriberB, usageSupportRefreshPayload)

	queueA.Enqueue([]byte("service-a"))
	requireUsageSubscriberPayload(t, subscriberA, "service-a")
	select {
	case payload := <-subscriberB:
		t.Fatalf("service B subscriber received service A payload %q", payload)
	case <-time.After(50 * time.Millisecond):
	}

	queueB.SetEnabled(false)
	select {
	case _, ok := <-subscriberB:
		if ok {
			t.Fatal("service B subscriber remained open after disabling service B")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for service B subscriber close")
	}
	queueA.Enqueue([]byte("service-a-after-b-shutdown"))
	requireUsageSubscriberPayload(t, subscriberA, "service-a-after-b-shutdown")
}

func TestQueueCloseIsIrreversible(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	subscriber, unsubscribe := queue.SubscribeUsage()
	defer unsubscribe()
	requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)

	queue.Close()
	queue.SetEnabled(true)
	if queue.Enabled() {
		t.Fatal("closed queue was re-enabled")
	}
	select {
	case _, ok := <-subscriber:
		if ok {
			t.Fatal("subscriber remained open after queue close")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber close")
	}
	queue.Enqueue([]byte("after-close"))
	if items := queue.PopOldest(1); len(items) != 0 {
		t.Fatalf("closed queue returned items: %q", items)
	}
}

func TestEnqueueBroadcastsToUsageSubscribersAndSkipsQueue(t *testing.T) {
	withEnabledQueue(
		t, func() {
			first, unsubscribeFirst := subscribeUsage()
			defer unsubscribeFirst()
			second, unsubscribeSecond := subscribeUsage()
			defer unsubscribeSecond()
			requireUsageSubscriberPayload(t, first, usageSupportRefreshPayload)
			requireUsageSubscriberPayload(t, second, usageSupportRefreshPayload)

			Enqueue([]byte("usage-record"))

			requireUsageSubscriberPayload(t, first, "usage-record")
			requireUsageSubscriberPayload(t, second, "usage-record")

			if items := PopOldest(1); len(items) != 0 {
				t.Fatalf("PopOldest() items = %q, want empty after subscriber broadcast", items)
			}

			unsubscribeFirst()
			unsubscribeSecond()

			Enqueue([]byte("queued-record"))
			items := PopOldest(1)
			if len(items) != 1 || string(items[0]) != "queued-record" {
				t.Fatalf("PopOldest() items = %q, want queued record after unsubscribe", items)
			}
		},
	)
}

func TestSetEnabledFalseClosesUsageSubscribers(t *testing.T) {
	withEnabledQueue(
		t, func() {
			subscriber, unsubscribe := subscribeUsage()
			defer unsubscribe()
			requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)

			SetEnabled(false)

			select {
			case _, ok := <-subscriber:
				if ok {
					t.Fatalf("subscriber channel remained open after SetEnabled(false)")
				}
			case <-time.After(time.Second):
				t.Fatalf("timeout waiting for subscriber close")
			}
		},
	)
}

func TestNotifyUsageRefreshBroadcastsToUsageSubscribers(t *testing.T) {
	withEnabledQueue(
		t, func() {
			subscriber, unsubscribe := subscribeUsage()
			defer unsubscribe()
			requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)

			NotifyUsageRefresh()

			requireUsageSubscriberPayload(t, subscriber, usageRefreshPayload)
		},
	)
}

func requireUsageSubscriberPayload(t *testing.T, subscriber <-chan []byte, want string) {
	t.Helper()

	select {
	case got, ok := <-subscriber:
		if !ok {
			t.Fatalf("subscriber closed before receiving %q", want)
		}
		if string(got) != want {
			t.Fatalf("subscriber payload = %q, want %q", string(got), want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for subscriber payload %q", want)
	}
}

func TestQueueRejectsOversizedItem(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	queue.Enqueue(make([]byte, maxQueueItemBytes+1))

	if dropped := queue.DroppedCount(); dropped != 1 {
		t.Fatalf("DroppedCount() = %d, want 1", dropped)
	}
	if items := queue.PopOldest(1); len(items) != 0 {
		t.Fatalf("oversized item was queued: %d items", len(items))
	}
}

func TestQueueEvictsOldestAtItemCapacity(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	for index := 0; index <= maxQueueItems; index++ {
		queue.Enqueue([]byte{byte(index % 251)})
	}

	queue.mu.Lock()
	available := len(queue.items) - queue.head
	itemsBytes := queue.itemsBytes
	queue.mu.Unlock()
	if available != maxQueueItems || itemsBytes != maxQueueItems {
		t.Fatalf("queue size = %d items/%d bytes, want %d/%d", available, itemsBytes, maxQueueItems, maxQueueItems)
	}
	if dropped := queue.DroppedCount(); dropped != 1 {
		t.Fatalf("DroppedCount() = %d, want one eviction", dropped)
	}
	items := queue.PopOldest(1)
	if len(items) != 1 || len(items[0]) != 1 || items[0][0] != 1 {
		t.Fatalf("oldest retained item = %v, want payload 1", items)
	}
}

func TestQueueBoundsTotalBytesAndPopResponse(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	for index := range maxQueueBytes/maxQueueItemBytes + 1 {
		payload := make([]byte, maxQueueItemBytes)
		payload[0] = byte(index)
		queue.Enqueue(payload)
	}

	queue.mu.Lock()
	available := len(queue.items) - queue.head
	itemsBytes := queue.itemsBytes
	queue.mu.Unlock()
	wantItems := maxQueueBytes / maxQueueItemBytes
	if available != wantItems || itemsBytes != maxQueueBytes {
		t.Fatalf("queue size = %d items/%d bytes, want %d/%d", available, itemsBytes, wantItems, maxQueueBytes)
	}
	if dropped := queue.DroppedCount(); dropped != 1 {
		t.Fatalf("DroppedCount() = %d, want one byte-cap eviction", dropped)
	}

	items := queue.PopOldest(MaxPopCount)
	if len(items) != maxPopBytes/maxQueueItemBytes {
		t.Fatalf("PopOldest() returned %d items, want %d", len(items), maxPopBytes/maxQueueItemBytes)
	}
	if items[0][0] != 1 {
		t.Fatalf("first retained payload marker = %d, want 1", items[0][0])
	}
}

func TestQueueSlowSubscriberOverflowIsCountedAndQueued(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	subscriber, unsubscribe, subscribed := queue.TrySubscribeUsage()
	defer unsubscribe()
	if !subscribed {
		t.Fatal("first usage subscriber was rejected")
	}
	requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
	for index := range usageSubscriberBuffer {
		queue.Enqueue([]byte{byte(index)})
	}
	queue.Enqueue([]byte("overflow"))

	if dropped := queue.DroppedCount(); dropped != 1 {
		t.Fatalf("DroppedCount() = %d, want 1 failed subscriber delivery", dropped)
	}
	items := queue.PopOldest(1)
	if len(items) != 1 || string(items[0]) != "overflow" {
		t.Fatalf("queued overflow item = %q, want overflow", items)
	}
}

func TestQueueLimitsUsageSubscribers(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	unsubscribes := make([]func(), 0, maxUsageSubscribers)
	for index := range maxUsageSubscribers {
		subscriber, unsubscribe, subscribed := queue.TrySubscribeUsage()
		if !subscribed {
			t.Fatalf("subscriber %d was rejected", index)
		}
		requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
		unsubscribes = append(unsubscribes, unsubscribe)
	}
	defer func() {
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
	}()

	extra, unsubscribeExtra, subscribed := queue.TrySubscribeUsage()
	defer unsubscribeExtra()
	if subscribed {
		t.Fatal("subscriber above the configured limit was accepted")
	}
	if _, ok := <-extra; ok {
		t.Fatal("rejected subscriber channel remained open")
	}
}

func TestQueueStalePruneCallbackKeepsCurrentTimer(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	queue.SetRetentionSeconds(int(maxRetentionSeconds))
	queue.Enqueue([]byte("retained"))
	defer queue.Close()

	queue.mu.Lock()
	staleTimer := queue.pruneTimer
	staleGeneration := queue.pruneGeneration
	queue.schedulePruneLocked(time.Now())
	currentTimer := queue.pruneTimer
	queue.mu.Unlock()

	if staleTimer == nil || currentTimer == nil || staleTimer == currentTimer {
		t.Fatalf("prune timers = stale %p current %p, want distinct non-nil timers", staleTimer, currentTimer)
	}

	queue.pruneExpired(staleGeneration)

	queue.mu.Lock()
	ownedTimer := queue.pruneTimer
	available := len(queue.items) - queue.head
	queue.mu.Unlock()
	if ownedTimer != currentTimer {
		t.Fatalf("stale callback changed timer ownership: got %p, want %p", ownedTimer, currentTimer)
	}
	if available != 1 {
		t.Fatalf("stale callback pruned %d retained items, want 1 available", 1-available)
	}
}

func TestQueueRetentionTimerPrunesWithoutAnotherOperation(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	queue.SetRetentionSeconds(1)
	queue.Enqueue([]byte("expires"))

	deadline := time.Now().Add(3 * time.Second)
	for {
		queue.mu.Lock()
		available := len(queue.items) - queue.head
		itemsBytes := queue.itemsBytes
		timer := queue.pruneTimer
		queue.mu.Unlock()
		if available == 0 && itemsBytes == 0 && timer == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention timer left %d items/%d bytes", available, itemsBytes)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

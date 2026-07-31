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
	if !queueA.UsageStatisticsEnabled() || queueB.UsageStatisticsEnabled() {
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
			first, unsubscribeFirst := SubscribeUsage()
			defer unsubscribeFirst()
			second, unsubscribeSecond := SubscribeUsage()
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
			subscriber, unsubscribe := SubscribeUsage()
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
			subscriber, unsubscribe := SubscribeUsage()
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

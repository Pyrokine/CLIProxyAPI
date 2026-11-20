package redisqueue

import (
	"bytes"
	"sync"
	"testing"
)

func TestEnqueueErrorBroadcastsToErrorSubscribersAndDiscardsWithoutSubscribers(t *testing.T) {
	withEnabledQueue(
		t, func() {
			subscriber, unsubscribe := subscribeErrors()
			defer unsubscribe()
			enqueueError([]byte("error-record"))
			requireUsageSubscriberPayload(t, subscriber, "error-record")
			unsubscribe()
			enqueueError([]byte("discarded-error"))
			if items := PopOldest(1); len(items) != 0 {
				t.Fatalf("error events entered the usage backlog: %q", items)
			}
			next, unsubscribeNext := subscribeErrors()
			defer unsubscribeNext()
			select {
			case payload := <-next:
				t.Fatalf("error event was retained without subscribers: %q", payload)
			default:
			}
		},
	)
}

func TestQueueErrorEventsIsolateTopicsAndInstances(t *testing.T) {
	first, second := NewQueue(), NewQueue()
	first.SetEnabled(true)
	second.SetEnabled(true)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	usage, unsubscribeUsage := first.SubscribeUsage()
	defer unsubscribeUsage()
	requireUsageSubscriberPayload(t, usage, usageSupportRefreshPayload)
	errorsOne, unsubscribeOne := first.SubscribeErrors()
	defer unsubscribeOne()
	errorsTwo, unsubscribeTwo := first.SubscribeErrors()
	defer unsubscribeTwo()
	otherErrors, unsubscribeOther := second.SubscribeErrors()
	defer unsubscribeOther()
	payload := []byte("error-record")
	first.EnqueueError(payload)
	payload[0] = 'x'
	requireUsageSubscriberPayload(t, errorsOne, "error-record")
	requireUsageSubscriberPayload(t, errorsTwo, "error-record")
	first.NotifyUsageRefresh()
	requireUsageSubscriberPayload(t, usage, usageRefreshPayload)
	for name, subscriber := range map[string]<-chan []byte{
		"first errors": errorsOne, "second errors": errorsTwo, "other instance": otherErrors,
	} {
		select {
		case got := <-subscriber:
			t.Fatalf("%s received an unrelated event: %q", name, got)
		default:
		}
	}
}

func TestQueueLimitsAndCountsErrorSubscribers(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	t.Cleanup(queue.Close)
	var first <-chan []byte
	for index := range maxUsageSubscribers {
		subscriber, unsubscribe, subscribed := queue.TrySubscribeErrors()
		if !subscribed {
			t.Fatalf("error subscriber %d was rejected", index)
		}
		t.Cleanup(unsubscribe)
		if index == 0 {
			first = subscriber
		}
	}
	extra, unsubscribeExtra, subscribed := queue.TrySubscribeErrors()
	defer unsubscribeExtra()
	if subscribed {
		t.Fatal("error subscriber above the limit was accepted")
	}
	if _, ok := <-extra; ok {
		t.Fatal("rejected error subscriber remained open")
	}
	queue.EnqueueError(bytes.Repeat([]byte("x"), maxQueueItemBytes+1))
	if dropped := queue.DroppedCount(); dropped != 1 {
		t.Fatalf("oversized error event drop count = %d, want 1", dropped)
	}
	for range usageSubscriberBuffer {
		queue.EnqueueError([]byte("buffered"))
	}
	queue.EnqueueError([]byte("overflow"))
	if dropped := queue.DroppedCount(); dropped != 1+maxUsageSubscribers {
		t.Fatalf("error event drop count = %d, want %d", dropped, 1+maxUsageSubscribers)
	}
	count := 0
	for range first {
		count++
	}
	if count != usageSubscriberBuffer || len(queue.PopOldest(1)) != 0 {
		t.Fatalf("error subscriber buffered %d events or retained a usage backlog", count)
	}
}

func TestQueueStopClosesErrorAndUsageSubscribers(t *testing.T) {
	for _, stop := range []struct {
		name string
		stop func(*Queue)
	}{
		{name: "disable", stop: func(queue *Queue) { queue.SetEnabled(false) }},
		{name: "close", stop: (*Queue).Close},
	} {
		t.Run(
			stop.name, func(t *testing.T) {
				queue := NewQueue()
				queue.SetEnabled(true)
				t.Cleanup(queue.Close)
				usage, unsubscribeUsage := queue.SubscribeUsage()
				defer unsubscribeUsage()
				requireUsageSubscriberPayload(t, usage, usageSupportRefreshPayload)
				events, unsubscribeErrors := queue.SubscribeErrors()
				defer unsubscribeErrors()
				stop.stop(queue)
				for _, subscriber := range []<-chan []byte{usage, events} {
					select {
					case _, ok := <-subscriber:
						if ok {
							t.Fatal("stopped queue delivered a new event")
						}
					default:
						t.Fatal("stopped queue retained a subscriber")
					}
				}
			},
		)
	}
}

func TestQueueErrorSubscribePublishAndCloseAreConcurrent(t *testing.T) {
	queue := NewQueue()
	queue.SetEnabled(true)
	var workers, started sync.WaitGroup
	started.Add(4)
	for range 4 {
		workers.Go(
			func() {
				_, unsubscribe, _ := queue.TrySubscribeErrors()
				queue.EnqueueError([]byte("initial error"))
				unsubscribe()
				started.Done()
				for range 100 {
					_, unsubscribe, _ := queue.TrySubscribeErrors()
					queue.EnqueueError([]byte("concurrent error"))
					unsubscribe()
				}
			},
		)
	}
	started.Wait()
	queue.Close()
	workers.Wait()
	queue.SetEnabled(true)
	if queue.Enabled() {
		t.Fatal("closed queue was re-enabled")
	}
}

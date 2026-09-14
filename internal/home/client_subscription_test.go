package home

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type homeSubscriptionResult struct {
	event any
	err   error
}

type fakeHomeSubscription struct {
	results   []homeSubscriptionResult
	pingError error
	pingCalls int
}

func (s *fakeHomeSubscription) ReceiveTimeout(context.Context, time.Duration) (any, error) {
	if len(s.results) == 0 {
		return nil, errors.New("no subscription result")
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result.event, result.err
}

func (s *fakeHomeSubscription) Ping(context.Context, ...string) error {
	s.pingCalls++
	return s.pingError
}

type homeSubscriptionTimeoutError struct{}

func (homeSubscriptionTimeoutError) Error() string   { return "i/o timeout" }
func (homeSubscriptionTimeoutError) Timeout() bool   { return true }
func (homeSubscriptionTimeoutError) Temporary() bool { return true }

func TestReceiveHomeSubscriptionEventKeepsSilentConnectionHealthy(t *testing.T) {
	subscription := &fakeHomeSubscription{
		results: []homeSubscriptionResult{
			{err: homeSubscriptionTimeoutError{}},
			{event: &redis.Pong{}},
		},
	}

	event, err := receiveHomeSubscriptionEvent(context.Background(), subscription)
	if err != nil {
		t.Fatalf("receiveHomeSubscriptionEvent() error = %v", err)
	}
	if _, ok := event.(*redis.Pong); !ok {
		t.Fatalf("event = %T, want *redis.Pong", event)
	}
	if subscription.pingCalls != 1 {
		t.Fatalf("ping calls = %d, want 1", subscription.pingCalls)
	}
}

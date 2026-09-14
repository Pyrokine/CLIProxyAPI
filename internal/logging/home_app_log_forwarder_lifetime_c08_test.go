package logging

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type blockingHomeAppLogClient struct {
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (*blockingHomeAppLogClient) HeartbeatOK() bool { return true }

func (c *blockingHomeAppLogClient) RPushAppLog(context.Context, []byte) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.err
}

func TestHomeAppLogForwarderOldOwnerCannotDisableReplacementRace(t *testing.T) {
	oldClient := &blockingHomeAppLogClient{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("ERR unsupported key"),
	}
	newClient := &stubHomeAppLogClient{heartbeatOK: true}
	forwarder := &HomeAppLogForwarder{
		client: oldClient,
		queue:  make(chan homeAppLogPayload, 1),
		stop:   make(chan struct{}),
	}
	forwarder.enabled.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		forwarder.forward(homeAppLogPayload{Line: "old lifetime"})
	}()
	<-oldClient.entered
	forwarder.bind(newClient)
	close(oldClient.release)
	<-done

	if !forwarder.enabled.Load() {
		t.Fatal("old owner disabled replacement forwarder")
	}
	if got := forwarder.homeClient(); got != newClient {
		t.Fatalf("forwarder client = %p, want replacement %p", got, newClient)
	}
}

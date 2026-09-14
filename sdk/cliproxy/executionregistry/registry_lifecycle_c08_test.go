package executionregistry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistryDrainAndCancellationTable(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "drain cancels owned scope and publishes release",
			run: func(t *testing.T) {
				registry := New()
				pending, errBegin := registry.BeginDispatch()
				if errBegin != nil {
					t.Fatalf("BeginDispatch() error = %v", errBegin)
				}
				scope, errInstall := registry.Install(
					pending, ScopeSpec{
						CredentialID: "credential",
						Model:        "model",
						Accounted:    true,
					},
				)
				if errInstall != nil {
					t.Fatalf("Install() error = %v", errInstall)
				}
				cancelled := make(chan struct{})
				if errBind := scope.Bind(
					func() error {
						close(cancelled)
						return nil
					},
				); errBind != nil {
					t.Fatalf("Bind() error = %v", errBind)
				}
				released := make(chan struct{})
				registry.SetReleaseSink(
					func(group ReleaseGroup, sequence int64) *ReleaseTicket {
						if group != (ReleaseGroup{CredentialID: "credential", Model: "model"}) || sequence != 1 {
							t.Errorf("release = %#v/%d", group, sequence)
						}
						close(released)
						return NewReleaseTicket(group, sequence, released)
					},
				)
				go func() {
					<-cancelled
					scope.End("drain")
				}()
				drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
				defer cancelDrain()
				if errDrain := registry.Drain(drainCtx); errDrain != nil {
					t.Fatalf("Drain() error = %v", errDrain)
				}
				select {
				case <-released:
				default:
					t.Fatal("accounted scope release was not published")
				}
			},
		},
		{
			name: "pending dispatch makes drain cancellation explicit",
			run: func(t *testing.T) {
				registry := New()
				pending, errBegin := registry.BeginDispatch()
				if errBegin != nil {
					t.Fatalf("BeginDispatch() error = %v", errBegin)
				}
				drainCtx, cancelDrain := context.WithTimeout(context.Background(), 20*time.Millisecond)
				errDrain := registry.Drain(drainCtx)
				cancelDrain()
				if !errors.Is(errDrain, context.DeadlineExceeded) {
					t.Fatalf("Drain() error = %v, want deadline exceeded", errDrain)
				}
				pending.End()
				finishCtx, cancelFinish := context.WithTimeout(context.Background(), time.Second)
				defer cancelFinish()
				if errFinish := registry.Drain(finishCtx); errFinish != nil {
					t.Fatalf("second Drain() error = %v", errFinish)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, test.run)
	}
}

func TestRegistryBeginDrainCancellationRace(t *testing.T) {
	registry := New()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			<-start
			for range 32 {
				pending, errBegin := registry.BeginDispatch()
				if errors.Is(errBegin, ErrRegistryNotAccepting) {
					return
				}
				if errBegin != nil {
					t.Errorf("BeginDispatch() error = %v", errBegin)
					return
				}
				pending.End()
			}
		})
	}
	close(start)
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if errDrain := registry.Drain(drainCtx); errDrain != nil {
		t.Fatalf("Drain() error = %v", errDrain)
	}
	workers.Wait()
	if _, errBegin := registry.BeginDispatch(); !errors.Is(errBegin, ErrRegistryNotAccepting) {
		t.Fatalf("BeginDispatch() after drain error = %v, want %v", errBegin, ErrRegistryNotAccepting)
	}
}

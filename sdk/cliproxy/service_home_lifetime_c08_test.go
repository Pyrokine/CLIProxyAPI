package cliproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
)

func TestUnsafeHomeLifetimeReplacementTable(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*home.Client)
		err     error
		want    bool
	}{
		{name: "stable transport", err: errors.New("connection reset")},
		{name: "membership takeover unavailable", err: errors.New("ERR membership_takeover_unavailable"), want: true},
		{
			name: "legacy membership fallback",
			err:  errors.New("ERR wrong number of arguments for 'subscribe' command"), want: true,
		},
		{name: "ambiguous dispatch", prepare: (*home.Client).AbortAmbiguousDispatch, want: true},
		{name: "persisted legacy membership", prepare: (*home.Client).EnableLegacyMembership, want: true},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				client := home.New(internalconfig.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 6379})
				if test.prepare != nil {
					test.prepare(client)
				}
				if got := unsafeHomeLifetimeReplacement(client, test.err); got != test.want {
					t.Fatalf("unsafeHomeLifetimeReplacement() = %v, want %v", got, test.want)
				}
			},
		)
	}
}

func TestDetachHomeLifetimePreservesConcurrentReplacementRace(t *testing.T) {
	service := &Service{}
	oldClient := home.New(internalconfig.HomeConfig{Enabled: true, Host: "old", Port: 6379})
	newClient := home.New(internalconfig.HomeConfig{Enabled: true, Host: "new", Port: 6380})
	oldRegistry := executionregistry.New()
	newRegistry := executionregistry.New()
	service.homeClient = newClient
	service.homeRegistry = newRegistry
	home.SetCurrent(newClient)
	t.Cleanup(home.ClearCurrent)

	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			service.detachHomeLifetime(oldClient, oldRegistry)
			if current := home.Current(); current != newClient {
				t.Errorf("Home current client = %p, want replacement %p", current, newClient)
			}
		})
	}
	workers.Wait()
	if current := service.currentHomeClient(); current != newClient {
		t.Fatalf("service current client = %p, want replacement %p", current, newClient)
	}
	if service.homeRegistry != newRegistry {
		t.Fatal("replacement registry was detached")
	}
}

func TestDrainDetachedHomeLifetimeCancelsOwnedExecution(t *testing.T) {
	service := &Service{homeDrainBound: time.Second}
	client := home.New(internalconfig.HomeConfig{Enabled: true, Host: "127.0.0.1", Port: 6379})
	registry := executionregistry.New()
	pending, errBegin := registry.BeginDispatch()
	if errBegin != nil {
		t.Fatalf("BeginDispatch() error = %v", errBegin)
	}
	scope, errInstall := registry.Install(
		pending, executionregistry.ScopeSpec{CredentialID: "credential", Model: "model"},
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
	go func() {
		<-cancelled
		scope.End("service replacement")
	}()

	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	service.homeClient = client
	service.homeRegistry = registry
	service.homeCancel = cancelLifetime
	home.SetCurrent(client)
	t.Cleanup(home.ClearCurrent)
	if !service.drainDetachedHomeLifetime(context.Background()) {
		t.Fatal("drainDetachedHomeLifetime() = false")
	}
	select {
	case <-lifetimeCtx.Done():
	default:
		t.Fatal("detached lifetime was not cancelled")
	}
	if service.currentHomeClient() != nil {
		t.Fatal("detached client remains published")
	}
	if home.Current() != nil {
		t.Fatal("detached global Home client remains published")
	}
	if _, errBegin = registry.BeginDispatch(); !errors.Is(errBegin, executionregistry.ErrRegistryNotAccepting) {
		t.Fatalf("BeginDispatch() after drain error = %v", errBegin)
	}
}

func TestStopHomeSubscriberWaitsForCancellation(t *testing.T) {
	service := &Service{}
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	supervisor := &homeSubscriberSupervisor{cancel: cancelLifetime, done: make(chan struct{})}
	service.homeSupervisor = supervisor
	go func() {
		<-lifetimeCtx.Done()
		close(supervisor.done)
	}()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if errStop := service.stopHomeSubscriber(stopCtx); errStop != nil {
		t.Fatalf("stopHomeSubscriber() error = %v", errStop)
	}
	if service.homeSupervisor != nil {
		t.Fatal("home supervisor remains published after stop")
	}
}

func TestStopHomeSubscriberReturnsDrainFailure(t *testing.T) {
	service := &Service{}
	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	supervisor := &homeSubscriberSupervisor{cancel: cancelLifetime, done: make(chan struct{})}
	supervisor.fail(context.DeadlineExceeded)
	service.homeSupervisor = supervisor
	go func() {
		<-lifetimeCtx.Done()
		close(supervisor.done)
	}()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if errStop := service.stopHomeSubscriber(stopCtx); !errors.Is(errStop, context.DeadlineExceeded) {
		t.Fatalf("stopHomeSubscriber() error = %v, want deadline exceeded", errStop)
	}
}

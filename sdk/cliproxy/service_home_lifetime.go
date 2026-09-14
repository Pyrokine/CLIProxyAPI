package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

const homeSubscriberRetryBackoff = time.Second

type homeSubscriberSupervisor struct {
	cancel context.CancelFunc
	done   chan struct{}

	errMu sync.Mutex
	err   error
}

func (s *homeSubscriberSupervisor) fail(err error) {
	if s == nil || err == nil {
		return
	}
	s.errMu.Lock()
	s.err = errors.Join(s.err, err)
	s.errMu.Unlock()
}

func (s *homeSubscriberSupervisor) result() error {
	if s == nil {
		return nil
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Service) startHomeSubscriber(ctx context.Context) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || !cfg.Home.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.homeLifecycleMu.Lock()
	defer s.homeLifecycleMu.Unlock()
	if previous := s.homeSupervisor; previous != nil {
		previous.cancel()
		<-previous.done
		s.homeSupervisor = nil
		if errPrevious := previous.result(); errPrevious != nil {
			log.WithError(errPrevious).Error("previous Home subscriber lifetime did not stop safely")
			return
		}
	}
	if !s.drainDetachedHomeLifetime(ctx) || ctx.Err() != nil {
		return
	}

	homeCtx, cancel := context.WithCancel(ctx)
	s.homeClientMu.Lock()
	s.homeGeneration++
	generation := s.homeGeneration
	s.homeCancel = cancel
	s.homeClientMu.Unlock()
	supervisor := &homeSubscriberSupervisor{cancel: cancel, done: make(chan struct{})}
	s.homeSupervisor = supervisor
	go s.runHomeSubscriber(homeCtx, ctx, cfg.Home, generation, supervisor)
}

func (s *Service) stopHomeSubscriber(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.homeLifecycleMu.Lock()
	defer s.homeLifecycleMu.Unlock()
	supervisor := s.homeSupervisor
	if supervisor != nil {
		supervisor.cancel()
		select {
		case <-supervisor.done:
			s.homeSupervisor = nil
			if errSupervisor := supervisor.result(); errSupervisor != nil {
				return errSupervisor
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !s.drainDetachedHomeLifetime(ctx) {
		return errors.New("failed to drain detached Home lifetime")
	}
	return nil
}

func (s *Service) runHomeSubscriber(
	homeCtx, parentCtx context.Context,
	homeCfg internalconfig.HomeConfig,
	generation uint64,
	supervisor *homeSubscriberSupervisor,
) {
	defer func() {
		s.homeClientMu.Lock()
		if s.homeGeneration == generation {
			s.homeCancel = nil
		}
		s.homeClientMu.Unlock()
		close(supervisor.done)
	}()

	var previousClient *home.Client
	registry := executionregistry.New()
	releaseFlusher := home.NewReleaseFlusher(nil, nil)
	registry.SetReleaseSink(releaseFlusher.MarkDirty)
	cancelBound := atomic.Int64{}
	cancelBound.Store(int64(internalconfig.CredentialConcurrencyConfig{}.WithDefaults().CPACancelBound))

	for homeCtx.Err() == nil {
		client := previousClient
		if client == nil {
			client = home.New(homeCfg)
		} else {
			client = client.NewLifetime()
		}
		client.SetManagedLifetime(true)

		lifetimeCtx, lifetimeCancel := context.WithCancel(homeCtx)
		releaseCtx, releaseCancel := context.WithCancel(context.WithoutCancel(homeCtx))
		releaseFlusher.SetConfigProvider(client.LimiterConfig)
		releaseFlusher.SetSender(client.PushConcurrencyRelease)
		releaseDone := make(chan struct{})
		go func() {
			defer close(releaseDone)
			releaseFlusher.Run(releaseCtx)
		}()

		var published atomic.Bool
		var background sync.WaitGroup
		errRun := client.RunConfigSubscriberLifetime(
			lifetimeCtx, func(raw []byte) error {
				parsed, errParse := config.ParseConfigBytes(raw)
				if errParse != nil {
					return errParse
				}
				if errLifecycle := client.SetLifecycleConfig(parsed.CredentialConcurrency); errLifecycle != nil {
					return errLifecycle
				}
				publisherConfig, errPublisher := coreauth.HomeInFlightPublisherConfigFromConfig(parsed.CredentialInFlight)
				if errPublisher != nil {
					return errPublisher
				}
				if s.coreManager != nil {
					s.coreManager.ApplyHomeInFlightPublisherConfig(publisherConfig)
				}
				registry.ObserveBarrier(parsed.CredentialConcurrency.ObservationBarrierRevision)
				cancelBound.Store(int64(parsed.CredentialConcurrency.WithDefaults().CPACancelBound))
				return s.applyHomeOverlayWithClient(lifetimeCtx, parsed, client)
			}, func() {
				if !s.publishHomeLifetime(
					homeCtx, lifetimeCtx, generation, client, registry, time.Duration(cancelBound.Load()),
				) {
					return
				}
				published.Store(true)
				if s.coreManager != nil {
					background.Go(func() {
						s.coreManager.StartHomeInFlightPublisher(lifetimeCtx, client, registry)
					})
				}
				background.Go(func() {
					s.runHomeUsageForwarder(lifetimeCtx, client)
				})
			},
		)

		lifetimeCancel()
		background.Wait()
		s.detachHomeLifetime(client, registry)

		retry := errRun != nil && homeCtx.Err() == nil
		if retry {
			releaseCancel()
			<-releaseDone
			client.Close()

			settleBound := durationOrDefault(time.Duration(cancelBound.Load()))
			settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(parentCtx), settleBound)
			errPending := registry.WaitPending(settleCtx)
			cancelSettle()
			if errPending != nil {
				supervisor.fail(
					fmt.Errorf(
						"settle pending Home dispatches before subscriber replacement: %w", errPending,
					),
				)
				log.WithError(errPending).Error("failed to settle pending Home dispatches before subscriber replacement")
				return
			}

			legacyProtocol := home.IsLegacyMembershipProtocolError(errRun)
			if legacyProtocol {
				client.EnableLegacyMembership()
			}
			if unsafeHomeLifetimeReplacement(client, errRun) {
				registry.SetReleaseSink(nil)
				drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(parentCtx), settleBound)
				errDrain := registry.Drain(drainCtx)
				cancelDrain()
				if errDrain != nil && !errors.Is(errDrain, executionregistry.ErrRegistryClosed) {
					supervisor.fail(
						fmt.Errorf(
							"drain Home executions after unsafe subscriber replacement: %w", errDrain,
						),
					)
					log.WithError(errDrain).Error("failed to drain Home executions after unsafe subscriber replacement")
					return
				}
				client.SuppressTakeover()
				registry = executionregistry.New()
				releaseFlusher = home.NewReleaseFlusher(nil, nil)
				registry.SetReleaseSink(releaseFlusher.MarkDirty)
			}
			log.WithError(errRun).Warn("Home config subscription lifetime ended")
			if !published.Load() && !waitForHomeSubscriberRetry(homeCtx, homeSubscriberRetryBackoff) {
				return
			}
			previousClient = client
			continue
		}

		drainBound := durationOrDefault(time.Duration(cancelBound.Load()))
		drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(parentCtx), drainBound)
		errDrain := registry.Drain(drainCtx)
		var errFlush error
		if errDrain == nil || errors.Is(errDrain, executionregistry.ErrRegistryClosed) {
			errFlush = releaseFlusher.Flush(drainCtx)
		}
		cancelDrain()
		releaseCancel()
		<-releaseDone
		client.Close()
		if errDrain != nil && !errors.Is(errDrain, executionregistry.ErrRegistryClosed) {
			supervisor.fail(fmt.Errorf("drain Home execution registry: %w", errDrain))
			if parentCtx.Err() == nil {
				log.WithError(errDrain).Error("failed to drain Home execution registry")
			}
			return
		}
		if errFlush != nil {
			supervisor.fail(fmt.Errorf("flush Home concurrency releases: %w", errFlush))
			if parentCtx.Err() == nil {
				log.WithError(errFlush).Error("failed to flush Home concurrency releases")
			}
			return
		}
		return
	}
}

func unsafeHomeLifetimeReplacement(client *home.Client, err error) bool {
	return client != nil && (client.AmbiguousDispatch() || client.LegacyMembership() || home.IsMembershipTakeoverUnavailableError(err) || home.IsLegacyMembershipProtocolError(err))
}

func durationOrDefault(value time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return internalconfig.CredentialConcurrencyConfig{}.WithDefaults().CPACancelBound
}

func (s *Service) publishHomeLifetime(
	homeCtx, lifetimeCtx context.Context,
	generation uint64,
	client *home.Client,
	registry *executionregistry.Registry,
	drainBound time.Duration,
) bool {
	if s == nil || client == nil || registry == nil || homeCtx.Err() != nil || lifetimeCtx.Err() != nil {
		return false
	}
	s.homeClientMu.Lock()
	defer s.homeClientMu.Unlock()
	if s.homeGeneration != generation || homeCtx.Err() != nil || lifetimeCtx.Err() != nil {
		return false
	}
	s.homeClient = client
	s.homeRegistry = registry
	s.homeDrainBound = durationOrDefault(drainBound)
	if s.coreManager != nil {
		s.homeDispatchBundle = s.coreManager.PublishHomeDispatch(client, registry, generation)
	}
	home.SetCurrent(client)
	if s.homeLogForwarder == nil {
		s.homeLogForwarder = logging.StartHomeAppLogForwarder(0)
	}
	s.homeLogForwarder.Bind(client)
	s.homeLogForwarderFor = client
	return true
}

func (s *Service) detachHomeLifetime(client *home.Client, registry *executionregistry.Registry) {
	if s == nil {
		return
	}
	s.homeClientMu.Lock()
	var bundle *coreauth.HomeDispatchBundle
	if s.homeClient == client && s.homeRegistry == registry {
		bundle = s.homeDispatchBundle
		s.homeClient = nil
		s.homeRegistry = nil
		s.homeDispatchBundle = nil
		s.homeDrainBound = 0
	}
	forwarder := s.homeLogForwarder
	if s.homeLogForwarderFor == client {
		s.homeLogForwarderFor = nil
	} else {
		forwarder = nil
	}
	s.homeClientMu.Unlock()
	if s.coreManager != nil {
		s.coreManager.ClearHomeDispatchBundle(bundle)
	}
	home.ClearCurrentIf(client)
	if forwarder != nil {
		forwarder.Deactivate(client)
	}
}

func (s *Service) drainDetachedHomeLifetime(parentCtx context.Context) bool {
	if s == nil {
		return false
	}
	s.homeClientMu.Lock()
	previousCancel := s.homeCancel
	previousClient := s.homeClient
	previousRegistry := s.homeRegistry
	previousBundle := s.homeDispatchBundle
	previousDrainBound := s.homeDrainBound
	previousForwarder := s.homeLogForwarder
	previousForwarderClient := s.homeLogForwarderFor
	s.homeCancel = nil
	s.homeClient = nil
	s.homeRegistry = nil
	s.homeDispatchBundle = nil
	s.homeDrainBound = 0
	s.homeLogForwarderFor = nil
	s.homeClientMu.Unlock()

	if s.coreManager != nil {
		s.coreManager.ClearHomeDispatchBundle(previousBundle)
	}
	home.ClearCurrentIf(previousClient)
	if previousCancel != nil {
		previousCancel()
	}
	if previousForwarder != nil && previousForwarderClient == previousClient {
		previousForwarder.Deactivate(previousClient)
	}
	if previousRegistry != nil {
		drainCtx, cancelDrain := context.WithTimeout(
			context.WithoutCancel(parentCtx), durationOrDefault(previousDrainBound),
		)
		errDrain := previousRegistry.Drain(drainCtx)
		cancelDrain()
		if errDrain != nil && !errors.Is(errDrain, executionregistry.ErrRegistryClosed) {
			if previousClient != nil {
				previousClient.Close()
			}
			log.WithError(errDrain).Error("failed to drain replaced Home execution registry")
			return false
		}
	}
	if previousClient != nil {
		previousClient.Close()
	}
	return true
}

func (s *Service) runHomeUsageForwarder(ctx context.Context, client *home.Client) {
	if s == nil || client == nil || s.usageQueue == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !client.HeartbeatOK() {
			if !waitForHomeSubscriberRetry(ctx, time.Second) {
				return
			}
			continue
		}
		items := s.usageQueue.PopOldest(64)
		if len(items) == 0 {
			if !waitForHomeSubscriberRetry(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}
		for index := range items {
			if errPush := client.LPushUsage(ctx, items[index]); errPush != nil {
				for retryIndex := index; retryIndex < len(items); retryIndex++ {
					s.usageQueue.Enqueue(items[retryIndex])
				}
				if !waitForHomeSubscriberRetry(ctx, time.Second) {
					return
				}
				break
			}
		}
	}
}

func waitForHomeSubscriberRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

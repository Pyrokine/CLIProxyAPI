package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type homeExecutionExecutor struct {
	schedulerTestExecutor
	id      string
	prepare func(context.Context, *Auth) (*Auth, error)
	execute func(context.Context, *Auth, executor.Request, executor.Options) error
}

func (e *homeExecutionExecutor) Identifier() string { return e.id }

func (e *homeExecutionExecutor) ShouldPrepareRequestAuth(*Auth) bool { return e.prepare != nil }

func (e *homeExecutionExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	return e.prepare(ctx, auth)
}

func (e *homeExecutionExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	req executor.Request,
	opts executor.Options,
) (executor.Response, error) {
	if err := e.execute(ctx, auth, req, opts); err != nil {
		return executor.Response{}, err
	}
	return executor.Response{Payload: []byte("ok")}, nil
}

func (e *homeExecutionExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	req executor.Request,
	opts executor.Options,
) (executor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *homeExecutionExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	req executor.Request,
	opts executor.Options,
) (*executor.StreamResult, error) {
	response, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan executor.StreamChunk, 1)
	chunks <- executor.StreamChunk{Payload: response.Payload}
	close(chunks)
	return &executor.StreamResult{Chunks: chunks}, nil
}

type homeExecutionStreamExecutor struct {
	schedulerTestExecutor
	chunks <-chan executor.StreamChunk
}

func (e *homeExecutionStreamExecutor) ExecuteStream(
	context.Context,
	*Auth,
	executor.Request,
	executor.Options,
) (*executor.StreamResult, error) {
	return &executor.StreamResult{Chunks: e.chunks}, nil
}

func TestHomeOriginalClosedErrorStream(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "legacy", true: "acknowledged"}[acknowledged], func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if acknowledged {
					ctx = executor.WithStreamDeliveryAcknowledgement(ctx)
				}
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				registry := executionregistry.New()
				manager.PublishHomeDispatch(
					&accountedHomeExecutionDispatcher{
						auths: []Auth{
							{
								ID: "home-auth", Provider: "test", Status: StatusActive,
							},
						},
					}, registry, 1,
				)
				failure := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream failed"}
				source := make(chan executor.StreamChunk, 2)
				source <- executor.StreamChunk{Payload: []byte("initial")}
				source <- executor.StreamChunk{Err: failure}
				close(source)
				manager.RegisterExecutor(&homeExecutionStreamExecutor{chunks: source})
				result, err := manager.ExecuteStream(
					ctx, []string{"test"}, executor.Request{Model: "model-a"}, executor.Options{Stream: true},
				)
				if err != nil {
					t.Fatal(err)
				}
				chunks := 0
				for chunk := range result.Chunks {
					if chunks == 0 && (string(chunk.Payload) != "initial" || chunk.Err != nil) ||
						chunks == 1 && !errors.Is(chunk.Err, failure) || chunks > 1 {
						t.Fatalf("closed error stream chunk %d=%+v", chunks, chunk)
					}
					chunks++
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
				}
				if active := registry.FreezeInFlight(time.Now()).Executions; chunks != 2 || len(active) != 0 {
					t.Fatalf("closed error stream chunks=%d retained executions=%+v", chunks, active)
				}
			},
		)
	}
}

func TestHomeOriginalStreamSourceLifetime(t *testing.T) {
	for _, ending := range []string{
		"source " +
			"close", "error then " +
			"close", "error then " +
			"cancel", "error then " +
			"bound", "consumer " +
			"cancel", "missing " +
			"source",
	} {
		for _, acknowledged := range []bool{false, true} {
			name := ending + "/legacy"
			if acknowledged {
				name = ending + "/acknowledged"
			}
			t.Run(
				name, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							ctx, cancel := context.WithCancel(t.Context())
							defer cancel()
							if acknowledged {
								ctx = executor.WithStreamDeliveryAcknowledgement(ctx)
							}
							registry := executionregistry.New()
							releases := make(chan executionregistry.ReleaseGroup, 4)
							registry.SetReleaseSink(
								func(
									group executionregistry.ReleaseGroup,
									_ int64,
								) {
									releases <- group
								},
							)
							manager := NewManager(nil, nil, nil)
							manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
							manager.PublishHomeDispatch(
								&accountedHomeExecutionDispatcher{
									auths: []Auth{
										{
											ID: "cred-1", Provider: "test", Status: StatusActive,
										},
									},
								}, registry, 1,
							)
							source := make(chan executor.StreamChunk, 1)
							closeSource := sync.OnceFunc(func() { close(source) })
							defer closeSource()
							producer := &homeExecutionStreamExecutor{chunks: source}
							if ending == "missing source" {
								producer.chunks = nil
							} else {
								source <- executor.StreamChunk{Payload: []byte("initial")}
							}
							manager.RegisterExecutor(producer)
							result, err := manager.ExecuteStream(
								ctx, []string{"test"}, executor.Request{Model: "model-a"},
								executor.Options{Stream: true},
							)
							if ending == "missing source" {
								if err == nil {
									t.Fatal("missing Home stream source was accepted")
								}
							} else {
								if err != nil {
									t.Fatal(err)
								}
								if ending != "consumer cancel" {
									first, open := <-result.Chunks
									if !open || string(first.Payload) != "initial" {
										t.Fatalf("initial chunk=%+v, open=%t", first, open)
									}
									if first.Acknowledge != nil {
										first.Acknowledge(nil)
									}
								}
								if ending ==
									"error then close" ||
									ending ==
										"error then cancel" ||
									ending ==
										"error then bound" {
									upstreamErr := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream failed"}
									source <- executor.StreamChunk{Err: upstreamErr}
									failed, open := <-result.Chunks
									if !open || !errors.Is(failed.Err, upstreamErr) {
										t.Fatalf("error chunk=%+v, open=%t", failed, open)
									}
									if failed.Acknowledge != nil {
										failed.Acknowledge(nil)
									}
								}
								synctest.Wait()
								if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 1 {
									t.Fatalf(
										"Home released before its source ended or the consumer canceled: %+v", active,
									)
								}
								select {
								case group := <-releases:
									t.Fatalf("released while the source was open: %+v", group)
								default:
								}
								if ending ==
									"error then close" ||
									ending ==
										"error then cancel" ||
									ending ==
										"error then bound" {
									source <- executor.StreamChunk{Payload: []byte("after-error-1")}
									source <- executor.StreamChunk{Payload: []byte("after-error-2")}
									synctest.Wait()
									if len(source) != 0 {
										t.Fatal("Home stream did not drain its source after the error chunk")
									}
								}
								if ending == "error then cancel" || ending == "consumer cancel" {
									cancel()
								} else if ending == "error then bound" {
									time.Sleep(time.Second)
								} else {
									closeSource()
								}
								for chunk := range result.Chunks {
									if chunk.Err != nil || len(chunk.Payload) != 0 && ending != "consumer cancel" {
										t.Fatalf("stream forwarded content after its completion boundary: %+v", chunk)
									}
									if chunk.Acknowledge != nil {
										chunk.Acknowledge(nil)
									}
								}
							}
							synctest.Wait()
							if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 {
								t.Fatalf("completed Home stream retained executions: %+v", active)
							}
							select {
							case group := <-releases:
								if group != (executionregistry.ReleaseGroup{CredentialID: "cred-1", Model: "model-a"}) {
									t.Fatalf("release group=%+v", group)
								}
							default:
								t.Fatal("Home stream did not release its accounted selection")
							}
							select {
							case group := <-releases:
								t.Fatalf("duplicate Home release=%+v", group)
							default:
							}
						},
					)
				},
			)
		}
	}
}

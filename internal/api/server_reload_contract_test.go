package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

// noinspection GoDirectComparisonOfErrors
func TestCommittedConfigPublicationSurvivesCancellationAndReportsFailure(t *testing.T) {
	for _, sample := range []struct {
		name string
		err  error
	}{
		{name: "published"},
		{name: "runtime-canceled", err: context.Canceled},
		{name: "runtime-error", err: errors.New("synthetic runtime publication failure")},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				t.Setenv("MANAGEMENT_PASSWORD", "synthetic-management-password")
				requestContext, cancel := context.WithCancel(t.Context())
				defer cancel()
				var server *Server
				calls := 0
				server = newTestServerWithOptions(
					t, WithManagementConfigCommitted(
						func(ctx context.Context, commit ManagementConfigCommit) error {
							calls++
							cancel()
							if ctx.Err() != nil || commit.Generation == 0 {
								t.Error("persisted commit lost publication ownership after request cancellation")
							}
							if sample.err != nil {
								return sample.err
							}
							server.UpdateClientsForConfigCommit(ctx, commit.Config, commit.Generation)
							return nil
						},
					),
				)
				t.Cleanup(server.quotaScheduler.Stop)
				t.Cleanup(server.imageArtifacts.CloseCleaner)
				request := httptest.NewRequest(
					http.MethodPut, "/v0/management/debug", strings.NewReader(`{"value":false}`),
				).WithContext(requestContext)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer synthetic-management-password")
				request.RemoteAddr = "127.0.0.1:12345"
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				persisted, err := config.LoadConfig(server.configFilePath)
				if err != nil || persisted.Debug || calls != 1 || requestContext.Err() != context.Canceled {
					t.Fatalf(
						"committed config=%+v error=%v callbacks=%d cancellation=%v", persisted, err, calls,
						requestContext.Err(),
					)
				}
				if sample.err == nil {
					if response.Code != http.StatusOK || server.configSnapshot().Debug {
						t.Fatalf("successful publication status=%d body=%s", response.Code, response.Body.String())
					}
				} else if response.Code != http.StatusServiceUnavailable ||
					gjson.GetBytes(response.Body.Bytes(), "error").String() != "runtime_sync_failed" ||
					gjson.GetBytes(response.Body.Bytes(), "generation").Uint() == 0 || !server.configSnapshot().Debug {
					t.Fatalf(
						"failed publication reported success or changed runtime: status=%d body=%s", response.Code,
						response.Body.String(),
					)
				}
			},
		)
	}
}

func TestConfigPublicationWaitCancellationDoesNotEndPublication(t *testing.T) {
	server := &Server{}
	if !server.beginConfigPublication() {
		t.Fatal("initial publication rejected")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	waitErr := server.WaitForConfigPublications(ctx)
	active := server.configPublicationCount
	server.endConfigPublication()
	if !errors.Is(waitErr, context.Canceled) || active != 1 {
		t.Fatalf("wait error=%v active publications=%d", waitErr, active)
	}
	if err := server.WaitForConfigPublications(ctx); err != nil {
		t.Fatalf("completed publication remains pending: %v", err)
	}
	server.BeginShutdown()
	if server.beginConfigPublication() {
		server.endConfigPublication()
		t.Fatal("stopped server accepted a publication")
	}
}

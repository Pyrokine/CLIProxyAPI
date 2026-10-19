package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestHomeExecutionDoesNotUseProviderExcludedByRequest(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		t.Run(mode, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			dispatcher := &roundsHomeDispatcher{}
			manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
			executor := &roundExecutor{}
			manager.RegisterExecutor(executor)
			req := coreexecutor.Request{Model: "test-model"}
			opts := coreexecutor.Options{Metadata: map[string]any{}}
			var err error
			switch mode {
			case "count":
				_, err = manager.ExecuteCount(context.Background(), []string{"claude"}, req, opts)
			case "stream":
				_, err = manager.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
			default:
				_, err = manager.Execute(context.Background(), []string{"claude"}, req, opts)
			}
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.HTTPStatus != http.StatusForbidden {
				t.Fatalf("Home %s error = %v, want forbidden provider", mode, err)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("Home %s executed excluded auths: %v", mode, executor.calls)
			}
		})
	}
}

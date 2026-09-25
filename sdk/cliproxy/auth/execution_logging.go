package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func warnLogUpstreamFailure(ctx context.Context, auth *Auth, provider, model string, started time.Time, err error, phase cliproxyexecutor.DispatchPhase) {
	if err == nil || errors.Is(err, context.Canceled) || isRequestInvalidError(err) || !log.IsLevelEnabled(log.WarnLevel) {
		return
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	diagnostic := err.Error()
	if body, direct := clienterror.ResponseBodyFromError(err); direct {
		diagnostic = string(body)
	}
	var marked interface{ LogDiagnostic() string }
	if errors.As(err, &marked) {
		if value := strings.TrimSpace(marked.LogDiagnostic()); value != "" {
			diagnostic = value
		}
	}
	fields := log.Fields{
		"provider": logging.SafeDiagnosticForLog(provider),
		"model":    logging.SafeDiagnosticForLog(model),
		"phase":    executionDispositionForError(err, phase).DispatchPhase,
		"duration": time.Since(started).Round(time.Millisecond),
		"error":    logging.SafeDiagnosticForLog(diagnostic),
	}
	if status := clienterror.HTTPStatusFromError(err); status != 0 {
		fields["status"] = status
	}
	if auth != nil {
		fields["auth_index"] = logging.SafeDiagnosticForLog(auth.Index)
	}
	logEntryWithRequestID(ctx).WithFields(fields).Warn("upstream execution failed")
}

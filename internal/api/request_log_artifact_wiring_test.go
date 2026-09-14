package api

import (
	"testing"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
)

type requestLogSinkCapture struct {
	c02RequestLogger
	sink logging.RequestLogArtifactSink
}

func (l *requestLogSinkCapture) SetRequestLogArtifactSink(sink logging.RequestLogArtifactSink) {
	l.sink = sink
}

func TestServerSetUsagePersisterWiresRequestLogArtifactSink(t *testing.T) {
	logger := &requestLogSinkCapture{}
	server := newTestServerWithOptions(
		t,
		WithRequestLoggerFactory(
			func(*proxyconfig.Config, string) logging.RequestLogger {
				return logger
			},
		),
	)
	persister := new(usage.Persister)

	server.SetUsagePersister(persister)
	if logger.sink != persister {
		t.Fatalf("request log artifact sink = %#v, want usage persister", logger.sink)
	}

	server.SetUsagePersister(nil)
	if logger.sink != nil {
		t.Fatalf("request log artifact sink = %#v after clear, want nil", logger.sink)
	}
}

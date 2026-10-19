package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

type shutdownTestListener struct {
	closed chan struct{}
	once   sync.Once
}

func newShutdownTestListener() *shutdownTestListener {
	return &shutdownTestListener{closed: make(chan struct{})}
}

func (l *shutdownTestListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *shutdownTestListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *shutdownTestListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

func TestServerGinClientIPIgnoresUntrustedForwarding(t *testing.T) {
	server := newTestServer(t)
	server.engine.GET("/test-client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})
	request := httptest.NewRequest(http.MethodGet, "/test-client-ip", nil)
	request.RemoteAddr = "198.51.100.20:43123"
	request.Header.Set("X-Forwarded-For", "203.0.113.5")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "198.51.100.20" {
		t.Fatalf("client IP status = %d, body = %q, want direct peer", recorder.Code, recorder.Body.String())
	}
}

func TestServerStartAfterShutdownRejected(t *testing.T) {
	server := newTestServer(t)
	server.server.Addr = "127.0.0.1:0"
	server.BeginShutdown()

	if err := server.StartWithReady(nil); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("StartWithReady() error = %v, want http.ErrServerClosed", err)
	}
	if server.muxBaseListener != nil || server.muxHTTPListener != nil || server.redirectSrv != nil {
		t.Fatal("StartWithReady() published listeners after shutdown")
	}
}

func TestServerStopClosesPublishedListeners(t *testing.T) {
	server := &Server{}
	base := newShutdownTestListener()
	httpListener := newMuxListener(base.Addr(), 1)
	redirect := &http.Server{}
	if !server.publishListeners(base, httpListener, redirect) {
		t.Fatal("listener publication was rejected before shutdown")
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-base.closed:
	default:
		t.Fatal("base listener remained open after Stop()")
	}
	select {
	case <-httpListener.closeCh:
	default:
		t.Fatal("HTTP listener remained open after Stop()")
	}
	probe := newShutdownTestListener()
	if err := redirect.Serve(probe); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("redirect Serve() error = %v, want http.ErrServerClosed", err)
	}
}

func TestServerListenerPublicationConcurrentWithStop(t *testing.T) {
	for range 64 {
		server := &Server{}
		base := newShutdownTestListener()
		httpListener := newMuxListener(base.Addr(), 1)
		redirect := &http.Server{}
		start := make(chan struct{})
		published := make(chan bool, 1)
		go func() {
			<-start
			if server.publishListeners(base, httpListener, redirect) {
				published <- true
				return
			}
			_ = httpListener.Close()
			_ = base.Close()
			published <- false
		}()

		close(start)
		if err := server.Stop(context.Background()); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		if <-published {
			probe := newShutdownTestListener()
			if err := redirect.Serve(probe); !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("redirect Serve() error = %v, want http.ErrServerClosed", err)
			}
			select {
			case <-probe.closed:
			default:
				t.Fatal("redirect Serve() did not close the listener")
			}
		}
		select {
		case <-base.closed:
		default:
			t.Fatal("base listener remained open after Stop()")
		}
	}
}

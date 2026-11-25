package api

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcceptMuxNotBlockedByIdleConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	var routed atomic.Int32
	handler := http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				t.Error("plain HTTP connection reported TLS state")
			}
			routed.Add(1)
			w.WriteHeader(http.StatusOK)
		},
	)
	srv := httptest.NewUnstartedServer(handler)
	defer srv.Close()
	if err = srv.Listener.Close(); err != nil {
		t.Fatalf("close initial test listener: %v", err)
	}
	muxListener := newMuxListener(listener.Addr(), 1024)
	srv.Listener = muxListener
	srv.Start()

	server := &Server{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.acceptMuxConnections(listener, muxListener)
	}()
	defer func() {
		_ = listener.Close()
		select {
		case errAccept := <-errCh:
			if !errors.Is(errAccept, net.ErrClosed) {
				t.Errorf("accept loop exit error = %v, want net.ErrClosed", errAccept)
			}
		case <-time.After(3 * time.Second):
			t.Error("accept loop did not stop after listener close")
		}
	}()

	idleConn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial idle connection: %v", err)
	}
	defer func() { _ = idleConn.Close() }()
	time.Sleep(50 * time.Millisecond)

	client := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	//goland:noinspection HttpUrlsUsage
	response, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("HTTP request failed while the idle connection remained open: %v", err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("response status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if routed.Load() == 0 {
		t.Error("expected at least one request to be routed")
	}
}

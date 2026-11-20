package api

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestMuxListenerCloseClosesQueuedConnections(t *testing.T) {
	listener := newMuxListener(&net.TCPAddr{}, 1)
	queued, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	if errPut := listener.put(queued); errPut != nil {
		t.Fatalf("queue connection: %v", errPut)
	}

	if errClose := listener.Close(); errClose != nil {
		t.Fatalf("close listener: %v", errClose)
	}
	if errDeadline := peer.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		if !errors.Is(errDeadline, io.ErrClosedPipe) {
			t.Fatalf("set peer read deadline: %v", errDeadline)
		}
	} else {
		var buffer [1]byte
		if _, errRead := peer.Read(buffer[:]); errRead == nil {
			t.Fatal("queued connection remained open after listener close")
		}
	}
	if _, errAccept := listener.Accept(); !errors.Is(errAccept, net.ErrClosed) {
		t.Fatalf("accept after close error = %v, want net.ErrClosed", errAccept)
	}
}

func TestMuxListenerPutAfterClose(t *testing.T) {
	listener := newMuxListener(&net.TCPAddr{}, 1)
	if errClose := listener.Close(); errClose != nil {
		t.Fatalf("close listener: %v", errClose)
	}
	conn, peer := net.Pipe()
	defer func() { _ = conn.Close() }()
	defer func() { _ = peer.Close() }()
	if errPut := listener.put(conn); !errors.Is(errPut, net.ErrClosed) {
		t.Fatalf("put after close error = %v, want net.ErrClosed", errPut)
	}
}

func TestMuxListener_PutAfterClose(t *testing.T) {
	listener := newMuxListener(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}, 16)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	for i := range 500 {
		conn, peer := net.Pipe()
		err := listener.put(conn)
		_ = conn.Close()
		_ = peer.Close()
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("put after Close at iteration %d error = %v, want %v", i, err, net.ErrClosed)
		}
	}
}

func TestMuxListener_ConcurrentBlockedPutDuringClose(t *testing.T) {
	const bufferSize = 2
	const numGoroutines = 16
	listener := newMuxListener(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}, bufferSize)
	defer func() { _ = listener.Close() }()
	for range bufferSize {
		conn, peer := net.Pipe()
		// 缓冲连接需保留至并发阻塞检查结束
		// noinspection GoDeferInLoop
		defer func() {
			_ = conn.Close()
			_ = peer.Close()
		}()
		if err := listener.put(conn); err != nil {
			t.Fatalf("initial put failed: %v", err)
		}
	}

	var wg sync.WaitGroup
	putErrors := make([]error, numGoroutines)
	started := make(chan struct{})
	var readyCount sync.WaitGroup
	readyCount.Add(numGoroutines)
	for i := range numGoroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, peer := net.Pipe()
			defer func() {
				_ = conn.Close()
				_ = peer.Close()
			}()
			readyCount.Done()
			<-started
			putErrors[idx] = listener.put(conn)
		}(i)
	}
	readyCount.Wait()
	close(started)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	wg.Wait()
	for idx, err := range putErrors {
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("goroutine %d put error = %v, want %v", idx, err, net.ErrClosed)
		}
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close returned %v, want %v", err, net.ErrClosed)
	}
}

func TestMuxListenerCloseUnblocksPendingPut(t *testing.T) {
	listener := newMuxListener(&net.TCPAddr{}, 1)
	queued, queuedPeer := net.Pipe()
	defer func() { _ = queuedPeer.Close() }()
	if errPut := listener.put(queued); errPut != nil {
		t.Fatalf("queue connection: %v", errPut)
	}

	pending, pendingPeer := net.Pipe()
	defer func() { _ = pending.Close() }()
	defer func() { _ = pendingPeer.Close() }()
	putDone := make(chan error, 1)
	go func() {
		putDone <- listener.put(pending)
	}()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- listener.Close()
	}()
	select {
	case errPut := <-putDone:
		if !errors.Is(errPut, net.ErrClosed) {
			t.Fatalf("pending put error = %v, want net.ErrClosed", errPut)
		}
	case <-time.After(time.Second):
		t.Fatal("listener close did not unblock pending put")
	}
	select {
	case errClose := <-closeDone:
		if errClose != nil {
			t.Fatalf("close listener: %v", errClose)
		}
	case <-time.After(time.Second):
		t.Fatal("listener close did not complete")
	}
	if errDeadline := queuedPeer.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		if !errors.Is(errDeadline, io.ErrClosedPipe) {
			t.Fatalf("set queued peer read deadline: %v", errDeadline)
		}
	} else {
		var buffer [1]byte
		if _, errRead := queuedPeer.Read(buffer[:]); errRead == nil {
			t.Fatal("queued connection remained open after listener close")
		}
	}
}

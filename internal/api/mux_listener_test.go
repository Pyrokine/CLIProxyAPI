package api

import (
	"errors"
	"io"
	"net"
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

package api

import (
	"net"
	"sync"
)

type muxListener struct {
	addr    net.Addr
	connCh  chan net.Conn
	closeCh chan struct{}

	mu     sync.Mutex
	closed bool
	puts   sync.WaitGroup
	once   sync.Once
}

func newMuxListener(addr net.Addr, buffer int) *muxListener {
	if buffer <= 0 {
		buffer = 1
	}
	return &muxListener{
		addr:    addr,
		connCh:  make(chan net.Conn, buffer),
		closeCh: make(chan struct{}),
	}
}

func (l *muxListener) put(conn net.Conn) error {
	if conn == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return net.ErrClosed
	}
	l.puts.Add(1)
	l.mu.Unlock()
	defer l.puts.Done()

	select {
	case <-l.closeCh:
		return net.ErrClosed
	case l.connCh <- conn:
		return nil
	}
}

func (l *muxListener) Accept() (net.Conn, error) {
	select {
	case <-l.closeCh:
		return nil, net.ErrClosed
	default:
	}
	select {
	case <-l.closeCh:
		return nil, net.ErrClosed
	case conn := <-l.connCh:
		if conn == nil {
			return nil, net.ErrClosed
		}
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

func (l *muxListener) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(
		func() {
			l.mu.Lock()
			l.closed = true
			close(l.closeCh)
			l.mu.Unlock()

			l.puts.Wait()
			for {
				select {
				case conn := <-l.connCh:
					if conn != nil {
						_ = conn.Close()
					}
				default:
					return
				}
			}
		},
	)
	return nil
}

func (l *muxListener) Addr() net.Addr {
	if l == nil {
		return &net.TCPAddr{}
	}
	if l.addr == nil {
		return &net.TCPAddr{}
	}
	return l.addr
}

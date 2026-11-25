package api

import (
	"bufio"
	"crypto/tls"
	"net"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c == nil {
		return 0, net.ErrClosed
	}
	if c.reader == nil {
		return c.Conn.Read(p)
	}
	return c.reader.Read(p)
}

// net/http 根据 ConnectionState 是否存在决定是否填充 Request.TLS
type bufferedTLSConn struct {
	*bufferedConn
	tlsConn *tls.Conn
}

func (c *bufferedTLSConn) ConnectionState() tls.ConnectionState {
	return c.tlsConn.ConnectionState()
}

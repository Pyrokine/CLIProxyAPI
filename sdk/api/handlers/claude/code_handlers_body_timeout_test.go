package claude

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestReadClaudeRequestBodyReturnsRequestTimeoutBeforeProxyDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(
		"/v1/messages", func(c *gin.Context) {
			_, status, errRead := readClaudeRequestBody(c, 50*time.Millisecond)
			if errRead == nil {
				c.Status(http.StatusNoContent)
				return
			}
			c.JSON(status, gin.H{"status": status})
		},
	)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, errDial := net.Dial("tcp", server.Listener.Addr().String())
	if errDial != nil {
		t.Fatalf("dial test server: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	if _, errWrite := fmt.Fprintf(
		conn,
		"POST /v1/messages HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{",
	); errWrite != nil {
		t.Fatalf("write partial request: %v", errWrite)
	}
	if errDeadline := conn.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		t.Fatalf("set client read deadline: %v", errDeadline)
	}

	response, errResponse := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if errResponse != nil {
		t.Fatalf("read timeout response: %v", errResponse)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("response status = %d, want %d", response.StatusCode, http.StatusRequestTimeout)
	}
}

func TestReadClaudeRequestBodyRefreshesDeadlineWhileUploadProgresses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(
		"/v1/messages", func(c *gin.Context) {
			body, status, errRead := readClaudeRequestBody(c, 150*time.Millisecond)
			if errRead != nil {
				c.JSON(status, gin.H{"status": status})
				return
			}
			if string(body) != `{"ok":true}` {
				c.Status(http.StatusBadRequest)
				return
			}
			c.Status(http.StatusNoContent)
		},
	)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, errDial := net.Dial("tcp", server.Listener.Addr().String())
	if errDial != nil {
		t.Fatalf("dial test server: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	if _, errWrite := fmt.Fprintf(
		conn,
		"POST /v1/messages HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n{\"",
	); errWrite != nil {
		t.Fatalf("write request headers: %v", errWrite)
	}
	for _, chunk := range []string{"ok", "\":tr", "ue}"} {
		time.Sleep(75 * time.Millisecond)
		if _, errWrite := conn.Write([]byte(chunk)); errWrite != nil {
			t.Fatalf("write request chunk: %v", errWrite)
		}
	}
	if errDeadline := conn.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		t.Fatalf("set client read deadline: %v", errDeadline)
	}

	response, errResponse := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if errResponse != nil {
		t.Fatalf("read response: %v", errResponse)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

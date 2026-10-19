package executor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gorilla/websocket"
)

type websocketSessionConnector interface {
	ensureUpstreamConn(context.Context, *cliproxyauth.Auth, *codexWebsocketSession, string, string, http.Header) (*websocket.Conn, *http.Response, error)
}

func TestWebsocketRequestProxyChangesConnectionIdentity(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		t.Run(provider, func(t *testing.T) {
			upstreamConnections := make(chan struct{}, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				upstreamConnections <- struct{}{}
				defer func() { _ = conn.Close() }()
				for {
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
				}
			}))
			t.Cleanup(upstream.Close)

			proxyHits := make(chan string, 4)
			proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
					return
				}
				upstreamConn, err := net.DialTimeout("tcp", r.Host, 3*time.Second)
				if err != nil {
					http.Error(w, "upstream unavailable", http.StatusBadGateway)
					return
				}
				clientConn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					_ = upstreamConn.Close()
					t.Errorf("hijack proxy connection: %v", err)
					return
				}
				defer func() { _ = clientConn.Close() }()
				defer func() { _ = upstreamConn.Close() }()
				if _, errWrite := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); errWrite != nil {
					t.Errorf("write CONNECT response: %v", errWrite)
					return
				}
				proxyHits <- r.Host
				go func() {
					_, _ = io.Copy(upstreamConn, clientConn)
					_ = upstreamConn.Close()
				}()
				_, _ = io.Copy(clientConn, upstreamConn)
			}))
			t.Cleanup(proxyServer.Close)

			var connector websocketSessionConnector
			const sessionID = "request-proxy-test"
			var sess *codexWebsocketSession
			if provider == "codex" {
				exec := newCodexWebsocketsExecutor(&config.Config{})
				connector = exec
				sess = exec.getOrCreateSession(sessionID)
				t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			} else {
				exec := newXAIWebsocketsExecutor(&config.Config{})
				exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				connector = exec
				sess = exec.store.getOrCreate(sessionID)
				t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			}
			auth := &cliproxyauth.Auth{ID: "auth-a", Metadata: map[string]any{"access_token": "token-a"}}
			wsURL := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/responses"
			connect := func(ctx context.Context) *websocket.Conn {
				t.Helper()
				conn, _, err := connector.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, http.Header{})
				if err != nil {
					t.Fatalf("ensureUpstreamConn() error = %v", err)
				}
				return conn
			}
			assertUpstreamConnection := func() {
				t.Helper()
				select {
				case <-upstreamConnections:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream websocket was not established")
				}
			}

			first := connect(context.Background())
			assertUpstreamConnection()
			proxyCtx := cliproxyexecutor.WithRequestProxyURL(context.Background(), proxyServer.URL)
			second := connect(proxyCtx)
			if first == second {
				t.Fatal("direct websocket reused after request proxy changed")
			}
			assertUpstreamConnection()
			select {
			case host := <-proxyHits:
				if host != strings.TrimPrefix(upstream.URL, "http://") {
					t.Fatalf("CONNECT host = %q, want upstream host", host)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request proxy did not receive CONNECT")
			}
			if sameProxy := connect(proxyCtx); sameProxy != second {
				t.Fatal("unchanged request proxy did not reuse the websocket")
			}
			third := connect(context.Background())
			if third == second {
				t.Fatal("proxied websocket reused without request proxy")
			}
			assertUpstreamConnection()
			sess.connMu.Lock()
			storedProxy := sess.proxyURL
			sess.connMu.Unlock()
			if storedProxy != "" {
				t.Fatalf("session proxy after direct reconnect = %q, want empty", storedProxy)
			}
		})
	}
}

func TestWebsocketRequestProxyInvalidURLFailsClosed(t *testing.T) {
	for _, proxyURL := range []string{"invalid-proxy", "http://127.0.0.1:0", "http://127.0.0.1:65536"} {
		t.Run(proxyURL, func(t *testing.T) {
			ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), proxyURL)
			dialer, err := newProxyAwareWebsocketDialer(ctx, &config.Config{}, nil)
			if err == nil || dialer != nil {
				t.Fatalf("newProxyAwareWebsocketDialer(%q) = %v, %v; want error", proxyURL, dialer, err)
			}
		})
	}
}

func TestWebsocketRequestProxyOverridesAuthAndGlobal(t *testing.T) {
	const requestProxy = "http://127.0.0.1:9001"
	ctx := cliproxyexecutor.WithRequestProxyURL(context.Background(), requestProxy)
	dialer, err := newProxyAwareWebsocketDialer(ctx,
		&config.Config{ProxyURL: "http://127.0.0.1:9002"},
		&cliproxyauth.Auth{ProxyURL: "http://127.0.0.1:9003"})
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := dialer.Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}})
	if err != nil || proxyURL == nil || proxyURL.String() != requestProxy {
		t.Fatalf("dialer proxy = %v, %v; want %s", proxyURL, err, requestProxy)
	}
}

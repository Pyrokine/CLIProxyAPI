package executor

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func newMemoryResponsesWebsocket(t *testing.T) (*websocket.Conn, <-chan error) {
	t.Helper()
	client, peer := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	closed := make(chan struct{})
	result := make(chan error, 1)
	t.Cleanup(func() { close(closed); _ = client.Close(); _ = peer.Close() })
	go func() {
		reader := bufio.NewReader(peer)
		req, err := http.ReadRequest(reader)
		if err != nil {
			result <- err
			return
		}
		// WebSocket 握手使用 RFC 6455 规定的摘要
		sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		if _, err := fmt.Fprintf(
			peer,
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: "+
				"Upgrade\r\nSec-WebSocket-Accept: "+
				"%s\r\n\r\n",
			base64.StdEncoding.EncodeToString(sum[:]),
		); err != nil {
			result <- err
			return
		}
		var header [2]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			result <- err
			return
		}
		length := uint64(header[1] & 127)
		if length == 126 {
			var extended [2]byte
			if _, err := io.ReadFull(reader, extended[:]); err != nil {
				result <- err
				return
			}
			length = uint64(binary.BigEndian.Uint16(extended[:]))
		}
		if length == 127 || length > 65535 || header[1]&128 == 0 {
			result <- fmt.Errorf("unexpected synthetic request frame")
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(length)+4); err != nil {
			result <- err
			return
		}
		for _, payload := range []string{
			`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42}}}`,
			`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`,
		} {
			frame := append([]byte{0x81, byte(len(payload))}, payload...)
			if _, err := peer.Write(frame); err != nil {
				result <- err
				return
			}
		}
		result <- nil
		<-closed
	}()
	dialer := websocket.Dialer{
		NetDialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil },
		ReadBufferSize: 1024, WriteBufferSize: 1024,
	}
	conn, response, err := dialer.DialContext(t.Context(), "ws://continuation125.invalid/responses", nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return conn, result
}

type xaiMemoryHomeDispatcher struct {
	calls atomic.Int32
}

func (*xaiMemoryHomeDispatcher) HeartbeatOK() bool { return true }

func (d *xaiMemoryHomeDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	d.calls.Add(1)
	return []byte((`{"model":"grok-4.3","auth_index":"synthetic-xai","auth":{"id":"synthetic-xai","provider":"` +
		`xai","status":"active","attributes":{"api_key":"synthetic-key","base_url":"http://continuation125.` +
		`invalid","websockets":"true"}}}`)), nil
}

func TestXAIManagerRegistryOwnsWebsocketOriginalContract(t *testing.T) {
	for _, explicitClose := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("explicit_close=%t", explicitClose), func(t *testing.T) {
				conn, peer := newMemoryResponsesWebsocket(t)
				cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
				executor := NewXAIAutoExecutor(cfg)
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
				registry := executionregistry.New()
				dispatcher := &xaiMemoryHomeDispatcher{}
				bundle := manager.PublishHomeDispatch(dispatcher, registry, 1)
				t.Cleanup(func() { manager.ClearHomeDispatchBundle(bundle) })
				manager.RegisterExecutor(executor)
				sessionID := fmt.Sprintf("synthetic-xai-manager-%t", explicitClose)
				auth := &cliproxyauth.Auth{
					ID: "synthetic-xai", Provider: "xai", Attributes: map[string]string{
						"api_key": "synthetic-key", "base_url": "http://continuation125.invalid", "websockets": "true",
					},
				}
				session := executor.wsExec.store.getOrCreate(sessionID)
				session.conn, session.readerConn = conn, conn
				session.connCloser = newWebsocketConnectionCloser(conn)
				session.authID, session.credentialSHA256 = auth.ID, cliproxyauth.AccessTokenSHA256(auth)
				session.wsURL = "ws://continuation125.invalid/responses"
				configureXAIWebsocketConn(session, conn)
				go executor.wsExec.readSessionLoop(session, conn)
				t.Cleanup(func() { manager.CloseExecutionSession(sessionID) })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ctx = util.WithRoundTripper(
					ctx, claudeContractTransport(
						func(*http.Request) (*http.Response, error) {
							return nil, fmt.Errorf("unexpected HTTP request in XAI memory consumer")
						},
					),
				)
				ctx = cliproxyexecutor.WithDownstreamWebsocket(
					cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx),
				)
				opts := cliproxyexecutor.Options{
					Stream: true, SourceFormat: sdktranslator.FormatOpenAIResponse,
					ResponseFormat: sdktranslator.FormatOpenAIResponse,
					Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
				}
				stream, err := manager.ExecuteStream(
					ctx, []string{"xai"}, cliproxyexecutor.Request{
						Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"synthetic"}`),
					}, opts,
				)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
				}
				if err := <-peer; err != nil {
					t.Fatal(err)
				}
				if dispatcher.calls.Load() != 1 {
					t.Fatalf("dispatch calls=%d, want 1", dispatcher.calls.Load())
				}
				if explicitClose {
					manager.CloseExecutionSession(sessionID)
				}
				drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
				defer drainCancel()
				if err := registry.Drain(drainCtx); err != nil {
					t.Fatal(err)
				}
				if err := conn.NetConn().SetWriteDeadline(time.Now().Add(time.Second)); !errors.Is(
					err, io.ErrClosedPipe,
				) {
					t.Fatalf(
						"XAI Manager completed and Registry.Drain returned nil; socket=%v, want closed pipe",
						err,
					)
				}
			},
		)
	}
}

func TestXAIResponseIDRewritePreservesOriginalData(t *testing.T) {
	payload := []byte((`{"type":"response.completed","response":{"id":"resp-up","previous_response_id":"prev-` +
		`up","model":"resp-up-finetune","output":[{"id":"msg_resp-up","type":"message","role":"assistant","` +
		`content":[{"type":"output_text","text":"literal resp-up and prev-up"}]},{"id":"tool_resp-up","type":` +
		`"function_call","name":"echo","arguments":"{\"value\":\"resp-up\",\"n\":9007199254740993}"}]}}`))
	for _, rewritten := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("rewritten=%t", rewritten), func(t *testing.T) {
				responseID, previousID := "resp-up", "prev-up"
				if rewritten {
					responseID, previousID = "resp-down", "prev-down"
				}
				out := rewriteXAIWebsocketDownstreamIDs(payload, "resp-up", responseID, "prev-up", previousID)
				if gjson.GetBytes(out, "response.id").String() != responseID || gjson.GetBytes(
					out, "response.previous_response_id",
				).String() != previousID || gjson.GetBytes(out, "response.output.0.id").String() != "msg_"+responseID {
					t.Fatalf("structural IDs were not rewritten: %s", out)
				}
				for _, field := range []string{
					"response.model", "response.output.0.content.0.text", "response.output.1.arguments",
				} {
					if got, want := gjson.GetBytes(out, field).String(), gjson.GetBytes(
						payload, field,
					).String(); got != want {
						t.Errorf("response data %s changed: got=%q want=%q", field, got, want)
					}
				}
			},
		)
	}
}

func TestXAIEmittedResponseIDOriginalFourTurnRoundTrip(t *testing.T) {
	store := &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	previous := ""
	for turn := range 4 {
		payload := []byte(fmt.Sprintf(`{"previous_response_id":%q,"input":[]}`, previous))
		mapper := newXAIWebsocketRequestIDMapper(store, "synthetic-id-roundtrip", payload)
		upstream, err := mapper.upstreamRequestPayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		wantPrevious := ""
		if turn > 0 {
			wantPrevious = "resp-up"
		}
		if got := gjson.GetBytes(upstream, "previous_response_id").String(); got != wantPrevious {
			t.Fatalf(
				"turn=%d emitted previous_response_id=%q became upstream=%q, want %q", turn+1, previous, got,
				wantPrevious,
			)
		}
		response := mapper.downstreamResponsePayload(
			[]byte(`{"type":"response.completed","response":{"id":"resp-up","output":[]}}`),
		)
		previous = gjson.GetBytes(response, "response.id").String()
		t.Logf("turn=%d emitted ID=%s", turn+1, previous)
	}
}

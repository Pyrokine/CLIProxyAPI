package management

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/google/uuid"
	"golang.org/x/net/dns/dnsmessage"
)

func TestModelCatalogRefreshReceiptFollowsActualPublication(t *testing.T) {
	const child = "CPA_TEST_CATALOG_REFRESH_CHILD"
	if os.Getenv(child) != "1" {
		command := exec.Command(
			os.Args[0], "-test.run=^TestModelCatalogRefreshReceiptFollowsActualPublication$", "-test.timeout=30s",
		)
		command.Env = []string{child + "=1"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated catalog consumer: %v\n%s", err, output)
		}
		return
	}

	previousResolver, previousTransport := net.DefaultResolver, http.DefaultTransport
	net.DefaultResolver = catalogReceiptTestResolver(t)
	t.Cleanup(func() { net.DefaultResolver, http.DefaultTransport = previousResolver, previousTransport })
	body, err := json.Marshal(map[string]any{"claude": registry.GetStaticModelDefinitionsByChannel("claude")})
	if err != nil {
		t.Fatal(err)
	}
	allowed := make(map[string]bool)
	for _, source := range registry.GetCatalogMeta().Sources {
		allowed[source] = true
	}
	mode := "success"
	http.DefaultTransport = roundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			if err := request.Context().Err(); err != nil {
				return nil, err
			}
			if request.Method != http.MethodGet || !allowed[request.URL.String()] {
				return nil, fmt.Errorf("unexpected synthetic catalog request")
			}
			status, value := 200, string(body)
			if mode == "failure" {
				status, value = 503, "synthetic catalog failure"
			}
			return &http.Response{
				StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(value)),
			}, nil
		},
	)
	h := &Handler{}
	engine := resourceMutationEngine(t, h)
	engine.POST("/models-catalog-refresh", h.PostModelsCatalogRefresh)
	for _, name := range []string{"success", "unchanged publication", "failure", "cancelled"} {
		mode = name
		request := httptest.NewRequest(http.MethodPost, "/models-catalog-refresh", nil)
		request.Header.Set("X-CPA-Mutation-ID", uuid.NewString())
		if name == "cancelled" {
			ctx, cancel := context.WithCancel(request.Context())
			cancel()
			request = request.WithContext(ctx)
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		result := decodeMutationHeader(t, response)
		if name == "success" || name == "unchanged publication" {
			if response.Code != 200 || result.Outcome != "succeeded" || !result.ConfirmedEffect ||
				len(result.Effects) != 1 ||
				result.Effects[0].Kind != "model.published" {
				t.Fatalf("%s status=%d result=%+v body=%s", name, response.Code, result, response.Body.String())
			}
		} else if response.Code != 502 || result.ConfirmedEffect || len(result.Effects) != 0 {
			t.Fatalf("%s status=%d result=%+v", name, response.Code, result)
		}
	}
}

func catalogReceiptTestResolver(t *testing.T) *net.Resolver {
	t.Helper()
	return &net.Resolver{
		PreferGo: true, Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer func(closeResource func() error) { _ = closeResource() }(server.Close)
				buffer := make([]byte, 4096)
				// net.Pipe is a stream connection, so Resolver prefixes each DNS message with its length.
				var length [2]byte
				_, err := io.ReadFull(server, length[:])
				n := int(binary.BigEndian.Uint16(length[:]))
				if err == nil {
					_, err = io.ReadFull(server, buffer[:n])
				}
				if err != nil {
					return
				}
				var query dnsmessage.Message
				if err := query.Unpack(buffer[:n]); err != nil {
					t.Error(err)
					return
				}
				response := dnsmessage.Message{
					ID: query.Header.ID, Response: true, RecursionAvailable: true, Questions: query.Questions,
				}
				for _, question := range query.Questions {
					if question.Type == dnsmessage.TypeA {
						response.Answers = append(
							response.Answers, dnsmessage.Resource{
								Header: dnsmessage.ResourceHeader{
									Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60,
								}, Body: &dnsmessage.AResource{A: [4]byte{8, 8, 8, 8}},
							},
						)
					}
				}
				wire, err := response.Pack()
				if err != nil {
					t.Error(err)
					return
				}
				var prefix [2]byte
				binary.BigEndian.PutUint16(prefix[:], uint16(len(wire)))
				wire = append(prefix[:], wire...)
				_, _ = server.Write(wire)
			}()
			return client, nil
		},
	}
}

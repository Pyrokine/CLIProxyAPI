package tui

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestNewClientUsesConfiguredTLSCertificate(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			},
		),
	)
	defer server.Close()

	certificate := server.Certificate()
	certPath := filepath.Join(t.TempDir(), "server-cert.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if errWrite := os.WriteFile(certPath, certPEM, 0o600); errWrite != nil {
		t.Fatalf("write server certificate: %v", errWrite)
	}
	parsedURL, errParse := url.Parse(server.URL)
	if errParse != nil {
		t.Fatalf("parse test server URL: %v", errParse)
	}
	_, rawPort, errSplit := net.SplitHostPort(parsedURL.Host)
	if errSplit != nil {
		t.Fatalf("split test server address: %v", errSplit)
	}
	port, errPort := strconv.Atoi(rawPort)
	if errPort != nil {
		t.Fatalf("parse test server port: %v", errPort)
	}

	client, errClient := NewClient(port, "", config.TLSConfig{Enable: true, Cert: certPath})
	if errClient != nil {
		t.Fatalf("NewClient() error = %v", errClient)
	}
	body, status, errRequest := client.doRequest(http.MethodGet, "/", nil)
	if errRequest != nil {
		t.Fatalf("TLS request error = %v", errRequest)
	}
	if status != http.StatusOK || string(body) != `{"status":"ok"}` {
		t.Fatalf("TLS response = %d %s", status, body)
	}
}

func TestClientCancelAuthSessionReturnsServerDecision(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{name: "cancelled", body: `{"status":"ok","cancelled":true}`, want: true},
		{name: "not cancelled", body: `{"status":"ok","cancelled":false}`, want: false},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							if r.Method != http.MethodDelete {
								t.Fatalf("method = %s, want DELETE", r.Method)
							}
							if got := r.URL.Query().Get("state"); got != "state-1" {
								t.Fatalf("state = %q, want state-1", got)
							}
							_, _ = w.Write([]byte(tt.body))
						},
					),
				)
				defer server.Close()

				client := &Client{baseURL: server.URL, http: server.Client()}
				got, err := client.cancelAuthSession("state-1")
				if err != nil {
					t.Fatalf("CancelAuthSession() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("CancelAuthSession() = %v, want %v", got, tt.want)
				}
			},
		)
	}
}

func TestClientCancelAuthSessionRejectsMissingDecision(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			},
		),
	)
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	_, err := client.cancelAuthSession("state-1")
	if err == nil || !strings.Contains(err.Error(), "missing cancelled field") {
		t.Fatalf("CancelAuthSession() error = %v", err)
	}
}

func TestClientRefreshAllAuthFilesUsesExplicitBulkMode(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files/refresh" {
					t.Fatalf("request = %s %s", r.Method, r.URL.Path)
				}
				var payload map[string]bool
				if errDecode := json.NewDecoder(r.Body).Decode(&payload); errDecode != nil {
					t.Fatalf("decode request: %v", errDecode)
				}
				if !payload["all"] || len(payload) != 1 {
					t.Fatalf("request body = %#v", payload)
				}
				time.Sleep(10 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
			},
		),
	)
	defer server.Close()

	httpClient := server.Client()
	httpClient.Timeout = time.Millisecond
	client := &Client{baseURL: server.URL, http: httpClient}
	if err := client.refreshAllAuthFiles(); err != nil {
		t.Fatalf("refreshAllAuthFiles() error = %v", err)
	}
}

func TestClientRefreshAuthFileUsesExactIdentity(t *testing.T) {
	const name = `所选 "same".json`
	const authIndex = "selected-index"
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files/refresh" ||
					r.URL.RawQuery != "" {
					t.Errorf("request = %s %s", r.Method, r.URL)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode refresh body: %v", err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				if len(payload) != 2 || payload["name"] != name || payload["auth_index"] != authIndex {
					t.Errorf("single refresh body = %#v", payload)
					http.Error(w, "wrong target", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			},
		),
	)
	defer server.Close()
	client := &Client{baseURL: server.URL, http: server.Client()}
	if err := client.refreshAuthFile(name, authIndex); err != nil {
		t.Fatalf("refreshAuthFile() error = %v", err)
	}
}

type authRefreshTransport func(*http.Request) (*http.Response, error)

func (transport authRefreshTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestClientAuthRefreshUsesEndpointDeadline(t *testing.T) {
	for _, tt := range []struct {
		name    string
		bulk    bool
		timeout time.Duration
	}{
		{"selected", false, 35 * time.Second},
		{"all", true, 4*time.Minute + 35*time.Second},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				calls := 0
				client := &Client{
					baseURL: "http://fixture.invalid", http: &http.Client{
						Timeout: time.Millisecond,
						Transport: authRefreshTransport(
							func(r *http.Request) (*http.Response, error) {
								calls++
								deadline, ok := r.Context().Deadline()
								remaining := time.Until(deadline)
								if !ok || remaining <= tt.timeout-time.Second || remaining > tt.timeout {
									t.Errorf(
										"refresh deadline remaining=%s present=%t, want %s", remaining, ok, tt.timeout,
									)
								}
								return &http.Response{
									StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"results":[]}`)),
								}, nil
							},
						),
					},
				}
				var err error
				if tt.bulk {
					err = client.refreshAllAuthFiles()
				} else {
					err = client.refreshAuthFile("selected.json", "selected-index")
				}
				if err != nil || calls != 1 || client.http.Timeout != time.Millisecond {
					t.Fatalf("refresh error=%v calls=%d shared timeout=%s", err, calls, client.http.Timeout)
				}
			},
		)
	}
}

func TestClientAuthRefreshPreservesTransportFailure(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		failure := errors.New("fixture transport unavailable")
		calls := 0
		client := &Client{
			baseURL: "http://fixture.invalid", http: &http.Client{
				Transport: authRefreshTransport(
					func(*http.Request) (*http.Response, error) {
						calls++
						return nil, failure
					},
				),
			},
		}
		var err error
		if bulk {
			err = client.refreshAllAuthFiles()
		} else {
			err = client.refreshAuthFile("selected.json", "selected-index")
		}
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("bulk=%t error=%v calls=%d", bulk, err, calls)
		}
	}
}

func TestClientBulkRefreshReportsPerCredentialFailures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		code    int
		body    string
		wantErr bool
	}{
		{"legacy empty response", http.StatusOK, "", false},
		{"no targets", http.StatusOK, `{"results":[]}`, false},
		{"all succeeded", http.StatusOK, `{"results":[{"success":true},{"success":true}]}`, false},
		{"partial failure", http.StatusOK, `{"results":[{"success":true},{"success":false}]}`, true},
		{"all failed", http.StatusOK, `{"results":[{"success":false},{"success":false}]}`, true},
		{"invalid JSON", http.StatusOK, `{"results":`, true},
		{"invalid results", http.StatusOK, `{"results":"unavailable"}`, true},
		{"request rejected", http.StatusServiceUnavailable, `{}`, true},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, _ *http.Request) {
							w.WriteHeader(tt.code)
							_, _ = w.Write([]byte(tt.body))
						},
					),
				)
				defer server.Close()
				client := &Client{baseURL: server.URL, http: server.Client()}
				err := client.refreshAllAuthFiles()
				if (err != nil) != tt.wantErr {
					t.Fatalf("bulk refresh error=%v, want error=%t", err, tt.wantErr)
				}
			},
		)
	}
}

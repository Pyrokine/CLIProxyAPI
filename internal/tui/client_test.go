package tui

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestNewClientUsesConfiguredTLSCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
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
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Fatalf("method = %s, want DELETE", r.Method)
				}
				if got := r.URL.Query().Get("state"); got != "state-1" {
					t.Fatalf("state = %q, want state-1", got)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := &Client{baseURL: server.URL, http: server.Client()}
			got, err := client.CancelAuthSession("state-1")
			if err != nil {
				t.Fatalf("CancelAuthSession() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("CancelAuthSession() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClientCancelAuthSessionRejectsMissingDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	_, err := client.CancelAuthSession("state-1")
	if err == nil || !strings.Contains(err.Error(), "missing cancelled field") {
		t.Fatalf("CancelAuthSession() error = %v", err)
	}
}

package home

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadRESPBulkRejectsOversizedResponse(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("$1048577\r\n"))

	if _, err := readRESPBulk(reader); err == nil {
		t.Fatalf("readRESPBulk() error = nil, want oversized response error")
	}
}

func TestVerifyPeerCertificateChain(t *testing.T) {
	_, _, fingerprint, rawDER := generateHomeCertificateTestCert(t)

	if err := verifyPeerCertificateChain([][]byte{rawDER}, fingerprint, "127.0.0.1"); err != nil {
		t.Fatalf("verifyPeerCertificateChain() error = %v", err)
	}
	if err := verifyPeerCertificateChain([][]byte{rawDER}, strings.Repeat("0", 64), "127.0.0.1"); err == nil {
		t.Fatalf("verifyPeerCertificateChain() error = nil, want mismatch")
	}
}

func TestVerifyPeerCertificateChainRejectsUnrelatedPinnedCA(t *testing.T) {
	rawCerts, unrelatedFingerprint := generateHomeCertificateMismatchedChain(t)

	if err := verifyPeerCertificateChain(rawCerts, unrelatedFingerprint, "127.0.0.1"); err == nil {
		t.Fatalf("verifyPeerCertificateChain() error = nil, want chain verification failure")
	}
}

func TestRequestClientCertificateUsesPinnedTLS(t *testing.T) {
	cert, certPEM, fingerprint, _ := generateHomeCertificateTestCert(t)
	listener, errListen := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if errListen != nil {
		t.Fatalf("tls.Listen: %v", errListen)
	}
	defer func() { _ = listener.Close() }()

	argsCh := make(chan []string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			errCh <- errAccept
			return
		}
		defer func() { _ = conn.Close() }()

		args, errRead := readCertificateTestRESPArray(bufio.NewReader(conn))
		if errRead != nil {
			errCh <- errRead
			return
		}
		payload, errJSON := json.Marshal(
			certificateRequestResponse{
				OK:          true,
				Certificate: "client-cert",
				CA:          string(certPEM),
			},
		)
		if errJSON != nil {
			errCh <- errJSON
			return
		}
		if _, errWrite := fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(payload), payload); errWrite != nil {
			errCh <- errWrite
			return
		}
		argsCh <- args
	}()

	_, portText, errSplit := net.SplitHostPort(listener.Addr().String())
	if errSplit != nil {
		t.Fatalf("SplitHostPort: %v", errSplit)
	}
	port, errPort := strconv.Atoi(portText)
	if errPort != nil {
		t.Fatalf("Atoi port: %v", errPort)
	}

	resp, errRequest := requestClientCertificate(
		context.Background(),
		homeJWTClaims{
			CertificateID:    "client-1",
			ClusterID:        "cluster-1",
			CAFingerprint:    fingerprint,
			EnrollmentSecret: "enroll-secret",
			IP:               "127.0.0.1",
			Port:             port,
		},
		[]byte("csr-pem"),
	)
	if errRequest != nil {
		t.Fatalf("requestClientCertificate: %v", errRequest)
	}
	if !resp.OK || resp.Certificate != "client-cert" || resp.CA != string(certPEM) {
		t.Fatalf("unexpected certificate response: %#v", resp)
	}

	select {
	case args := <-argsCh:
		want := []string{"CERTIFICATE", "REQUEST", "client-1", "enroll-secret", "csr-pem"}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("request args = %#v, want %#v", args, want)
		}
	case err := <-errCh:
		t.Fatalf("TLS server error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for TLS server")
	}
}

func generateHomeCertificateMismatchedChain(t *testing.T) ([][]byte, string) {
	t.Helper()

	_, _, _, serverRawDER := generateHomeCertificateTestCert(t)
	_, _, caFingerprint, caRawDER := generateHomeCertificateTestCert(t)
	return [][]byte{serverRawDER, caRawDER}, caFingerprint
}

func generateHomeCertificateTestCert(t *testing.T) (tls.Certificate, []byte, string, []byte) {
	t.Helper()

	key, errKey := rsa.GenerateKey(rand.Reader, 2048)
	if errKey != nil {
		t.Fatalf("GenerateKey: %v", errKey)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, errCert := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if errCert != nil {
		t.Fatalf("CreateCertificate: %v", errCert)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, errPair := tls.X509KeyPair(certPEM, keyPEM)
	if errPair != nil {
		t.Fatalf("X509KeyPair: %v", errPair)
	}
	sum := sha256.Sum256(der)
	return cert, certPEM, hex.EncodeToString(sum[:]), der
}

func readCertificateTestRESPArray(reader *bufio.Reader) ([]string, error) {
	prefix, errPrefix := reader.ReadByte()
	if errPrefix != nil {
		return nil, errPrefix
	}
	if prefix != '*' {
		return nil, fmt.Errorf("expected array prefix, got %q", prefix)
	}
	line, errLine := reader.ReadString('\n')
	if errLine != nil {
		return nil, errLine
	}
	count, errCount := strconv.Atoi(strings.TrimSpace(line))
	if errCount != nil {
		return nil, errCount
	}
	args := make([]string, 0, count)
	for range count {
		bulkPrefix, errBulkPrefix := reader.ReadByte()
		if errBulkPrefix != nil {
			return nil, errBulkPrefix
		}
		if bulkPrefix != '$' {
			return nil, fmt.Errorf("expected bulk prefix, got %q", bulkPrefix)
		}
		lengthLine, errLength := reader.ReadString('\n')
		if errLength != nil {
			return nil, errLength
		}
		length, errParse := strconv.Atoi(strings.TrimSpace(lengthLine))
		if errParse != nil {
			return nil, errParse
		}
		payload := make([]byte, length+2)
		if _, errFull := io.ReadFull(reader, payload); errFull != nil {
			return nil, errFull
		}
		if payload[length] != '\r' || payload[length+1] != '\n' {
			return nil, fmt.Errorf("invalid bulk string terminator")
		}
		args = append(args, string(payload[:length]))
	}
	return args, nil
}

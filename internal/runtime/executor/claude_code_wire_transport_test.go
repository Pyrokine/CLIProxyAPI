package executor

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tls "github.com/refraction-networking/utls"
)

type claudeWireRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeWireRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestClaudeCodeTLSClientHelloSpecMatches220Capture(t *testing.T) {
	fixture := claudeCodeTLSFingerprintFixture{
		clientHelloLength: 508,
		ja3:               "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-21,29-23-24,0",
		ja3MD5:            "d871d02cecbde59abbf8f4806134addf",
		alpn:              []string{"http/1.1"},
		cipherSuites: []uint16{
			4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393, 52392, 49161, 49171, 49162, 49172, 156, 157, 47, 53,
		},
		extensionTypes: []uint16{0, 23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43, 21},
		extensionLengths: [][2]int{
			{0, 22}, {23, 0}, {65281, 1}, {10, 8}, {11, 2}, {35, 0}, {16, 11},
			{5, 5}, {13, 20}, {18, 0}, {51, 38}, {45, 2}, {43, 5}, {21, 231},
		},
		supportedGroups:     []uint16{29, 23, 24},
		pointFormats:        []uint8{0},
		signatureAlgorithms: []uint16{1027, 2052, 1025, 1283, 2053, 1281, 2054, 1537, 513},
		supportedVersions:   []uint16{772, 771},
		keyShareGroups:      []uint16{29},
	}

	record := captureClaudeCodeClientHello(t)
	if got := len(record) - 9; got != fixture.clientHelloLength {
		t.Fatalf("ClientHello length = %d, want %d", got, fixture.clientHelloLength)
	}
	if got := parseClaudeCodeClientHelloExtensionLengths(t, record); !reflect.DeepEqual(got, fixture.extensionLengths) {
		t.Fatalf("extension lengths = %v, want %v", got, fixture.extensionLengths)
	}
	spec, errFingerprint := (&tls.Fingerprinter{}).FingerprintClientHello(record)
	if errFingerprint != nil {
		t.Fatal(errFingerprint)
	}
	actual := summarizeClaudeCodeClientHelloSpec(t, spec)
	if !reflect.DeepEqual(actual.cipherSuites, fixture.cipherSuites) ||
		!reflect.DeepEqual(actual.extensionTypes, fixture.extensionTypes) ||
		!reflect.DeepEqual(actual.alpn, fixture.alpn) ||
		!reflect.DeepEqual(actual.supportedGroups, fixture.supportedGroups) ||
		!reflect.DeepEqual(actual.pointFormats, fixture.pointFormats) ||
		!reflect.DeepEqual(actual.signatureAlgorithms, fixture.signatureAlgorithms) ||
		!reflect.DeepEqual(actual.supportedVersions, fixture.supportedVersions) ||
		!reflect.DeepEqual(actual.keyShareGroups, fixture.keyShareGroups) {
		t.Fatalf("ClientHello summary = %+v, want %+v", actual, fixture)
	}
	if actual.ja3 != fixture.ja3 || actual.ja3MD5 != fixture.ja3MD5 {
		t.Fatalf("JA3 = %q (%s), want %q (%s)", actual.ja3, actual.ja3MD5, fixture.ja3, fixture.ja3MD5)
	}

	transport, ok := newClaudeCodeRoundTripper("").(*http.Transport)
	if !ok {
		t.Fatalf("Claude Code transport type = %T, want *http.Transport", newClaudeCodeRoundTripper(""))
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("Claude Code transport must not force HTTP/2")
	}
}

func TestClaudeCodeTLSResumptionIsWireSafe(t *testing.T) {
	spec := claudeCodeTLSClientHelloSpec()
	if _, ok := spec.Extensions[len(spec.Extensions)-1].(*tls.UtlsPreSharedKeyExtension); !ok {
		t.Fatalf(
			"last inference extension = %T, want *tls.UtlsPreSharedKeyExtension",
			spec.Extensions[len(spec.Extensions)-1],
		)
	}
	if _, ok := spec.Extensions[len(spec.Extensions)-2].(*tls.UtlsPaddingExtension); !ok {
		t.Fatalf(
			"extension before pre_shared_key = %T, want *tls.UtlsPaddingExtension",
			spec.Extensions[len(spec.Extensions)-2],
		)
	}

	cfg := newClaudeCodeTLSConfig(
		"api.anthropic.com",
		tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity),
	)
	if cfg.ClientSessionCache == nil || !cfg.OmitEmptyPsk || !cfg.PreferSkipResumptionOnNilExtension {
		t.Fatalf("TLS resumption config is not wire-safe: %+v", cfg)
	}
}

func TestClaudeCodeRequestHeaderOrderMatchesNative220Capture(t *testing.T) {
	if got := claudeCodeRequestHeaderOrder(http.MethodPost, "/v1/messages?beta=true"); !reflect.DeepEqual(
		got, claudeCodeMessagesHeaderOrder,
	) {
		t.Fatalf("Messages header order = %v, want %v", got, claudeCodeMessagesHeaderOrder)
	}
	if got := claudeCodeRequestHeaderOrder(
		http.MethodPost, "/v1/messages/count_tokens?beta=true",
	); !reflect.DeepEqual(got, claudeCodeCountTokensHeaderOrder) {
		t.Fatalf("count_tokens header order = %v, want %v", got, claudeCodeCountTokensHeaderOrder)
	}
	for _, name := range claudeCodeCountTokensHeaderOrder {
		if name == "X-Stainless-Timeout" {
			t.Fatal("count_tokens header order unexpectedly contains X-Stainless-Timeout")
		}
	}
}

func TestCachedClaudeCodeRoundTripperReusesAndBoundsTransports(t *testing.T) {
	firstProxy := "http://127.0.0.1:32000"
	first := cachedClaudeCodeRoundTripper(firstProxy)
	if second := cachedClaudeCodeRoundTripper(firstProxy); first != second {
		t.Fatal("Claude Code transport cache returned different transports for one proxy")
	}
	for index := 1; index <= claudeCodeRoundTripperCacheCapacity; index++ {
		cachedClaudeCodeRoundTripper(fmt.Sprintf("http://127.0.0.1:%d", 32000+index))
	}
	if got := claudeCodeRoundTripperCache.Len(); got > claudeCodeRoundTripperCacheCapacity {
		t.Fatalf("transport cache entries = %d, want at most %d", got, claudeCodeRoundTripperCacheCapacity)
	}
	if recreated := cachedClaudeCodeRoundTripper(firstProxy); recreated == first {
		t.Fatal("least recently used proxy transport was not evicted")
	}
}

func TestClaudeFallbackRoundTripperSelectsProviderFingerprint(t *testing.T) {
	route := func(label string) http.RoundTripper {
		return claudeWireRoundTripFunc(
			func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"X-Test-Route": []string{label}},
					Body:       io.NopCloser(strings.NewReader("{}")),
					Request:    req,
				}, nil
			},
		)
	}
	roundTripper := &claudeFallbackRoundTripper{
		anthropic: route("anthropic"),
		chrome:    route("chrome"),
		fallback:  route("fallback"),
	}
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "Anthropic HTTPS", url: "https://api.anthropic.com/v1/messages", want: "anthropic"},
		{name: "Anthropic explicit HTTPS port", url: "https://api.anthropic.com:443/v1/messages", want: "anthropic"},
		{name: "Anthropic custom port", url: "https://api.anthropic.com:8443/v1/messages", want: "fallback"},
		{name: "Anthropic userinfo", url: "https://caller@api.anthropic.com/v1/messages", want: "fallback"},
		{name: "Anthropic lookalike", url: "https://api.anthropic.com.example/v1/messages", want: "fallback"},
		{name: "ChatGPT HTTPS", url: "https://chatgpt.com/backend-api/codex/responses", want: "chrome"},
		{name: "Other HTTPS", url: "https://example.com/v1/messages", want: "fallback"},
		{name: "Anthropic HTTP", url: "http://api.anthropic.com/v1/messages", want: "fallback"},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				req, errRequest := http.NewRequest(http.MethodGet, test.url, nil)
				if errRequest != nil {
					t.Fatal(errRequest)
				}
				resp, errRoundTrip := roundTripper.RoundTrip(req)
				if errRoundTrip != nil {
					t.Fatal(errRoundTrip)
				}
					defer func() { _ = resp.Body.Close() }()
				if got := resp.Header.Get("X-Test-Route"); got != test.want {
					t.Fatalf("route = %q, want %q", got, test.want)
				}
			},
		)
	}
}

type claudeCodeTLSFingerprintFixture struct {
	clientHelloLength   int
	ja3                 string
	ja3MD5              string
	alpn                []string
	cipherSuites        []uint16
	extensionTypes      []uint16
	extensionLengths    [][2]int
	supportedGroups     []uint16
	pointFormats        []uint8
	signatureAlgorithms []uint16
	supportedVersions   []uint16
	keyShareGroups      []uint16
}

type claudeCodeClientHelloSummary struct {
	cipherSuites        []uint16
	extensionTypes      []uint16
	alpn                []string
	supportedGroups     []uint16
	pointFormats        []uint8
	signatureAlgorithms []uint16
	supportedVersions   []uint16
	keyShareGroups      []uint16
	ja3                 string
	ja3MD5              string
}

func captureClaudeCodeClientHello(t *testing.T) []byte {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(
		func() {
			if errClose := clientConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				t.Errorf("close client pipe: %v", errClose)
			}
			if errClose := serverConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				t.Errorf("close server pipe: %v", errClose)
			}
		},
	)
	cfg := newClaudeCodeTLSConfig(
		"api.anthropic.com",
		tls.NewLRUClientSessionCache(claudeCodeSessionCacheCapacity),
	)
	tlsConn := tls.UClient(clientConn, cfg, tls.HelloCustom)
	if errPreset := tlsConn.ApplyPreset(claudeCodeTLSClientHelloSpec()); errPreset != nil {
		t.Fatal(errPreset)
	}
	handshakeDone := make(chan error, 1)
	go func() { handshakeDone <- tlsConn.Handshake() }()
	if errDeadline := serverConn.SetReadDeadline(time.Now().Add(5 * time.Second)); errDeadline != nil {
		t.Fatal(errDeadline)
	}
	header := make([]byte, 5)
	if _, errRead := io.ReadFull(serverConn, header); errRead != nil {
		t.Fatal(errRead)
	}
	payload := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
	if _, errRead := io.ReadFull(serverConn, payload); errRead != nil {
		t.Fatal(errRead)
	}
	if errClose := serverConn.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	select {
	case <-handshakeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("uTLS handshake did not exit after the capture connection closed")
	}
	return append(header, payload...)
}

func parseClaudeCodeClientHelloExtensionLengths(t *testing.T, record []byte) [][2]int {
	t.Helper()
	if len(record) < 9 || record[0] != 22 || record[5] != 1 {
		t.Fatal("invalid TLS ClientHello record")
	}
	body := record[9:]
	offset := 2 + 32
	if offset >= len(body) {
		t.Fatal("truncated ClientHello random")
	}
	offset += 1 + int(body[offset])
	if offset+2 > len(body) {
		t.Fatal("truncated ClientHello cipher suites")
	}
	offset += 2 + int(binary.BigEndian.Uint16(body[offset:offset+2]))
	if offset >= len(body) {
		t.Fatal("truncated ClientHello compression methods")
	}
	offset += 1 + int(body[offset])
	if offset+2 > len(body) {
		t.Fatal("truncated ClientHello extensions")
	}
	extensionsLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
	offset += 2
	end := offset + extensionsLength
	if end > len(body) {
		t.Fatal("truncated ClientHello extension data")
	}
	lengths := make([][2]int, 0)
	for offset+4 <= end {
		extensionType := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		extensionLength := int(binary.BigEndian.Uint16(body[offset+2 : offset+4]))
		lengths = append(lengths, [2]int{extensionType, extensionLength})
		offset += 4 + extensionLength
	}
	if offset != end {
		t.Fatal("misaligned ClientHello extension data")
	}
	return lengths
}

func summarizeClaudeCodeClientHelloSpec(t *testing.T, spec *tls.ClientHelloSpec) claudeCodeClientHelloSummary {
	t.Helper()
	summary := claudeCodeClientHelloSummary{cipherSuites: append([]uint16(nil), spec.CipherSuites...)}
	for _, extension := range spec.Extensions {
		switch ext := extension.(type) {
		case *tls.SNIExtension:
			summary.extensionTypes = append(summary.extensionTypes, 0)
		case *tls.ExtendedMasterSecretExtension:
			summary.extensionTypes = append(summary.extensionTypes, 23)
		case *tls.RenegotiationInfoExtension:
			summary.extensionTypes = append(summary.extensionTypes, 65281)
		case *tls.SupportedCurvesExtension:
			summary.extensionTypes = append(summary.extensionTypes, 10)
			for _, curve := range ext.Curves {
				summary.supportedGroups = append(summary.supportedGroups, uint16(curve))
			}
		case *tls.SupportedPointsExtension:
			summary.extensionTypes = append(summary.extensionTypes, 11)
			summary.pointFormats = append(summary.pointFormats, ext.SupportedPoints...)
		case *tls.SessionTicketExtension:
			summary.extensionTypes = append(summary.extensionTypes, 35)
		case *tls.ALPNExtension:
			summary.extensionTypes = append(summary.extensionTypes, 16)
			summary.alpn = append(summary.alpn, ext.AlpnProtocols...)
		case *tls.StatusRequestExtension:
			summary.extensionTypes = append(summary.extensionTypes, 5)
		case *tls.SignatureAlgorithmsExtension:
			summary.extensionTypes = append(summary.extensionTypes, 13)
			for _, algorithm := range ext.SupportedSignatureAlgorithms {
				summary.signatureAlgorithms = append(summary.signatureAlgorithms, uint16(algorithm))
			}
		case *tls.SCTExtension:
			summary.extensionTypes = append(summary.extensionTypes, 18)
		case *tls.KeyShareExtension:
			summary.extensionTypes = append(summary.extensionTypes, 51)
			for _, keyShare := range ext.KeyShares {
				summary.keyShareGroups = append(summary.keyShareGroups, uint16(keyShare.Group))
			}
		case *tls.PSKKeyExchangeModesExtension:
			summary.extensionTypes = append(summary.extensionTypes, 45)
		case *tls.SupportedVersionsExtension:
			summary.extensionTypes = append(summary.extensionTypes, 43)
			summary.supportedVersions = append(summary.supportedVersions, ext.Versions...)
		case *tls.UtlsPaddingExtension:
			summary.extensionTypes = append(summary.extensionTypes, 21)
		default:
			t.Fatalf("unexpected ClientHello extension type %T", extension)
		}
	}
	summary.ja3 = fmt.Sprintf(
		"771,%s,%s,%s,%s",
		joinClaudeCodeUint16(summary.cipherSuites),
		joinClaudeCodeUint16(summary.extensionTypes),
		joinClaudeCodeUint16(summary.supportedGroups),
		joinClaudeCodeUint8(summary.pointFormats),
	)
	digest := md5.Sum([]byte(summary.ja3)) // #nosec G401 -- JA3 requires MD5.
	summary.ja3MD5 = hex.EncodeToString(digest[:])
	return summary
}

func joinClaudeCodeUint16(values []uint16) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.Itoa(int(value))
	}
	return strings.Join(parts, "-")
}

func joinClaudeCodeUint8(values []uint8) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.Itoa(int(value))
	}
	return strings.Join(parts, "-")
}

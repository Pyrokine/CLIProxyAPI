package management

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/gin-gonic/gin"
)

func verifyUpdateGuardHealth(
	ctx context.Context,
	session updateGuardSession,
	token string,
	expectedVersion string,
	runtime updateGuardRuntime,
) error {
	client, err := runtime.newHTTPClient(session)
	if err != nil {
		return err
	}
	consecutive := 0
	for runtime.now().Before(session.Deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, updateGuardHTTPTimeout)
		err = runtime.runCommand(probeCtx, "systemctl", "is-active", "--quiet", session.ServiceUnit)
		if err == nil {
			err = probeUpdatePublicHealth(probeCtx, client, session.HealthBaseURL)
		}
		if err == nil {
			err = probeUpdatePrivateHealth(probeCtx, client, session, token, expectedVersion)
		}
		cancel()
		if err == nil {
			consecutive++
			if consecutive >= updateGuardRequiredSuccesses {
				return nil
			}
		} else {
			consecutive = 0
		}
		if sleepErr := runtime.sleep(ctx, updateGuardProbeInterval); sleepErr != nil {
			return sleepErr
		}
	}
	return fmt.Errorf("update health deadline exceeded")
}

func probeUpdatePublicHealth(ctx context.Context, client *http.Client, baseURL string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxUpdateHealthResponseBytes))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("public health returned status %d", response.StatusCode)
	}
	return nil
}

func probeUpdatePrivateHealth(
	ctx context.Context,
	client *http.Client,
	session updateGuardSession,
	token string,
	expectedVersion string,
) error {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(session.HealthBaseURL, "/")+updateHealthPath,
		nil,
	)
	if err != nil {
		return err
	}
	request.Header.Set(updateHealthTokenHeader, token)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxUpdateHealthResponseBytes))
		return fmt.Errorf("private health returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpdateHealthResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read private health response: %w", err)
	}
	if len(body) > maxUpdateHealthResponseBytes {
		return fmt.Errorf("private health response exceeds maximum size")
	}
	var payload updateHealthResponse
	if err = json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode private health response: %w", err)
	}
	if payload.Status != "ok" || payload.AttemptID != session.AttemptID ||
		!strings.EqualFold(normalizeUpdateHealthVersion(payload.Version), normalizeUpdateHealthVersion(expectedVersion)) {
		return fmt.Errorf("private health identity mismatch")
	}
	if session.SchemaVersion == updateGuardStateSchemaVersion {
		expectedIdentity := session.TargetIdentity
		if expectedVersion == session.PreviousVersion {
			expectedIdentity = session.PreviousIdentity
		}
		if payload.Identity == nil || *payload.Identity != expectedIdentity {
			return fmt.Errorf("private health executable identity mismatch")
		}
	}
	return nil
}

func normalizeUpdateHealthVersion(version string) string {
	version = strings.TrimSpace(version)
	if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') && version[1] >= '0' && version[1] <= '9' {
		return version[1:]
	}
	return version
}

func newUpdateGuardHTTPClient(session updateGuardSession) (*http.Client, error) {
	parsed, err := url.Parse(session.HealthBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse update health URL: %w", err)
	}
	transport := &http.Transport{Proxy: nil}
	if parsed.Scheme == "https" {
		certPEM, errRead := os.ReadFile(session.HealthTLSCertPath)
		if errRead != nil {
			return nil, fmt.Errorf("read update health certificate: %w", errRead)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(certPEM) {
			return nil, fmt.Errorf("parse update health certificate roots")
		}
		serverName, errName := updateHealthCertificateName(certPEM)
		if errName != nil {
			return nil, errName
		}
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: serverName,
		}
	}
	return &http.Client{
		Timeout: updateGuardHTTPTimeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func updateHealthCertificateName(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("decode update health certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse update health certificate: %w", err)
	}
	if len(certificate.DNSNames) > 0 {
		return certificate.DNSNames[0], nil
	}
	if len(certificate.IPAddresses) > 0 {
		return certificate.IPAddresses[0].String(), nil
	}
	if strings.TrimSpace(certificate.Subject.CommonName) != "" {
		return strings.TrimSpace(certificate.Subject.CommonName), nil
	}
	return "", fmt.Errorf("update health certificate has no server identity")
}

// GetUpdateHealth authenticates the update guard on a direct loopback connection.
func (h *Handler) GetUpdateHealth(c *gin.Context) {
	session, _, ok := h.authenticateUpdateGuard(c)
	if !ok {
		return
	}
	version := buildinfo.Version
	expectedVersion := session.TargetVersion
	switch session.Phase {
	case updatePhaseRollingBack, updatePhaseRollbackRestarting, updatePhaseRollbackHealthCheck:
		expectedVersion = session.PreviousVersion
	}
	if strings.EqualFold(normalizeUpdateHealthVersion(version), normalizeUpdateHealthVersion(expectedVersion)) {
		version = expectedVersion
	}
	response := updateHealthResponse{Status: "ok", Version: version, AttemptID: session.AttemptID}
	if session.SchemaVersion == updateGuardStateSchemaVersion {
		if buildinfo.UpdateProtocol != fmt.Sprint(updateGuardStateSchemaVersion) {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		executable, err := currentUpdateExecutablePath()
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		identity, err := readUpdateArtifact(executable, buildinfo.Version, buildinfo.Commit)
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		response.Identity = &identity
	}
	c.JSON(http.StatusOK, response)
}

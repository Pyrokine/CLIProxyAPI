package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestInspectUpdateArtifactReadsBuildIdentityWithoutExecutingCandidate(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "candidate.go")
	code := "package main\nimport \"os\"\nvar version, commit string\nfunc main() { _ = os.WriteFile(\"must-not-execute\", []byte(version+commit), 0600) }\n"
	if err := os.WriteFile(source, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{strings.Repeat("a", 40), "123abcd"} {
		binary := filepath.Join(dir, "candidate-"+commit)
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		flags := "-X main.version=v1.0.1 -X main.commit=" + commit +
			" -X github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo.UpdateProtocol=2"
		command := exec.Command("go", "build", "-o", binary, "-ldflags", flags, source)
		command.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build synthetic candidate: %v: %s", err, output)
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := inspectUpdateArtifact(data, "", "")
		if len(commit) != 40 {
			if err == nil {
				t.Fatal("short commit was accepted as a release identity")
			}
			continue
		}
		if err != nil || identity.Tag != "v1.0.1" || identity.Commit != commit || identity.Legacy ||
			identity.OS != runtime.GOOS || identity.Arch != runtime.GOARCH || identity.SHA256 != sha256Hex(data) {
			t.Fatalf("candidate identity = %#v, %v", identity, err)
		}
		if err := validateUpdateArtifact(identity, "v1.0.2", false); err == nil {
			t.Fatal("another requested tag accepted the candidate")
		}
		if err := verifyUpdateArtifactFile(binary, identity); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, []byte("changed"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := verifyUpdateArtifactFile(binary, identity); err == nil {
			t.Fatal("changed candidate retained its verified identity")
		}
	}
	if _, err := inspectUpdateArtifact([]byte("not an executable"), "", ""); err == nil {
		t.Fatal("non-executable release asset accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "must-not-execute")); !os.IsNotExist(err) {
		t.Fatal("candidate executed while inspecting it")
	}
}

func TestUpdateArtifactRejectsOldProtocolBeforeInstallation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "previous.go")
	code := "package main\nimport \"os\"\nvar version, commit string\nfunc main() { _ = os.WriteFile(\"must-not-execute\", []byte(version+commit), 0600) }\n"
	if err := os.WriteFile(source, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"", "1"} {
		binary := filepath.Join(dir, "previous-"+protocol)
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		flags := "-X main.version=v1.0.0 -X main.commit=" + strings.Repeat("a", 40)
		if protocol != "" {
			flags += " -X github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo.UpdateProtocol=" + protocol
		}
		command := exec.Command("go", "build", "-o", binary, "-ldflags", flags, source)
		command.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build old-protocol fixture: %v: %s", err, output)
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inspectUpdateArtifact(data, "", ""); err == nil {
			t.Fatalf("old update protocol %q was accepted as an install candidate", protocol)
		}
		previous, err := inspectUpdateArtifact(data, "v1.0.0", strings.Repeat("a", 40))
		if err != nil || validateUpdateArtifact(previous, "v1.0.0", true) != nil {
			t.Fatalf("old-protocol rollback identity was rejected: %#v, %v", previous, err)
		}
		if err := validateUpdateArtifact(previous, "v1.0.0", false); err == nil {
			t.Fatal("old-protocol rollback identity was reused as an install candidate")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "must-not-execute")); !os.IsNotExist(err) {
		t.Fatal("old-protocol candidate was executed during validation")
	}
}

func TestUpdatePrivateHealthRequiresAllArtifactIdentityFields(t *testing.T) {
	identity := *updateGuardTestIdentity("new-binary", "v1.0.1")
	session := updateGuardSession{
		SchemaVersion: updateGuardStateSchemaVersion, AttemptID: "0123456789abcdef0123456789abcdef",
		HealthBaseURL: "http://127.0.0.1:8317", TargetVersion: identity.Tag, TargetIdentity: identity,
	}
	for _, field := range []string{"match", "tag", "commit", "sha256", "os", "arch", "protocol", "missing"} {
		t.Run(field, func(t *testing.T) {
			actual := identity
			switch field {
			case "tag":
				actual.Tag = "v1.0.0"
			case "commit":
				actual.Commit = strings.Repeat("b", 40)
			case "sha256":
				actual.SHA256 = strings.Repeat("0", 64)
			case "os":
				actual.OS = "other"
			case "arch":
				actual.Arch = "other"
			case "protocol":
				actual.UpdateProtocol = "1"
			}
			payload := updateHealthResponse{Status: "ok", Version: session.TargetVersion, AttemptID: session.AttemptID, Identity: &actual}
			if field == "missing" {
				payload.Identity = nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			current := session
			current.HealthBaseURL = server.URL
			err := probeUpdatePrivateHealth(context.Background(), server.Client(), current, "synthetic-token", session.TargetVersion)
			if (err == nil) != (field == "match") {
				t.Fatalf("identity field %s, result: %v", field, err)
			}
		})
	}
}

func TestGetUpdateStatusFiltersAClientAttemptInsteadOfReturningAnOlderSuccess(t *testing.T) {
	store := openUpdateGuardTestStore(t)
	session := newUpdateGuardSession(updateGuardHost{}, "0123456789abcdef0123456789abcdef", "v1.0.1", false, time.Now())
	session.Phase = updatePhaseSucceeded
	if err := store.beginUpdateSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{stateStore: store}
	engine := gin.New()
	engine.GET("/update-status", handler.GetUpdateStatus)
	for _, test := range []struct {
		attempt string
		status  int
	}{
		{session.AttemptID, http.StatusOK},
		{strings.Repeat("b", 32), http.StatusNotFound},
		{"not-an-attempt", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/update-status?attempt_id="+test.attempt, nil))
		if response.Code != test.status {
			t.Fatalf("attempt %s status = %d, %s", test.attempt, response.Code, response.Body.String())
		}
		if test.status != http.StatusOK && strings.Contains(response.Body.String(), `"status":"done"`) {
			t.Fatal("a previous success was attributed to another update")
		}
	}
	if err := store.beginUpdateSession(context.Background(), session); err == nil {
		t.Fatal("completed attempt ID was reused for a second operation")
	}
}

package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	log "github.com/sirupsen/logrus"
)

const (
	defaultUpdateServiceUnit     = "cli-proxy-api.service"
	updateGuardUnitPrefix        = "cli-proxy-api-update-guard-"
	updateGuardCommandFlag       = "--update-guard"
	updateGuardStateDBFlag       = "--update-state-db"
	updateGuardAttemptFlag       = "--update-attempt"
	updateHealthTokenHeader      = "X-CPA-Update-Token"
	updateHealthPath             = "/v0/management/update-health"
	updateDrainPath              = "/v0/management/update-drain"
	updateGuardDrainTimeout      = 2 * time.Minute
	updateGuardRuntimeLimit      = 5 * time.Minute
	updateGuardReconcileInterval = 5 * time.Second
	updateGuardDownloadTimeout   = 7 * time.Minute
	updateGuardHealthTimeout     = 60 * time.Second
	updateGuardClaimTimeout      = 10 * time.Second
	updateGuardClaimPollInterval = 100 * time.Millisecond
	updateGuardProbeInterval     = 2 * time.Second
	updateGuardHTTPTimeout       = 1500 * time.Millisecond
	updateGuardRequiredSuccesses = 3
	updateFailureBaseDelay       = time.Hour
	updateFailureMaxDelay        = 24 * time.Hour
	maxUpdateHealthResponseBytes = 4096
)

type updateGuardHost struct {
	ExecutablePath    string
	StateDBPath       string
	ServiceUnit       string
	HealthBaseURL     string
	HealthTLSCertPath string
}

type updateGuardRuntime struct {
	now               func() time.Time
	sleep             func(context.Context, time.Duration) error
	runCommand        func(context.Context, string, ...string) error
	currentExecutable func() (string, error)
	identify          func(string, string, string) (updateArtifactIdentity, error)
	install           func(updateGuardSession) error
	restore           func(updateGuardSession) error
	newHTTPClient     func(updateGuardSession) (*http.Client, error)
	quiescent         func(context.Context, updateGuardSession) (bool, error)
	drain             func(context.Context, updateGuardSession, string, string) (executionregistry.DrainConfirmation, error)
}

type updateHealthResponse struct {
	Status    string                  `json:"status"`
	Version   string                  `json:"version"`
	AttemptID string                  `json:"attempt_id"`
	Identity  *updateArtifactIdentity `json:"identity,omitempty"`
}

func realUpdateGuardRuntime() updateGuardRuntime {
	return updateGuardRuntime{
		now: time.Now,
		sleep: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		runCommand: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
		currentExecutable: currentUpdateExecutablePath,
		identify:          readUpdateArtifact,
		install:           installUpdateCandidate,
		restore:           restoreUpdateBackup,
		newHTTPClient:     newUpdateGuardHTTPClient,
		quiescent:         updateGuardQuiescent,
		drain:             requestUpdateDrain,
	}
}

func updateGuardQuiescent(ctx context.Context, session updateGuardSession) (bool, error) {
	if session.GuardUnit != updateGuardUnit(session.AttemptID) || session.ServiceUnit != defaultUpdateServiceUnit {
		return false, fmt.Errorf("invalid update service identity")
	}
	for index, unit := range []string{session.GuardUnit, session.ServiceUnit} {
		output, err := exec.CommandContext(ctx, "systemctl", "show", "--no-pager",
			"--property=LoadState", "--property=ActiveState", "--property=Job", unit).Output()
		if err != nil {
			return false, fmt.Errorf("inspect update service state: %w", err)
		}
		ready, err := updateUnitQuiescent(string(output), index == 0)
		if err != nil || !ready {
			return false, err
		}
	}
	return true, nil
}

func updateUnitQuiescent(output string, guard bool) (bool, error) {
	properties := make(map[string]string, 3)
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			properties[name] = strings.TrimSpace(value)
		}
	}
	job, hasJob := properties["Job"]
	if !hasJob || properties["LoadState"] == "" || properties["ActiveState"] == "" {
		return false, fmt.Errorf("incomplete update service state")
	}
	if job != "" {
		return false, nil
	}
	if guard {
		return properties["ActiveState"] == "inactive" || properties["ActiveState"] == "failed", nil
	}
	return properties["LoadState"] == "loaded" && properties["ActiveState"] == "active", nil
}

func generateUpdateAttemptID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate update attempt id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func generateUpdateHealthToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate update health token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func isUpdateAttemptID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func isUpdateHealthToken(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func updatePhaseMayHaveInstalledCandidate(phase string) bool {
	switch phase {
	case updatePhaseInstalling,
		updatePhaseRestarting,
		updatePhaseHealthCheck,
		updatePhaseRollingBack,
		updatePhaseRollbackRestarting,
		updatePhaseRollbackHealthCheck:
		return true
	default:
		return false
	}
}

func updateGuardUnit(attemptID string) string {
	if !isUpdateAttemptID(attemptID) {
		return ""
	}
	return updateGuardUnitPrefix + attemptID + ".service"
}

func updateCandidatePath(executablePath, attemptID string) string {
	if executablePath == "" || !isUpdateAttemptID(attemptID) {
		return ""
	}
	return executablePath + ".update-" + attemptID + ".candidate"
}

func currentUpdateExecutablePath() (string, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	executablePath, err = filepath.EvalSymlinks(executablePath)
	if err != nil {
		return "", fmt.Errorf("resolve executable symlinks: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		return "", fmt.Errorf("resolve absolute executable path: %w", err)
	}
	return executablePath, nil
}

func (h *Handler) resolveUpdateGuardHost(cfg *config.Config) (updateGuardHost, error) {
	if h == nil || h.stateStore == nil || h.stateStore.path == "" {
		return updateGuardHost{}, fmt.Errorf("update guard state persistence is unavailable")
	}
	if os.Getenv("INVOCATION_ID") == "" {
		return updateGuardHost{}, fmt.Errorf("update guard requires a systemd service")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return updateGuardHost{}, fmt.Errorf("systemd-run is unavailable: %w", err)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return updateGuardHost{}, fmt.Errorf("systemctl is unavailable: %w", err)
	}
	executablePath, err := currentUpdateExecutablePath()
	if err != nil {
		return updateGuardHost{}, err
	}
	if err = requireRegularFile(executablePath); err != nil {
		return updateGuardHost{}, fmt.Errorf("validate executable: %w", err)
	}
	if cfg == nil || cfg.Port <= 0 || cfg.Port > 65535 {
		return updateGuardHost{}, fmt.Errorf("update guard requires a valid service port")
	}
	scheme := "http"
	tlsCertPath := ""
	if cfg.TLS.Enable {
		scheme = "https"
		tlsCertPath = strings.TrimSpace(cfg.TLS.Cert)
		if tlsCertPath == "" {
			return updateGuardHost{}, fmt.Errorf("update guard requires the configured TLS certificate")
		}
		tlsCertPath, err = filepath.Abs(tlsCertPath)
		if err != nil {
			return updateGuardHost{}, fmt.Errorf("resolve TLS certificate path: %w", err)
		}
		tlsCertPath, err = filepath.EvalSymlinks(tlsCertPath)
		if err != nil {
			return updateGuardHost{}, fmt.Errorf("resolve TLS certificate symlinks: %w", err)
		}
		if err = requireRegularFile(tlsCertPath); err != nil {
			return updateGuardHost{}, fmt.Errorf("validate TLS certificate: %w", err)
		}
	}
	stateDBPath, err := filepath.Abs(h.stateStore.path)
	if err != nil {
		return updateGuardHost{}, fmt.Errorf("resolve management state path: %w", err)
	}
	return updateGuardHost{
		ExecutablePath:    executablePath,
		StateDBPath:       stateDBPath,
		ServiceUnit:       defaultUpdateServiceUnit,
		HealthBaseURL:     scheme + "://127.0.0.1:" + strconv.Itoa(cfg.Port),
		HealthTLSCertPath: tlsCertPath,
	}, nil
}

func newUpdateGuardSession(
	host updateGuardHost,
	attemptID string,
	targetVersion string,
	automatic bool,
	now time.Time,
) updateGuardSession {
	return updateGuardSession{
		SchemaVersion:     updateGuardStateSchemaVersion,
		AttemptID:         attemptID,
		TargetVersion:     strings.TrimSpace(targetVersion),
		PreviousVersion:   strings.TrimSpace(buildinfo.Version),
		Automatic:         automatic,
		Phase:             updatePhaseDownloading,
		Percent:           0,
		ExecutablePath:    host.ExecutablePath,
		BackupPath:        updateBackupPath(host.ExecutablePath, attemptID),
		StateDBPath:       host.StateDBPath,
		ServiceUnit:       host.ServiceUnit,
		GuardUnit:         updateGuardUnit(attemptID),
		HealthBaseURL:     host.HealthBaseURL,
		HealthTLSCertPath: host.HealthTLSCertPath,
		StartedAt:         now.UTC(),
		UpdatedAt:         now.UTC(),
		Deadline:          now.Add(updateGuardDownloadTimeout).UTC(),
		DeadlineUnix:      now.Add(updateGuardDownloadTimeout).Unix(),
	}
}

func (h *Handler) beginPersistentUpdate(
	ctx context.Context,
	cfg *config.Config,
	targetVersion string,
	automatic bool,
	attemptID string,
) (updateGuardSession, error) {
	host, err := h.resolveUpdateGuardHost(cfg)
	if err != nil {
		return updateGuardSession{}, err
	}
	runtime := realUpdateGuardRuntime()
	if err = h.reconcileStaleUpdateSession(ctx, runtime); err != nil {
		return updateGuardSession{}, err
	}
	if attemptID == "" {
		attemptID, err = generateUpdateAttemptID()
		if err != nil {
			return updateGuardSession{}, err
		}
	}
	now := runtime.now()
	session := newUpdateGuardSession(host, attemptID, targetVersion, automatic, now)
	session.OriginInstance = h.inferenceAdmission.InstanceID()
	if session.OriginInstance == "" {
		return updateGuardSession{}, fmt.Errorf("inference admission is unavailable")
	}
	session.PreviousIdentity, err = readUpdateArtifact(host.ExecutablePath, buildinfo.Version, buildinfo.Commit)
	if err != nil {
		return updateGuardSession{}, err
	}
	if err = validateUpdateArtifact(session.PreviousIdentity, session.PreviousVersion, true); err != nil {
		return updateGuardSession{}, err
	}
	if err = validateUpdateGuardSession(session, host.StateDBPath, host.ExecutablePath, false, now); err != nil {
		return updateGuardSession{}, err
	}
	if err = h.stateStore.beginUpdateSession(ctx, session); err != nil {
		return updateGuardSession{}, err
	}
	return session, nil
}

// RunUpdateGuardCommand runs the internal update guard mode before normal service initialization.
func RunUpdateGuardCommand(args []string) (bool, int) {
	stateDBPath, attemptID, handled, err := parseUpdateGuardCommand(args)
	if !handled {
		return false, 0
	}
	if err != nil {
		log.WithError(err).Error("update guard arguments rejected")
		return true, 1
	}
	if err = requireRegularFile(stateDBPath); err != nil {
		log.WithError(err).Error("update guard state database rejected")
		return true, 1
	}
	store, err := openManagementStateStorePath(stateDBPath)
	if err != nil {
		log.WithError(err).Error("update guard state unavailable")
		return true, 1
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), updateGuardRuntimeLimit)
	defer cancel()
	if err = runUpdateGuard(ctx, store, attemptID, realUpdateGuardRuntime()); err != nil {
		log.WithError(err).Error("update guard failed")
		return true, 1
	}
	return true, 0
}

func parseUpdateGuardCommand(args []string) (string, string, bool, error) {
	if len(args) == 0 || args[0] != updateGuardCommandFlag {
		if slices.Contains(args, updateGuardCommandFlag) {
			return "", "", true, fmt.Errorf("update guard flag must be first")
		}
		return "", "", false, nil
	}
	handled := false
	values := make(map[string]string, 2)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == updateGuardCommandFlag {
			if handled {
				return "", "", true, fmt.Errorf("duplicate update guard argument")
			}
			handled = true
			continue
		}
		matched := false
		for _, name := range []string{updateGuardStateDBFlag, updateGuardAttemptFlag} {
			if argument == name {
				if _, exists := values[name]; exists {
					return "", "", handled, fmt.Errorf("duplicate update guard argument")
				}
				if index+1 >= len(args) {
					return "", "", handled, fmt.Errorf("%s requires a value", name)
				}
				index++
				values[name] = args[index]
				matched = true
				break
			}
			if strings.HasPrefix(argument, name+"=") {
				if _, exists := values[name]; exists {
					return "", "", handled, fmt.Errorf("duplicate update guard argument")
				}
				values[name] = strings.TrimPrefix(argument, name+"=")
				matched = true
				break
			}
		}
		if !matched && handled {
			return "", "", true, fmt.Errorf("unexpected update guard argument")
		}
	}
	if !handled {
		return "", "", false, nil
	}
	stateDBPath := strings.TrimSpace(values[updateGuardStateDBFlag])
	attemptID := strings.ToLower(strings.TrimSpace(values[updateGuardAttemptFlag]))
	if stateDBPath == "" || !filepath.IsAbs(stateDBPath) {
		return "", "", true, fmt.Errorf("update state database path must be absolute")
	}
	if !isUpdateAttemptID(attemptID) {
		return "", "", true, fmt.Errorf("update attempt id is invalid")
	}
	return filepath.Clean(stateDBPath), attemptID, true, nil
}

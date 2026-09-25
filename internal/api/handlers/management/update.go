package management

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

/* ---------- types ---------- */

type updateState struct {
	mu        sync.RWMutex
	status    string // idle | downloading | verifying | replacing | done | error
	message   string
	version   string
	attemptID string
	percent   int
}

type updateRequest struct {
	Version string `json:"version"` // e.g. "v1.3.2"
}

type updateCompatibilityResponse struct {
	CurrentVersion        string                   `json:"current_version"`
	TargetVersion         string                   `json:"target_version"`
	MinPanelVersion       string                   `json:"min_panel_version"`
	RequiresRestart       bool                     `json:"requires_restart"`
	Compatible            bool                     `json:"compatible"`
	AutomaticRetryBlocked bool                     `json:"automatic_retry_blocked"`
	LastFailure           *updateFailureResponse   `json:"last_failure,omitempty"`
	Warnings              []string                 `json:"warnings,omitempty"`
	Usage                 updateUsageCompatibility `json:"usage"`
}

type updateFailureResponse struct {
	FailureCount  int       `json:"failure_count"`
	ResultCode    string    `json:"result_code"`
	LastFailureAt time.Time `json:"last_failure_at"`
	NextRetryAt   time.Time `json:"next_retry_at"`
}

type updateUsageCompatibility struct {
	ConfiguredDataDir string     `json:"configured_data_dir,omitempty"`
	ResolvedDataDir   string     `json:"resolved_data_dir,omitempty"`
	DBPath            string     `json:"db_path,omitempty"`
	DBExists          bool       `json:"db_exists"`
	PersisterReady    bool       `json:"persister_ready"`
	DBSizeBytes       int64      `json:"db_size_bytes"`
	SchemaVersion     string     `json:"schema_version,omitempty"`
	MigratedFrom      string     `json:"migrated_from,omitempty"`
	MigratedAt        *time.Time `json:"migrated_at,omitempty"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubReleaseForUpdate struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

const (
	defaultBackendUpdateCheckInterval = 3 * time.Hour
	minBackendUpdateCheckMinutes      = 30
)

/* ---------- singleton state ---------- */

var (
	selfUpdate         updateState
	cpaAutoUpdaterMu   sync.Mutex
	cpaAutoUpdaterDone context.CancelFunc
)

func init() {
	selfUpdate.status = "idle"
}

/* ---------- handlers ---------- */

// GetUpdateStatus returns the current self-update progress.
func (h *Handler) GetUpdateStatus(c *gin.Context) {
	status := snapshotSelfUpdate()
	stateAvailable := h != nil && h.stateStore != nil
	var session *updateGuardSession
	var stateErr error
	if stateAvailable {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		session, stateErr = h.stateStore.loadUpdateSession(ctx)
		cancel()
	}
	if session != nil && (status.status == "idle" || status.attemptID == session.AttemptID) {
		status = updateStateFromSession(*session)
	}
	response := gin.H{
		"status":          status.status,
		"message":         status.message,
		"target_version":  status.version,
		"attempt_id":      status.attemptID,
		"percent":         status.percent,
		"current_version": buildinfo.Version,
		"state_available": stateAvailable && stateErr == nil,
	}
	if !stateAvailable || stateErr != nil {
		response["state_error"] = "update_state_unavailable"
	}
	if session != nil && (status.attemptID == "" || status.attemptID == session.AttemptID) {
		response["guard_phase"] = session.Phase
		response["result_code"] = session.ResultCode
		response["failure_count"] = session.FailureCount
		response["next_retry_at"] = session.NextRetryAt
	}
	c.JSON(http.StatusOK, response)
}

func snapshotSelfUpdate() updateState {
	selfUpdate.mu.RLock()
	defer selfUpdate.mu.RUnlock()
	return updateState{
		status:    selfUpdate.status,
		message:   selfUpdate.message,
		version:   selfUpdate.version,
		attemptID: selfUpdate.attemptID,
		percent:   selfUpdate.percent,
	}
}

func setSelfUpdate(status, message, version, attemptID string, percent int) {
	selfUpdate.mu.Lock()
	selfUpdate.status = status
	selfUpdate.message = message
	selfUpdate.version = version
	selfUpdate.attemptID = attemptID
	selfUpdate.percent = percent
	selfUpdate.mu.Unlock()
}

func updateStateFromSession(session updateGuardSession) updateState {
	status := "replacing"
	message := "Update guard is applying and validating the new version"
	switch session.Phase {
	case updatePhaseDownloading:
		status = "downloading"
		message = "Downloading release archive"
	case updatePhaseVerifying, updatePhasePreparing:
		status = "verifying"
		message = "Verifying and preparing release"
	case updatePhaseSucceeded:
		status = "done"
		message = "Update completed and passed health checks"
	case updatePhaseRolledBack:
		status = "error"
		message = "Update failed and the previous version was restored"
	case updatePhaseError, updatePhaseLaunchFailed, updatePhaseTerminalFailure:
		status = "error"
		message = "Update failed"
	}
	return updateState{
		status:    status,
		message:   message,
		version:   session.TargetVersion,
		attemptID: session.AttemptID,
		percent:   session.Percent,
	}
}

// GetUpdateCompatibility returns real backend upgrade compatibility signals.
func (h *Handler) GetUpdateCompatibility(c *gin.Context) {
	targetVersion := strings.TrimSpace(c.Query("version"))
	if targetVersion == "" {
		targetVersion = strings.TrimSpace(c.Query("target_version"))
	}
	compat := h.buildUpdateCompatibilityFromConfig(c.Request.Context(), targetVersion, h.configSnapshot())
	c.JSON(http.StatusOK, compat)
}

// PostUpdate triggers a self-update to the specified version.
func (h *Handler) PostUpdate(c *gin.Context) {
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Version) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version is required"})
		return
	}

	targetVersion := strings.TrimSpace(req.Version)
	if !validVersion.MatchString(targetVersion) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_version", "message": "Version format is invalid"})
		return
	}
	setManagementMutationTarget(c, "version", targetVersion)
	cfg := h.configSnapshot()
	if strings.EqualFold(targetVersion, buildinfo.Version) {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "same_version", "message": "Target version is the same as current version"},
		)
		return
	}

	compat := h.buildUpdateCompatibilityFromConfig(c.Request.Context(), targetVersion, cfg)
	if !compat.Compatible {
		c.JSON(
			http.StatusConflict,
			gin.H{
				"error":         "update_incompatible",
				"message":       "Target version requires manual migration review before update",
				"compatibility": compat,
			},
		)
		return
	}

	startCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	session, err := h.beginPersistentUpdate(startCtx, cfg, targetVersion, false)
	cancel()
	if err != nil {
		status := http.StatusServiceUnavailable
		code := "update_guard_unavailable"
		message := "The update guard is unavailable"
		if errors.Is(err, errUpdateInProgress) {
			status = http.StatusConflict
			code = "update_in_progress"
			message = "An update is already in progress"
		}
		c.JSON(status, gin.H{"error": code, "message": message})
		return
	}
	setSelfUpdate("downloading", "", targetVersion, session.AttemptID, 0)

	repo := cpaRepositoryFromConfig(cfg)
	proxyURL := ""
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	go h.performUpdate(repo, proxyURL, session)

	c.JSON(
		http.StatusAccepted,
		gin.H{
			"status": "downloading", "target_version": targetVersion,
			"attempt_id": session.AttemptID, "compatibility": compat,
		},
	)
}

/* ---------- update logic ---------- */

func (h *Handler) performUpdate(repo, proxyURL string, session updateGuardSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	release, err := h.fetchRelease(ctx, repo, session.TargetVersion, proxyURL)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhaseDownloading, "download_failed", 0, err)
		return
	}

	assetNames := expectedArchiveAssetNames(session.TargetVersion)
	asset, checksumAsset := findReleaseAssets(release.Assets, assetNames)
	if asset == nil {
		h.failUpdateBeforeGuard(
			session,
			updatePhaseDownloading,
			"download_failed",
			0,
			fmt.Errorf("release archive is unavailable"),
		)
		return
	}
	if checksumAsset == nil {
		h.failUpdateBeforeGuard(
			session,
			updatePhaseDownloading,
			"checksum_failed",
			0,
			fmt.Errorf("release checksum is unavailable"),
		)
		return
	}

	for label, assetURL := range map[string]string{
		"archive": asset.BrowserDownloadURL, "checksum": checksumAsset.BrowserDownloadURL,
	} {
		if err = validateGitHubDownloadURL(label, assetURL); err != nil {
			h.failUpdateBeforeGuard(session, updatePhaseDownloading, "download_failed", 0, err)
			return
		}
	}

	setSelfUpdate(
		"downloading", "Downloading release archive...", session.TargetVersion, session.AttemptID, 20,
	)
	client := newUpdateHTTPClient(proxyURL)
	archiveData, err := githubGet(ctx, client, asset.BrowserDownloadURL)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhaseDownloading, "download_failed", 20, err)
		return
	}

	now := time.Now().UTC()
	updated, err := h.stateStore.transitionUpdateSession(
		ctx,
		session.AttemptID,
		updatePhaseDownloading,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseVerifying
			next.Percent = 55
			next.UpdatedAt = now
		},
	)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhaseDownloading, "state_failed", 55, err)
		return
	}
	session = *updated
	setSelfUpdate("verifying", "Verifying checksum...", session.TargetVersion, session.AttemptID, 55)
	downloadedHash := sha256Hex(archiveData)
	expectedHash, err := fetchExpectedHash(ctx, client, checksumAsset.BrowserDownloadURL, asset.Name)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhaseVerifying, "checksum_failed", 55, err)
		return
	}
	if expectedHash == "" || !strings.EqualFold(expectedHash, downloadedHash) {
		h.failUpdateBeforeGuard(
			session,
			updatePhaseVerifying,
			"checksum_failed",
			55,
			fmt.Errorf("release checksum mismatch"),
		)
		return
	}

	now = time.Now().UTC()
	updated, err = h.stateStore.transitionUpdateSession(
		ctx,
		session.AttemptID,
		updatePhaseVerifying,
		func(next *updateGuardSession) {
			next.Phase = updatePhasePreparing
			next.Percent = 75
			next.UpdatedAt = now
		},
	)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhaseVerifying, "state_failed", 75, err)
		return
	}
	session = *updated
	setSelfUpdate("verifying", "Extracting release archive...", session.TargetVersion, session.AttemptID, 75)
	binaryData, err := extractBinaryFromArchive(asset.Name, archiveData)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhasePreparing, "extract_failed", 75, err)
		return
	}

	session.CandidatePath = updateCandidatePath(session.ExecutablePath, session.AttemptID)
	now = time.Now().UTC()
	updated, err = h.stateStore.transitionUpdateSession(
		ctx,
		session.AttemptID,
		updatePhasePreparing,
		func(next *updateGuardSession) {
			next.CandidatePath = session.CandidatePath
			next.Percent = 85
			next.UpdatedAt = now
		},
	)
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhasePreparing, "state_failed", 85, err)
		return
	}
	session = *updated
	if err = writeUpdateCandidate(session, binaryData); err != nil {
		h.failUpdateBeforeGuard(session, updatePhasePreparing, "candidate_failed", 85, err)
		return
	}

	session, err = armUpdateGuardSession(ctx, h.stateStore, session, time.Now())
	if err != nil {
		h.failUpdateBeforeGuard(session, updatePhasePreparing, "state_failed", 90, err)
		return
	}
	setSelfUpdate("replacing", "Starting independent update guard...", session.TargetVersion, session.AttemptID, 95)
	launchCtx, launchCancel := context.WithTimeout(context.Background(), updateGuardClaimTimeout+5*time.Second)
	err = launchUpdateGuard(launchCtx, h.stateStore, session, realUpdateGuardRuntime())
	launchCancel()
	if err != nil {
		h.finishLaunchedUpdateFailure(session, err)
		return
	}
	setSelfUpdate(
		"replacing", "Update guard claimed the update and is validating the service",
		session.TargetVersion, session.AttemptID, 95,
	)
	log.WithFields(
		log.Fields{
			"attempt_id": session.AttemptID,
			"version":    session.TargetVersion,
			"sha256":     downloadedHash,
		},
	).Info("self-update handed to independent guard")
}

func (h *Handler) failUpdateBeforeGuard(
	session updateGuardSession,
	fromPhase string,
	resultCode string,
	percent int,
	cause error,
) {
	now := time.Now().UTC()
	updated, transitionErr := h.stateStore.transitionUpdateSession(
		context.Background(),
		session.AttemptID,
		fromPhase,
		func(next *updateGuardSession) {
			next.Phase = updatePhaseError
			next.ResultCode = normalizeUpdateResultCode(resultCode)
			next.Percent = percent
			next.UpdatedAt = now
		},
	)
	if transitionErr == nil {
		session = *updated
		h.attachUpdateFailure(session, updatePhaseError, resultCode, now)
	}
	removeCandidateErr := removeUpdateCandidate(session)
	setSelfUpdate("error", normalizeUpdateResultCode(resultCode), session.TargetVersion, session.AttemptID, percent)
	log.WithFields(
		log.Fields{
			"attempt_id":  session.AttemptID,
			"version":     session.TargetVersion,
			"result_code": normalizeUpdateResultCode(resultCode),
		},
	).WithError(errors.Join(cause, transitionErr, removeCandidateErr)).Error("self-update preparation failed")
}

func (h *Handler) finishLaunchedUpdateFailure(session updateGuardSession, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := h.stateStore.loadUpdateSession(ctx)
	if err == nil && current != nil && current.AttemptID == session.AttemptID {
		session = *current
		if session.Phase == updatePhaseLaunchFailed {
			h.attachUpdateFailure(session, updatePhaseLaunchFailed, session.ResultCode, time.Now())
		}
	}
	setSelfUpdate("error", "update guard failed", session.TargetVersion, session.AttemptID, session.Percent)
	log.WithFields(
		log.Fields{
			"attempt_id": session.AttemptID,
			"version":    session.TargetVersion,
		},
	).WithError(errors.Join(cause, err)).Error("self-update guard launch failed")
}

func (h *Handler) attachUpdateFailure(
	session updateGuardSession,
	phase string,
	resultCode string,
	now time.Time,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failure, err := h.stateStore.recordUpdateFailure(
		ctx,
		session.TargetVersion,
		resultCode,
		now,
		updateFailureBaseDelay,
		updateFailureMaxDelay,
	)
	if err != nil {
		log.WithError(err).Error("failed to persist update retry state")
		return
	}
	_, err = h.stateStore.transitionUpdateSession(
		ctx,
		session.AttemptID,
		phase,
		func(next *updateGuardSession) {
			next.FailureCount = failure.FailureCount
			next.NextRetryAt = &failure.NextRetryAt
			next.UpdatedAt = now.UTC()
		},
	)
	if err != nil {
		log.WithError(err).Error("failed to attach update retry state")
	}
}

var validVersion = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
var validBuildVersion = regexp.MustCompile(`^[a-zA-Z0-9._-]+(?:\+[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*)?$`)
var comparableUpdateVersion = regexp.MustCompile(
	`(?i)^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-aug\.([0-9]+))?(?:\+[a-z0-9-]+(?:\.[a-z0-9-]+)*)?$`,
)

type parsedUpdateVersion struct {
	major        uint64
	minor        uint64
	patch        uint64
	augmented    bool
	augmentation uint64
}

func compareUpdateVersions(left, right string) (int, bool) {
	leftVersion, okLeft := parseUpdateVersion(left)
	rightVersion, okRight := parseUpdateVersion(right)
	if !okLeft || !okRight {
		return 0, false
	}
	leftParts := [...]uint64{leftVersion.major, leftVersion.minor, leftVersion.patch}
	rightParts := [...]uint64{rightVersion.major, rightVersion.minor, rightVersion.patch}
	for i := range leftParts {
		switch {
		case leftParts[i] < rightParts[i]:
			return -1, true
		case leftParts[i] > rightParts[i]:
			return 1, true
		}
	}
	if leftVersion.augmented != rightVersion.augmented {
		if leftVersion.augmented {
			return 1, true
		}
		return -1, true
	}
	switch {
	case leftVersion.augmentation < rightVersion.augmentation:
		return -1, true
	case leftVersion.augmentation > rightVersion.augmentation:
		return 1, true
	default:
		return 0, true
	}
}

func parseUpdateVersion(raw string) (parsedUpdateVersion, bool) {
	matches := comparableUpdateVersion.FindStringSubmatch(strings.TrimSpace(raw))
	if len(matches) != 5 {
		return parsedUpdateVersion{}, false
	}
	var parts [4]uint64
	for i := range parts {
		if matches[i+1] == "" {
			continue
		}
		part, err := strconv.ParseUint(matches[i+1], 10, 64)
		if err != nil {
			return parsedUpdateVersion{}, false
		}
		parts[i] = part
	}
	return parsedUpdateVersion{
		major:        parts[0],
		minor:        parts[1],
		patch:        parts[2],
		augmented:    matches[4] != "",
		augmentation: parts[3],
	}, true
}

func (h *Handler) fetchRelease(
	ctx context.Context,
	repo string,
	version string,
	proxyURL string,
) (*githubReleaseForUpdate, error) {
	if !validVersion.MatchString(version) {
		return nil, fmt.Errorf("invalid version format: %q", version)
	}
	releaseURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, version)
	client := newUpdateHTTPClient(proxyURL)
	body, err := githubGet(ctx, client, releaseURL)
	if err != nil {
		return nil, err
	}
	var release githubReleaseForUpdate
	if err = json.Unmarshal(body, &release); err != nil {
		return nil, err
	}
	return &release, nil
}

func (h *Handler) StartCPAAutoUpdater(ctx context.Context) {
	if h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cpaAutoUpdaterMu.Lock()
	if cpaAutoUpdaterDone != nil {
		cpaAutoUpdaterDone()
	}
	runCtx, cancel := context.WithCancel(ctx)
	cpaAutoUpdaterDone = cancel
	cpaAutoUpdaterMu.Unlock()

	go h.runCPAAutoUpdater(runCtx)
}

func (h *Handler) StopCPAAutoUpdater() {
	cpaAutoUpdaterMu.Lock()
	cancel := cpaAutoUpdaterDone
	cpaAutoUpdaterDone = nil
	cpaAutoUpdaterMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Handler) runCPAAutoUpdater(ctx context.Context) {
	if h == nil {
		return
	}
	interval := defaultBackendUpdateCheckInterval
	var ticker *time.Ticker
	var tick <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	for {
		cfg := h.configSnapshot()
		if cfg != nil && cfg.RemoteManagement.CheckInterval >= minBackendUpdateCheckMinutes {
			interval = time.Duration(cfg.RemoteManagement.CheckInterval) * time.Minute
		}
		if ticker == nil {
			ticker = time.NewTicker(interval)
			tick = ticker.C
		}

		h.tryAutoUpdateCPA(ctx, cfg)

		select {
		case <-ctx.Done():
			return
		case <-tick:
			cfg = h.configSnapshot()
			if cfg != nil {
				next := defaultBackendUpdateCheckInterval
				if cfg.RemoteManagement.CheckInterval >= minBackendUpdateCheckMinutes {
					next = time.Duration(cfg.RemoteManagement.CheckInterval) * time.Minute
				}
				if next != interval {
					interval = next
					ticker.Stop()
					ticker = time.NewTicker(interval)
					tick = ticker.C
				}
			}
		}
	}
}

func (h *Handler) tryAutoUpdateCPA(ctx context.Context, cfg *config.Config) {
	if h == nil || h.stateStore == nil || cfg == nil {
		return
	}
	if !cfg.RemoteManagement.AutoCheckUpdate || !cfg.RemoteManagement.AutoUpdateCPA {
		return
	}
	current := snapshotSelfUpdate()
	if current.status == "downloading" || current.status == "verifying" || current.status == "replacing" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	repo := cpaRepositoryFromConfig(cfg)
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	latest, err := h.fetchLatestReleaseVersion(ctx, repo, proxyURL)
	if err != nil {
		log.Warnf("cpa auto-update: fetch latest release failed: %v", err)
		return
	}
	if latest == "" || strings.EqualFold(strings.TrimSpace(latest), buildinfo.Version) {
		return
	}
	compat := h.buildUpdateCompatibilityFromConfig(ctx, latest, cfg)
	if !compat.Compatible {
		log.Warnf("cpa auto-update: blocked by compatibility checks for %s: %v", latest, compat.Warnings)
		return
	}

	failure, err := h.stateStore.loadUpdateFailure(ctx, latest)
	if err != nil {
		log.Warnf("cpa auto-update: read retry state for %s failed: %v", latest, err)
		return
	}
	if failure != nil && time.Now().Before(failure.NextRetryAt) {
		log.WithFields(
			log.Fields{
				"version":       latest,
				"next_retry_at": failure.NextRetryAt,
			},
		).Warn("cpa auto-update: version is in retry backoff")
		return
	}
	session, err := h.beginPersistentUpdate(ctx, cfg, latest, true)
	if err != nil {
		if !errors.Is(err, errUpdateInProgress) {
			log.Warnf("cpa auto-update: update guard unavailable for %s: %v", latest, err)
		}
		return
	}
	setSelfUpdate("downloading", "", latest, session.AttemptID, 0)
	go h.performUpdate(repo, proxyURL, session)
}

func (h *Handler) fetchLatestReleaseVersion(ctx context.Context, repo string, proxyURL string) (string, error) {
	client := newUpdateHTTPClient(proxyURL)
	body, err := githubGet(
		ctx, client, fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo),
	)
	if err != nil {
		return "", err
	}
	var info releaseInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		return "", fmt.Errorf("missing latest release version")
	}
	return version, nil
}

func (h *Handler) buildUpdateCompatibility(ctx context.Context, targetVersion string) updateCompatibilityResponse {
	return h.buildUpdateCompatibilityFromConfig(ctx, targetVersion, h.configSnapshot())
}

func (h *Handler) buildUpdateCompatibilityFromConfig(
	ctx context.Context,
	targetVersion string,
	cfg *config.Config,
) updateCompatibilityResponse {
	compat := updateCompatibilityResponse{
		CurrentVersion:  buildinfo.Version,
		TargetVersion:   strings.TrimSpace(targetVersion),
		MinPanelVersion: minPanelVersion,
		RequiresRestart: true,
		Compatible:      true,
	}
	if order, ok := compareUpdateVersions(compat.TargetVersion, compat.CurrentVersion); ok && order < 0 {
		compat.Compatible = false
		compat.Warnings = append(
			compat.Warnings,
			fmt.Sprintf(
				"target version %s is older than current version %s; automatic downgrade is not supported",
				compat.TargetVersion,
				compat.CurrentVersion,
			),
		)
	}

	if h == nil {
		return compat
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if h.stateStore != nil && validVersion.MatchString(compat.TargetVersion) {
		failure, err := h.stateStore.loadUpdateFailure(ctx, compat.TargetVersion)
		if err != nil {
			compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to read update retry state: %v", err))
		} else if failure != nil {
			compat.LastFailure = &updateFailureResponse{
				FailureCount:  failure.FailureCount,
				ResultCode:    failure.ResultCode,
				LastFailureAt: failure.LastFailureAt,
				NextRetryAt:   failure.NextRetryAt,
			}
			compat.AutomaticRetryBlocked = time.Now().Before(failure.NextRetryAt)
		}
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	compat.Usage.ConfiguredDataDir = strings.TrimSpace(cfg.UsageDataDir)
	compat.Usage.ResolvedDataDir = usage.ResolveDataDir(cfg.UsageDataDir, h.configFilePath)
	if compat.Usage.ResolvedDataDir != "" {
		compat.Usage.DBPath = filepath.Join(compat.Usage.ResolvedDataDir, "events.db")
		if info, err := os.Stat(compat.Usage.DBPath); err == nil && !info.IsDir() {
			compat.Usage.DBExists = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to stat usage db: %v", err))
		}
	}
	if h.usagePersister == nil {
		if compat.Usage.DBExists {
			compat.Compatible = false
			compat.Warnings = append(compat.Warnings, "usage db exists but persister is unavailable in this process")
		}
		return compat
	}

	compat.Usage.PersisterReady = h.usagePersister.IsReady()
	compat.Usage.DBSizeBytes = h.usagePersister.DBSize()
	if !compat.Usage.PersisterReady {
		if compat.Usage.DBExists {
			compat.Compatible = false
			compat.Warnings = append(
				compat.Warnings, "usage persister is not ready; update would hide schema compatibility failures",
			)
		}
		return compat
	}

	schemaVersion, err := h.usagePersister.SchemaVersion(ctx)
	if err != nil {
		compat.Compatible = false
		compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to read usage schema version: %v", err))
	} else {
		compat.Usage.SchemaVersion = schemaVersion
		if schemaVersion != "" && schemaVersion != usage.CurrentSchemaVersion {
			compat.Compatible = false
			compat.Warnings = append(
				compat.Warnings,
				fmt.Sprintf(
					"usage schema_version=%s, expected %s",
					schemaVersion,
					usage.CurrentSchemaVersion,
				),
			)
		}
	}

	migrationStatus, err := h.usagePersister.MigrationStatus(ctx)
	if err != nil {
		compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to read usage migration marker: %v", err))
	} else {
		compat.Usage.MigratedFrom = migrationStatus.From
		if !migrationStatus.At.IsZero() {
			compat.Usage.MigratedAt = new(time.Time)
			*compat.Usage.MigratedAt = migrationStatus.At
		}
	}

	return compat
}

/* ---------- helpers ---------- */

func expectedArchiveAssetNames(version string) []string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	format := "tar.gz"
	if goos == "windows" {
		format = "zip"
	}
	baseName := fmt.Sprintf("CLIProxyAPI_%s_%s.%s", goos, goarch, format)
	normalizedVersion := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if normalizedVersion == "" {
		return []string{baseName}
	}
	return []string{fmt.Sprintf("CLIProxyAPI_%s_%s_%s.%s", normalizedVersion, goos, goarch, format), baseName}
}

func findReleaseAssets(assets []githubReleaseAsset, archiveNames []string) (*githubReleaseAsset, *githubReleaseAsset) {
	var archiveAsset *githubReleaseAsset
	var checksumAsset *githubReleaseAsset
	for i := range assets {
		asset := &assets[i]
		for _, archiveName := range archiveNames {
			if strings.EqualFold(asset.Name, archiveName) {
				archiveAsset = asset
				break
			}
		}
		if strings.EqualFold(asset.Name, "checksums.txt") {
			checksumAsset = asset
		}
	}
	return archiveAsset, checksumAsset
}

func validateGitHubDownloadURL(label, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return fmt.Errorf("%s URL is not HTTPS", label)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") && !strings.HasSuffix(
		host, ".githubusercontent.com",
	) {
		return fmt.Errorf("%s URL host %q is not a GitHub domain", label, host)
	}
	return nil
}

func extractBinaryFromArchive(assetName string, archiveData []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(strings.ToLower(assetName), ".tar.gz"):
		return extractBinaryFromTarGz(archiveData)
	case strings.HasSuffix(strings.ToLower(assetName), ".zip"):
		return extractBinaryFromZip(archiveData)
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", assetName)
	}
}

func extractBinaryFromTarGz(archiveData []byte) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return nil, fmt.Errorf("open tar.gz: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		if !isArchiveBinaryPath(header.Name) {
			continue
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, fmt.Errorf("read binary entry: %w", err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("binary entry not found in tar.gz archive")
}

func extractBinaryFromZip(archiveData []byte) ([]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, file := range zipReader.File {
		if file == nil || file.FileInfo().IsDir() || !isArchiveBinaryPath(file.Name) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry: %w", err)
		}
		data, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read zip entry: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close zip entry: %w", closeErr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("binary entry not found in zip archive")
}

func isArchiveBinaryPath(name string) bool {
	base := strings.ToLower(path.Base(strings.TrimSpace(name)))
	return base == "cli-proxy-api" || base == "cli-proxy-api.exe"
}

func newUpdateHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-HTTPS URL blocked")
			}
			host := strings.ToLower(req.URL.Hostname())
			if host != "github.com" && !strings.HasSuffix(host, ".github.com") &&
				!strings.HasSuffix(host, ".githubusercontent.com") {
				return fmt.Errorf("redirect to non-GitHub domain %q blocked", host)
			}
			return nil
		},
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}
	return client
}

func fetchExpectedHash(ctx context.Context, client *http.Client, checksumURL, assetName string) (string, error) {
	body, err := githubGet(ctx, client, checksumURL)
	if err != nil {
		return "", err
	}

	// Parse checksum file: each line is "hash  filename" or "hash filename"
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && strings.EqualFold(parts[1], assetName) {
			return strings.ToLower(parts[0]), nil
		}
	}

	// If the file contains only a single hash (no filename)
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 64 && !strings.ContainsAny(trimmed, " \t\n") {
		return strings.ToLower(trimmed), nil
	}

	return "", nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PostPanelUpdate triggers a panel HTML asset refresh. An optional `version`
// field in the request body pins the update to a specific release tag; when
// empty the latest release is used.
func (h *Handler) PostPanelUpdate(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg != nil && cfg.RemoteManagement.DisableControlPanel {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "control_panel_disabled", "message": "Control panel is disabled in configuration"},
		)
		return
	}

	staticDir := managementasset.StaticDir(h.configFilePath)
	if staticDir == "" {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "static_dir_unknown", "message": "Cannot resolve static directory"},
		)
		return
	}

	proxyURL := ""
	panelRepo := ""
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
		panelRepo = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	}

	var req struct {
		Version string `json:"version"`
	}
	_ = c.ShouldBindJSON(&req)
	version := strings.TrimSpace(req.Version)

	ok := managementasset.EnsureManagementHTML(c.Request.Context(), staticDir, proxyURL, panelRepo, version)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_failed", "message": "Failed to update management panel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Management panel updated"})
}

package management

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type bannedIPEntry struct {
	IP                  string    `json:"ip"`
	BannedUntil         time.Time `json:"banned_until"`
	BanRemainingSeconds int       `json:"ban_remaining_seconds"`
	BanCount            int       `json:"ban_count"`
}

const (
	maxRateLimitEvents                    = 100
	defaultRateLimitPersistenceTimeout    = 5 * time.Second
	defaultRateLimitPersistenceRetryDelay = time.Second
)

type rateLimitEventType string

const (
	rateLimitEventBanStarted  rateLimitEventType = "ban_started"
	rateLimitEventAutoExpired rateLimitEventType = "automatic_expiry"
	rateLimitEventManualUnban rateLimitEventType = "manual_unban"
)

type rateLimitEventEntry struct {
	Type        rateLimitEventType `json:"type"`
	IP          string             `json:"ip"`
	OccurredAt  time.Time          `json:"occurred_at"`
	BannedUntil time.Time          `json:"banned_until"`
	BanCount    int                `json:"ban_count"`
}

type rateLimitUnbanHistoryEntry struct {
	IP          string    `json:"ip"`
	UnbannedAt  time.Time `json:"unbanned_at"`
	BannedUntil time.Time `json:"banned_until"`
	BanCount    int       `json:"ban_count"`
}

type failedLoginNoticeEntry struct {
	IP                   string    `json:"ip"`
	Count                int       `json:"count"`
	LastFailureAt        time.Time `json:"last_failure_at"`
	LastSuccessfulAuthAt time.Time `json:"last_successful_auth_at"`
}

type rateLimitPersistedBan struct {
	IP           string    `json:"ip"`
	BlockedUntil time.Time `json:"blocked_until"`
	LastActivity time.Time `json:"last_activity"`
	BanCount     int       `json:"ban_count"`
}

type rateLimitPersistedAttempt struct {
	IP                     string      `json:"ip"`
	Failures               []time.Time `json:"failures,omitempty"`
	BlockedUntil           time.Time   `json:"blocked_until,omitempty"`
	LastActivity           time.Time   `json:"last_activity,omitempty"`
	BanCount               int         `json:"ban_count,omitempty"`
	PendingNoticeFailures  int         `json:"pending_notice_failures,omitempty"`
	LastFailureAt          time.Time   `json:"last_failure_at,omitempty"`
	LastSuccessfulAuthAt   time.Time   `json:"last_successful_auth_at,omitempty"`
	LastFailureFingerprint string      `json:"last_failure_fingerprint,omitempty"`
	LastCountedFailureAt   time.Time   `json:"last_counted_failure_at,omitempty"`
}

type rateLimitPersistedState struct {
	Revision   int64                       `json:"revision"`
	Attempts   []rateLimitPersistedAttempt `json:"attempts,omitempty"`
	ActiveBans []rateLimitPersistedBan     `json:"active_bans,omitempty"`
	Events     []rateLimitEventEntry       `json:"events"`
}

func (h *Handler) appendRateLimitEventLocked(
	eventType rateLimitEventType,
	ip string,
	occurredAt time.Time,
	bannedUntil time.Time,
	banCount int,
) {
	h.rateLimitEvents = append(
		h.rateLimitEvents, rateLimitEventEntry{
			Type:        eventType,
			IP:          ip,
			OccurredAt:  occurredAt,
			BannedUntil: bannedUntil,
			BanCount:    banCount,
		},
	)
	if len(h.rateLimitEvents) > maxRateLimitEvents {
		h.rateLimitEvents = h.rateLimitEvents[len(h.rateLimitEvents)-maxRateLimitEvents:]
	}
}

func (h *Handler) expireRateLimitBanLocked(ip string, ai *attemptInfo, now time.Time) bool {
	if ai == nil || ai.blockedUntil.IsZero() || now.Before(ai.blockedUntil) {
		return false
	}
	bannedUntil := ai.blockedUntil
	ai.blockedUntil = time.Time{}
	ai.failures = ai.failures[:0]
	ai.lastActivity = now
	h.appendRateLimitEventLocked(rateLimitEventAutoExpired, ip, now, bannedUntil, ai.banCount)
	return true
}

func (h *Handler) trimRateLimitAttemptsLocked(now time.Time) int {
	excess := len(h.failedAttempts) - maxManagementRateLimitClients
	if excess <= 0 {
		return 0
	}

	type rankedAttempt struct {
		ip             string
		activeBan      bool
		failureCount   int
		pendingNotices bool
		lastActivity   time.Time
	}
	cutoff := now.Add(-managementFailureWindow)
	ranked := make([]rankedAttempt, 0, len(h.failedAttempts))
	for ip, ai := range h.failedAttempts {
		attempt := rankedAttempt{ip: ip}
		if ai != nil {
			if !ai.blockedUntil.IsZero() && !now.Before(ai.blockedUntil) {
				h.expireRateLimitBanLocked(ip, ai, now)
			}
			attempt.activeBan = !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil)
			for _, failureAt := range ai.failures {
				if failureAt.After(cutoff) {
					attempt.failureCount++
				}
			}
			attempt.pendingNotices = ai.pendingNoticeFailures > 0
			attempt.lastActivity = ai.lastActivity
			if ai.lastFailureAt.After(attempt.lastActivity) {
				attempt.lastActivity = ai.lastFailureAt
			}
		}
		ranked = append(ranked, attempt)
	}
	sort.Slice(
		ranked, func(i, j int) bool {
			if ranked[i].activeBan != ranked[j].activeBan {
				return ranked[i].activeBan
			}
			if ranked[i].failureCount != ranked[j].failureCount {
				return ranked[i].failureCount > ranked[j].failureCount
			}
			if ranked[i].pendingNotices != ranked[j].pendingNotices {
				return ranked[i].pendingNotices
			}
			if !ranked[i].lastActivity.Equal(ranked[j].lastActivity) {
				return ranked[i].lastActivity.After(ranked[j].lastActivity)
			}
			return ranked[i].ip < ranked[j].ip
		},
	)
	for _, attempt := range ranked[maxManagementRateLimitClients:] {
		delete(h.failedAttempts, attempt.ip)
	}
	return excess
}

func (h *Handler) rateLimitCapacityBanLocked(now time.Time) (time.Duration, bool) {
	if len(h.failedAttempts) < maxManagementRateLimitClients {
		return 0, false
	}
	var earliestExpiry time.Time
	for _, ai := range h.failedAttempts {
		if ai == nil || ai.blockedUntil.IsZero() || !now.Before(ai.blockedUntil) {
			return 0, false
		}
		if earliestExpiry.IsZero() || ai.blockedUntil.Before(earliestExpiry) {
			earliestExpiry = ai.blockedUntil
		}
	}
	return earliestExpiry.Sub(now), true
}

func (h *Handler) rateLimitCapacityWarningDueLocked(now time.Time) bool {
	if !h.rateLimitCapacityWarningAt.IsZero() &&
		!now.Before(h.rateLimitCapacityWarningAt) &&
		now.Sub(h.rateLimitCapacityWarningAt) < managementRateLimitCapacityWarningInterval {
		return false
	}
	h.rateLimitCapacityWarningAt = now
	return true
}

func (h *Handler) rateLimitStateSnapshotLocked(now time.Time) rateLimitPersistedState {
	revision := now.UTC().UnixNano()
	if revision <= h.rateLimitRevision {
		revision = h.rateLimitRevision + 1
	}
	if revision <= 0 {
		revision = 1
	}
	h.rateLimitRevision = revision

	state := rateLimitPersistedState{
		Revision: revision,
		Attempts: make([]rateLimitPersistedAttempt, 0),
		Events:   append([]rateLimitEventEntry(nil), h.rateLimitEvents...),
	}
	cutoff := now.Add(-managementFailureWindow)
	for ip, ai := range h.failedAttempts {
		if ai == nil {
			continue
		}
		failures := make([]time.Time, 0, len(ai.failures))
		for _, failureAt := range ai.failures {
			if failureAt.After(cutoff) {
				failures = append(failures, failureAt)
			}
		}
		activeBan := !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil)
		if !activeBan && len(failures) == 0 && ai.pendingNoticeFailures <= 0 {
			continue
		}
		state.Attempts = append(
			state.Attempts, rateLimitPersistedAttempt{
				IP:                     ip,
				Failures:               failures,
				BlockedUntil:           ai.blockedUntil,
				LastActivity:           ai.lastActivity,
				BanCount:               ai.banCount,
				PendingNoticeFailures:  ai.pendingNoticeFailures,
				LastFailureAt:          ai.lastFailureAt,
				LastSuccessfulAuthAt:   ai.lastSuccessfulAuthAt,
				LastFailureFingerprint: ai.lastFailureFingerprint,
				LastCountedFailureAt:   ai.lastCountedFailureAt,
			},
		)
	}
	sort.Slice(state.Attempts, func(i, j int) bool { return state.Attempts[i].IP < state.Attempts[j].IP })
	return state
}

func (h *Handler) persistRateLimitState() {
	if h == nil || h.stateStore == nil {
		return
	}
	h.rateLimitPersistenceMu.Lock()
	h.rateLimitPersistenceDirty = true
	h.rateLimitPersistenceMu.Unlock()
	if h.rateLimitPersistenceWake != nil {
		select {
		case h.rateLimitPersistenceWake <- struct{}{}:
		default:
		}
	}
}

func (h *Handler) startRateLimitPersistence() {
	if h == nil || h.stateStore == nil || h.rateLimitPersistenceWake == nil || h.rateLimitPersistenceStop == nil {
		return
	}
	h.rateLimitPersistenceWG.Add(1)
	go h.runRateLimitPersistence()
}

func (h *Handler) runRateLimitPersistence() {
	defer h.rateLimitPersistenceWG.Done()
	for {
		state, pending := h.pendingRateLimitState()
		if !pending {
			select {
			case <-h.rateLimitPersistenceWake:
				continue
			case <-h.rateLimitPersistenceStop:
				h.rateLimitPersistenceCloseErr = h.flushPendingRateLimitState()
				return
			}
		}

		if err := h.saveRateLimitState(state); err == nil {
			h.clearPendingRateLimitState(state.Revision)
			h.rateLimitPersistenceAvailable.Store(true)
			continue
		} else if h.rateLimitPersistenceAvailable.Swap(false) {
			log.WithError(err).Warn("failed to save management rate limit state; retrying")
		}

		delay := h.rateLimitPersistenceRetryDelay
		if delay <= 0 {
			delay = defaultRateLimitPersistenceRetryDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-h.rateLimitPersistenceWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-h.rateLimitPersistenceStop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			h.rateLimitPersistenceCloseErr = h.flushPendingRateLimitState()
			return
		}
	}
}

func (h *Handler) pendingRateLimitState() (rateLimitPersistedState, bool) {
	h.rateLimitPersistenceMu.Lock()
	dirty := h.rateLimitPersistenceDirty
	if dirty {
		h.rateLimitPersistenceDirty = false
	}
	if !dirty {
		if h.rateLimitPendingState == nil {
			h.rateLimitPersistenceMu.Unlock()
			return rateLimitPersistedState{}, false
		}
		state := *h.rateLimitPendingState
		h.rateLimitPersistenceMu.Unlock()
		return state, true
	}
	h.rateLimitPersistenceMu.Unlock()

	h.attemptsMu.Lock()
	state := h.rateLimitStateSnapshotLocked(time.Now())
	h.attemptsMu.Unlock()

	h.rateLimitPersistenceMu.Lock()
	if h.rateLimitPendingState == nil || state.Revision > h.rateLimitPendingState.Revision {
		pending := state
		h.rateLimitPendingState = &pending
	}
	state = *h.rateLimitPendingState
	h.rateLimitPersistenceMu.Unlock()
	return state, true
}

func (h *Handler) clearPendingRateLimitState(revision int64) {
	h.rateLimitPersistenceMu.Lock()
	if h.rateLimitPendingState != nil && h.rateLimitPendingState.Revision <= revision {
		h.rateLimitPendingState = nil
	}
	h.rateLimitPersistenceMu.Unlock()
}

func (h *Handler) saveRateLimitState(state rateLimitPersistedState) error {
	timeout := h.rateLimitPersistenceTimeout
	if timeout <= 0 {
		timeout = defaultRateLimitPersistenceTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return h.stateStore.saveRateLimitState(ctx, state)
}

func (h *Handler) flushPendingRateLimitState() error {
	for {
		state, pending := h.pendingRateLimitState()
		if !pending {
			return nil
		}
		if err := h.saveRateLimitState(state); err != nil {
			h.rateLimitPersistenceAvailable.Store(false)
			return err
		}
		h.clearPendingRateLimitState(state.Revision)
		h.rateLimitPersistenceAvailable.Store(true)
	}
}

func (h *Handler) stopRateLimitPersistence() error {
	if h == nil || h.rateLimitPersistenceStop == nil {
		return nil
	}
	close(h.rateLimitPersistenceStop)
	h.rateLimitPersistenceWG.Wait()
	return h.rateLimitPersistenceCloseErr
}

func (h *Handler) restoreRateLimitState() {
	if h == nil || h.stateStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultRateLimitPersistenceTimeout)
	defer cancel()
	state, err := h.stateStore.loadRateLimitState(ctx)
	if err != nil {
		h.rateLimitPersistenceAvailable.Store(false)
		log.WithError(err).Warn("failed to load management rate limit state")
		return
	}
	if state == nil {
		return
	}

	now := time.Now()
	cutoff := now.Add(-managementFailureWindow)
	changed := state.Revision <= 0 || len(state.ActiveBans) > 0
	restoredAttempts := make(map[string]struct{}, len(state.Attempts))
	h.attemptsMu.Lock()
	if state.Revision > h.rateLimitRevision {
		h.rateLimitRevision = state.Revision
	}
	h.rateLimitEvents = append(h.rateLimitEvents[:0], state.Events...)
	if len(h.rateLimitEvents) > maxRateLimitEvents {
		h.rateLimitEvents = h.rateLimitEvents[len(h.rateLimitEvents)-maxRateLimitEvents:]
		changed = true
	}
	for _, event := range h.rateLimitEvents {
		if event.IP == "" {
			continue
		}
		ai := h.failedAttempts[event.IP]
		if ai == nil {
			ai = &attemptInfo{}
			h.failedAttempts[event.IP] = ai
		}
		if event.BanCount > ai.banCount {
			ai.banCount = event.BanCount
		}
		if event.OccurredAt.After(ai.lastActivity) {
			ai.lastActivity = event.OccurredAt
		}
	}
	restoreAttempt := func(attempt rateLimitPersistedAttempt) {
		attempt.IP = strings.TrimSpace(attempt.IP)
		if attempt.IP == "" {
			changed = true
			return
		}
		restoredAttempts[attempt.IP] = struct{}{}
		ai := h.failedAttempts[attempt.IP]
		if ai == nil {
			ai = &attemptInfo{}
			h.failedAttempts[attempt.IP] = ai
		}
		ai.failures = ai.failures[:0]
		for _, failureAt := range attempt.Failures {
			if failureAt.After(cutoff) {
				ai.failures = append(ai.failures, failureAt)
			} else {
				changed = true
			}
		}
		ai.blockedUntil = attempt.BlockedUntil
		ai.lastActivity = attempt.LastActivity
		if ai.lastActivity.IsZero() {
			ai.lastActivity = now
			changed = true
		}
		if attempt.BanCount > ai.banCount {
			ai.banCount = attempt.BanCount
		}
		if attempt.PendingNoticeFailures > 0 {
			ai.pendingNoticeFailures = attempt.PendingNoticeFailures
		} else if attempt.PendingNoticeFailures < 0 {
			changed = true
		}
		ai.lastFailureAt = attempt.LastFailureAt
		ai.lastSuccessfulAuthAt = attempt.LastSuccessfulAuthAt
		ai.lastFailureFingerprint = strings.TrimSpace(attempt.LastFailureFingerprint)
		ai.lastCountedFailureAt = attempt.LastCountedFailureAt
		if !ai.lastCountedFailureAt.IsZero() && now.Sub(ai.lastCountedFailureAt) >= managementDuplicateFailureWindow {
			ai.lastFailureFingerprint = ""
			ai.lastCountedFailureAt = time.Time{}
			changed = true
		}
		if !ai.blockedUntil.IsZero() && !now.Before(ai.blockedUntil) {
			changed = h.expireRateLimitBanLocked(attempt.IP, ai, now) || changed
		}
		if ai.blockedUntil.IsZero() && len(ai.failures) == 0 && ai.pendingNoticeFailures == 0 {
			changed = true
		}
	}
	for _, attempt := range state.Attempts {
		restoreAttempt(attempt)
	}
	for _, ban := range state.ActiveBans {
		if _, restored := restoredAttempts[ban.IP]; restored {
			continue
		}
		restoreAttempt(
			rateLimitPersistedAttempt{
				IP:           ban.IP,
				BlockedUntil: ban.BlockedUntil,
				LastActivity: ban.LastActivity,
				BanCount:     ban.BanCount,
			},
		)
	}
	dropped := h.trimRateLimitAttemptsLocked(now)
	warnCapacity := dropped > 0 && h.rateLimitCapacityWarningDueLocked(now)
	changed = dropped > 0 || changed
	h.attemptsMu.Unlock()
	if warnCapacity {
		log.WithFields(
			log.Fields{
				"capacity":  maxManagementRateLimitClients,
				"discarded": dropped,
			},
		).Warn("restored management rate limit state exceeded client capacity; discarded lower-priority state")
	}
	if changed {
		h.persistRateLimitState()
	}
}

// GetRateLimitStatus returns the current management authentication limits and recent events.
func (h *Handler) GetRateLimitStatus(c *gin.Context) {
	now := time.Now()
	changed := false
	h.attemptsMu.Lock()

	banned := make([]bannedIPEntry, 0)
	notices := make([]failedLoginNoticeEntry, 0)
	consumeNotices := c.Query("consume_failed_login_notices") == "true"
	for ip, ai := range h.failedAttempts {
		changed = h.expireRateLimitBanLocked(ip, ai, now) || changed
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			banned = append(
				banned, bannedIPEntry{
					IP:                  ip,
					BannedUntil:         ai.blockedUntil,
					BanRemainingSeconds: managementRetryAfterSeconds(ai.blockedUntil.Sub(now)),
					BanCount:            ai.banCount,
				},
			)
		}
		if ai.pendingNoticeFailures > 0 {
			notices = append(
				notices, failedLoginNoticeEntry{
					IP:                   ip,
					Count:                ai.pendingNoticeFailures,
					LastFailureAt:        ai.lastFailureAt,
					LastSuccessfulAuthAt: ai.lastSuccessfulAuthAt,
				},
			)
			if consumeNotices {
				ai.pendingNoticeFailures = 0
				changed = true
			}
		}
	}
	sort.Slice(banned, func(i, j int) bool { return banned[i].IP < banned[j].IP })
	sort.Slice(notices, func(i, j int) bool { return notices[i].LastFailureAt.After(notices[j].LastFailureAt) })

	events := append([]rateLimitEventEntry(nil), h.rateLimitEvents...)
	sort.Slice(events, func(i, j int) bool { return events[i].OccurredAt.After(events[j].OccurredAt) })
	history := make([]rateLimitUnbanHistoryEntry, 0)
	for _, event := range events {
		if event.Type != rateLimitEventManualUnban {
			continue
		}
		history = append(
			history, rateLimitUnbanHistoryEntry{
				IP:          event.IP,
				UnbannedAt:  event.OccurredAt,
				BannedUntil: event.BannedUntil,
				BanCount:    event.BanCount,
			},
		)
	}
	h.attemptsMu.Unlock()
	if changed {
		h.persistRateLimitState()
	}

	client, _ := managementClientAddressFromContext(c)
	c.JSON(
		http.StatusOK, gin.H{
			"banned_ips":                       banned,
			"failed_login_notices":             notices,
			"unban_history":                    history,
			"rate_limit_events":                events,
			"rate_limit_persistence_available": h.rateLimitPersistenceAvailable.Load(),
			"client_ip":                        client.ip,
			"client_ip_source":                 client.source,
		},
	)
}

// DeleteRateLimitBan removes the ban on a specific IP from the management endpoint rate limiter.
func (h *Handler) DeleteRateLimitBan(c *gin.Context) {
	var body struct {
		IP string `json:"ip"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.IP == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing ip"})
		return
	}

	now := time.Now()
	changed := false
	h.attemptsMu.Lock()
	if ai := h.failedAttempts[body.IP]; ai != nil {
		changed = len(ai.failures) > 0 || ai.lastFailureFingerprint != "" || !ai.lastCountedFailureAt.IsZero()
		if h.expireRateLimitBanLocked(body.IP, ai, now) {
			changed = true
		} else if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			bannedUntil := ai.blockedUntil
			ai.blockedUntil = time.Time{}
			ai.failures = ai.failures[:0]
			ai.lastActivity = now
			h.appendRateLimitEventLocked(rateLimitEventManualUnban, body.IP, now, bannedUntil, ai.banCount)
			changed = true
		}
		ai.failures = ai.failures[:0]
		ai.blockedUntil = time.Time{}
		ai.lastActivity = now
		ai.lastFailureFingerprint = ""
		ai.lastCountedFailureAt = time.Time{}
	}
	h.attemptsMu.Unlock()
	if changed {
		h.persistRateLimitState()
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

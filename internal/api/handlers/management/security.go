package management

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

type bannedIPEntry struct {
	IP                  string    `json:"ip"`
	BannedUntil         time.Time `json:"banned_until"`
	BanRemainingSeconds int       `json:"ban_remaining_seconds"`
	BanCount            int       `json:"ban_count"`
}

const maxRateLimitUnbanHistory = 100

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

// GetRateLimitStatus returns the list of IPs currently banned by the management endpoint rate limiter.
func (h *Handler) GetRateLimitStatus(c *gin.Context) {
	now := time.Now()
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()

	banned := make([]bannedIPEntry, 0)
	notices := make([]failedLoginNoticeEntry, 0)
	consumeNotices := c.Query("consume_failed_login_notices") == "true"
	for ip, ai := range h.failedAttempts {
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			banned = append(
				banned, bannedIPEntry{
					IP:                  ip,
					BannedUntil:         ai.blockedUntil,
					BanRemainingSeconds: int(ai.blockedUntil.Sub(now).Seconds()),
					BanCount:            ai.banCount,
				},
			)
		}
		if ai.pendingNoticeFailures > 0 {
			notices = append(
				notices,
				failedLoginNoticeEntry{
					IP:                   ip,
					Count:                ai.pendingNoticeFailures,
					LastFailureAt:        ai.lastFailureAt,
					LastSuccessfulAuthAt: ai.lastSuccessfulAuthAt,
				},
			)
			if consumeNotices {
				ai.pendingNoticeFailures = 0
			}
		}
	}
	sort.Slice(banned, func(i, j int) bool { return banned[i].IP < banned[j].IP })
	sort.Slice(notices, func(i, j int) bool { return notices[i].LastFailureAt.After(notices[j].LastFailureAt) })

	history := append([]rateLimitUnbanHistoryEntry(nil), h.rateLimitUnbanHistory...)
	sort.Slice(history, func(i, j int) bool { return history[i].UnbannedAt.After(history[j].UnbannedAt) })
	c.JSON(http.StatusOK, gin.H{"banned_ips": banned, "failed_login_notices": notices, "unban_history": history})
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
	h.attemptsMu.Lock()
	if ai := h.failedAttempts[body.IP]; ai != nil {
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			h.rateLimitUnbanHistory = append(
				h.rateLimitUnbanHistory,
				rateLimitUnbanHistoryEntry{
					IP:          body.IP,
					UnbannedAt:  now,
					BannedUntil: ai.blockedUntil,
					BanCount:    ai.banCount,
				},
			)
			if len(h.rateLimitUnbanHistory) > maxRateLimitUnbanHistory {
				h.rateLimitUnbanHistory = h.rateLimitUnbanHistory[len(h.rateLimitUnbanHistory)-maxRateLimitUnbanHistory:]
			}
		}
		ai.failures = ai.failures[:0]
		ai.blockedUntil = time.Time{}
		ai.lastActivity = now
	}
	h.attemptsMu.Unlock()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

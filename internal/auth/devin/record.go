package devin

import (
	"context"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CreateAuthRecord builds a persistent Devin credential record.
func (service *DevinAuthService) CreateAuthRecord(ctx context.Context, token string) (*coreauth.Auth, error) {
	sessionToken := FormatSessionToken(token)
	if sessionToken == "" {
		return nil, fmt.Errorf("devin session token is required")
	}

	userName, userID, orgID, profileErr := service.FetchSelfProfile(ctx, sessionToken)
	if profileErr != nil {
		log.Warnf("failed to fetch Devin profile: %v", profileErr)
	}
	status, statusErr := service.FetchUserStatus(ctx, sessionToken, "")
	if statusErr != nil {
		log.Warnf("failed to fetch Devin user status: %v", statusErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var email string
	var plan string
	if status != nil {
		if userName == "" {
			userName = status.UserName
		}
		if userID == "" {
			userID = status.UserID
		}
		if orgID == "" {
			orgID = status.OrgID
		}
		email = status.Email
		plan = status.Plan
	}

	identifier := firstNonEmptyDevinIdentity(userName, userID)
	fileName := CredentialFileName(userID, orgID, sessionToken)
	if identifier == "" {
		identifier = strings.TrimSuffix(strings.TrimPrefix(fileName, "devin-"), ".json")
	}
	label := fmt.Sprintf("Devin (%s)", identifier)
	if email != "" {
		label = fmt.Sprintf("Devin (%s - %s)", identifier, email)
	}

	attributes := map[string]string{
		"session_token": sessionToken,
		"base_url":      DefaultServerURL,
		"auth_kind":     "oauth",
	}
	metadata := map[string]any{
		"type":          "devin",
		"api_key":       sessionToken,
		"session_token": sessionToken,
		"base_url":      DefaultServerURL,
		"auth_kind":     "oauth",
	}
	for key, value := range map[string]string{
		"user_name": userName,
		"user_id":   userID,
		"org_id":    orgID,
	} {
		if value = strings.TrimSpace(value); value != "" {
			attributes[key] = value
			metadata[key] = value
		}
	}
	if email != "" {
		attributes["email"] = email
		metadata["email"] = email
	}
	if plan != "" {
		attributes["plan"] = plan
		metadata["plan"] = plan
	}
	quota := coreauth.QuotaState{
		Family:         "devin",
		ResetAuthority: coreauth.StateResetAuthoritySuccess,
	}
	if status != nil {
		ApplyQuotaStatus(&quota, status, time.Now().UTC())
	}

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "devin",
		FileName:   fileName,
		Label:      label,
		Status:     coreauth.StatusActive,
		Attributes: attributes,
		Metadata:   metadata,
		Quota:      quota,
	}, nil
}

// ApplyQuotaStatus replaces the transient Devin quota observation while preserving cooldown state.
func ApplyQuotaStatus(quota *coreauth.QuotaState, status *DevinUserStatus, observedAt time.Time) {
	if quota == nil || status == nil {
		return
	}
	quota.Family = "devin"
	quota.ObservedAt = observedAt.UTC()
	quota.Signals = make(map[string]string, 9)
	quota.Signals["daily_quota_present"] = fmt.Sprintf("%t", status.DailyQuotaPresent)
	quota.Signals["weekly_quota_present"] = fmt.Sprintf("%t", status.WeeklyQuotaPresent)
	if status.Plan != "" {
		quota.Signals["plan"] = status.Plan
	}
	if status.DailyQuotaPresent {
		quota.Signals["daily_quota_remaining_percent"] = fmt.Sprintf("%d%%", status.DailyQuotaRemainingPercent)
		if !status.DailyQuotaResetAt.IsZero() {
			quota.Signals["daily_quota_reset_at"] = status.DailyQuotaResetAt.UTC().Format(time.RFC3339)
		}
	}
	if status.WeeklyQuotaPresent {
		quota.Signals["weekly_quota_remaining_percent"] = fmt.Sprintf("%d%%", status.WeeklyQuotaRemainingPercent)
		if !status.WeeklyQuotaResetAt.IsZero() {
			quota.Signals["weekly_quota_reset_at"] = status.WeeklyQuotaResetAt.UTC().Format(time.RFC3339)
		}
	}
	if !status.PlanStart.IsZero() {
		quota.Signals["plan_start"] = status.PlanStart.UTC().Format(time.RFC3339)
	}
	if !status.PlanEnd.IsZero() {
		quota.Signals["plan_end"] = status.PlanEnd.UTC().Format(time.RFC3339)
	}
}

func firstNonEmptyDevinIdentity(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

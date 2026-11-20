package devin

import (
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestBuildGetUserStatusRequest(t *testing.T) {
	reqBytes := buildGetUserStatusRequest("test-token-123", "test-device-seed")
	if len(reqBytes) == 0 {
		t.Fatal("expected non-empty request bytes")
	}

	num, wireType, n := protowire.ConsumeTag(reqBytes)
	if num != 1 || wireType != protowire.BytesType {
		t.Fatalf("expected Field 1 BytesType, got num=%d wireType=%d", num, wireType)
	}

	f1Bytes, _ := protowire.ConsumeBytes(reqBytes[n:])
	if len(f1Bytes) == 0 {
		t.Fatal("expected non-empty ClientMetadata bytes")
	}
}

func buildMockUserStatusResponse() []byte {
	// PlanInfo
	var planInfo []byte
	planInfo = protowire.AppendTag(planInfo, 2, protowire.BytesType)
	planInfo = protowire.AppendString(planInfo, "Pro")

	var orgInfo []byte
	orgInfo = protowire.AppendTag(orgInfo, 4, protowire.BytesType)
	orgInfo = protowire.AppendString(orgInfo, "org-test-123")
	orgInfo = protowire.AppendTag(orgInfo, 8, protowire.BytesType)
	orgInfo = protowire.AppendString(orgInfo, "TestOrg")

	planInfo = protowire.AppendTag(planInfo, 33, protowire.BytesType)
	planInfo = protowire.AppendBytes(planInfo, orgInfo)

	// PlanStatus
	var planStatus []byte
	planStatus = protowire.AppendTag(planStatus, 1, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planInfo)

	// plan_start
	var planStart []byte
	planStart = protowire.AppendTag(planStart, 1, protowire.VarintType)
	planStart = protowire.AppendVarint(planStart, 1789087364)
	planStatus = protowire.AppendTag(planStatus, 2, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planStart)

	// plan_end
	var planEnd []byte
	planEnd = protowire.AppendTag(planEnd, 1, protowire.VarintType)
	planEnd = protowire.AppendVarint(planEnd, 1791679364)
	planStatus = protowire.AppendTag(planStatus, 3, protowire.BytesType)
	planStatus = protowire.AppendBytes(planStatus, planEnd)

	// daily quota: 100%
	planStatus = protowire.AppendTag(planStatus, 14, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 100)

	// weekly quota: 50%
	planStatus = protowire.AppendTag(planStatus, 15, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 50)

	// daily reset: 1789200000
	planStatus = protowire.AppendTag(planStatus, 17, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 1789200000)

	// weekly reset: 1789286400
	planStatus = protowire.AppendTag(planStatus, 18, protowire.VarintType)
	planStatus = protowire.AppendVarint(planStatus, 1789286400)

	// UserStatus
	var userStatus []byte
	userStatus = protowire.AppendTag(userStatus, 3, protowire.BytesType)
	//goland:noinspection SpellCheckingInspection
	userStatus = protowire.AppendString(userStatus, "testuser")

	userStatus = protowire.AppendTag(userStatus, 5, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "team-123")

	userStatus = protowire.AppendTag(userStatus, 7, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "testuser@example.com")

	userStatus = protowire.AppendTag(userStatus, 13, protowire.BytesType)
	userStatus = protowire.AppendBytes(userStatus, planStatus)

	userStatus = protowire.AppendTag(userStatus, 36, protowire.BytesType)
	userStatus = protowire.AppendString(userStatus, "user-id-456")

	// Top-level response: Field 1 is UserStatus
	var resp []byte
	resp = protowire.AppendTag(resp, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, userStatus)

	return resp
}

func TestParseGetUserStatusResponse(t *testing.T) {
	mockBytes := buildMockUserStatusResponse()

	status, err := parseGetUserStatusResponse(mockBytes)
	if err != nil || status == nil {
		t.Fatalf("ParseGetUserStatusResponse failed: status=%+v error=%v", status, err)
	}

	//goland:noinspection SpellCheckingInspection
	if status.UserName != "testuser" {
		//goland:noinspection SpellCheckingInspection
		t.Errorf("expected UserName=testuser, got %q", status.UserName)
	}
	if status.Email != "testuser@example.com" {
		t.Errorf("expected Email=testuser@example.com, got %q", status.Email)
	}
	if status.UserID != "user-id-456" {
		t.Errorf("expected UserID=user-id-456, got %q", status.UserID)
	}
	if status.TeamID != "team-123" {
		t.Errorf("expected TeamID=team-123, got %q", status.TeamID)
	}
	if status.Plan != "Pro" {
		t.Errorf("expected Plan=Pro, got %q", status.Plan)
	}
	if status.OrgID != "org-test-123" {
		t.Errorf("expected OrgID=org-test-123, got %q", status.OrgID)
	}
	if status.OrgName != "TestOrg" {
		t.Errorf("expected OrgName=TestOrg, got %q", status.OrgName)
	}
	if status.DailyQuotaRemainingPercent != 100 {
		t.Errorf("expected DailyQuotaRemainingPercent=100, got %d", status.DailyQuotaRemainingPercent)
	}
	if status.WeeklyQuotaRemainingPercent != 50 {
		t.Errorf("expected WeeklyQuotaRemainingPercent=50, got %d", status.WeeklyQuotaRemainingPercent)
	}
	expectedDailyReset := time.Unix(1789200000, 0).UTC()
	if !status.DailyQuotaResetAt.Equal(expectedDailyReset) {
		t.Errorf("expected DailyQuotaResetAt=%v, got %v", expectedDailyReset, status.DailyQuotaResetAt)
	}
	expectedWeeklyReset := time.Unix(1789286400, 0).UTC()
	if !status.WeeklyQuotaResetAt.Equal(expectedWeeklyReset) {
		t.Errorf("expected WeeklyQuotaResetAt=%v, got %v", expectedWeeklyReset, status.WeeklyQuotaResetAt)
	}
}

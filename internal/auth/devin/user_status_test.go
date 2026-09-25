package devin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestParseGetUserStatusResponsePreservesQuotaPresence(t *testing.T) {
	response := testDevinUserStatusResponse(0, 75)
	status, err := ParseGetUserStatusResponse(response)
	if err != nil {
		t.Fatalf("ParseGetUserStatusResponse(): %v", err)
	}
	if status.UserName != "alice" || status.UserID != "user-1" || status.TeamID != "team-1" ||
		status.Email != "alice@example.test" || status.OrgID != "org-1" || status.OrgName != "Example Org" ||
		status.Plan != "pro" {
		t.Fatalf("status identity = %+v", status)
	}
	if !status.DailyQuotaPresent || status.DailyQuotaRemainingPercent != 0 ||
		!status.WeeklyQuotaPresent || status.WeeklyQuotaRemainingPercent != 75 {
		t.Fatalf("status quotas = %+v", status)
	}
	if !status.PlanStart.Equal(time.Unix(1000, 0).UTC()) ||
		!status.PlanEnd.Equal(time.Unix(2000, 0).UTC()) ||
		!status.DailyQuotaResetAt.Equal(time.Unix(3000, 0).UTC()) ||
		!status.WeeklyQuotaResetAt.Equal(time.Unix(4000, 0).UTC()) {
		t.Fatalf("status timestamps = %+v", status)
	}
}

func TestParseGetUserStatusResponseRejectsMalformedProtobuf(t *testing.T) {
	wrongRootWire := protowire.AppendTag(nil, 1, protowire.VarintType)
	wrongRootWire = protowire.AppendVarint(wrongRootWire, 1)

	badPercentPlan := testDevinVarintField(nil, 14, 101)
	badPercentUser := testDevinBytesField(nil, 13, badPercentPlan)
	badPercent := testDevinBytesField(nil, 1, badPercentUser)

	missingTimestampPlan := testDevinBytesField(nil, 2, nil)
	missingTimestampUser := testDevinBytesField(nil, 13, missingTimestampPlan)
	missingTimestamp := testDevinBytesField(nil, 1, missingTimestampUser)

	for _, payload := range [][]byte{nil, wrongRootWire, badPercent, missingTimestamp, {0x80}} {
		if _, err := ParseGetUserStatusResponse(payload); err == nil {
			t.Fatalf("ParseGetUserStatusResponse(%x) error = nil", payload)
		}
	}
}

func TestFetchUserStatusUsesUnaryConnectHeaders(t *testing.T) {
	responsePayload := testDevinUserStatusResponse(10, 20)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != DevinGetUserStatusPath || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		token := devinTokenPrefix + "token"
		if request.Header.Get("Authorization") != "Basic "+token+"-"+token ||
			request.Header.Get("Connect-Protocol-Version") != "1" ||
			request.Header.Get("Content-Type") != "application/proto" {
			t.Fatalf("request headers = %v", request.Header)
		}
		if request.Header.Get("Sentry-Trace") != "" {
			t.Fatalf("unary status request included Sentry-Trace: %q", request.Header.Get("Sentry-Trace"))
		}
		if request.Header.Get("User-Agent") != "" {
			t.Fatalf("User-Agent = %q, want empty", request.Header.Get("User-Agent"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !strings.Contains(string(body), token) {
			t.Fatal("request metadata is missing session token")
		}
		_, _ = response.Write(responsePayload)
	}))
	defer server.Close()

	service := NewDevinAuthService(server.Client())
	service.SetServerBaseURL(server.URL)
	status, err := service.FetchUserStatus(context.Background(), "token", "device-seed")
	if err != nil {
		t.Fatalf("FetchUserStatus(): %v", err)
	}
	if status.DailyQuotaRemainingPercent != 10 || status.WeeklyQuotaRemainingPercent != 20 {
		t.Fatalf("status quotas = %+v", status)
	}
}

func TestGenerateDeviceFingerprint(t *testing.T) {
	first := GenerateDeviceFingerprint("seed")
	second := GenerateDeviceFingerprint("seed")
	if first != second || len(first) != devinFingerprintHexLen {
		t.Fatalf("fingerprint equality/length = %t/%d", first == second, len(first))
	}
}

func testDevinUserStatusResponse(daily, weekly uint64) []byte {
	organization := testDevinStringField(nil, 4, "org-1")
	organization = testDevinStringField(organization, 8, "Example Org")
	planInfo := testDevinStringField(nil, 2, "pro")
	planInfo = testDevinBytesField(planInfo, 33, organization)
	plan := testDevinBytesField(nil, 1, planInfo)
	plan = testDevinBytesField(plan, 2, testDevinVarintField(nil, 1, 1000))
	plan = testDevinBytesField(plan, 3, testDevinVarintField(nil, 1, 2000))
	plan = testDevinVarintField(plan, 14, daily)
	plan = testDevinVarintField(plan, 15, weekly)
	plan = testDevinVarintField(plan, 17, 3000)
	plan = testDevinVarintField(plan, 18, 4000)

	user := testDevinStringField(nil, 3, "alice")
	user = testDevinStringField(user, 5, "team-1")
	user = testDevinStringField(user, 7, "alice@example.test")
	user = testDevinBytesField(user, 13, plan)
	user = testDevinStringField(user, 36, "user-1")
	return testDevinBytesField(nil, 1, user)
}

func testDevinStringField(dst []byte, number protowire.Number, value string) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendString(dst, value)
}

func testDevinBytesField(dst []byte, number protowire.Number, value []byte) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}

func testDevinVarintField(dst []byte, number protowire.Number, value uint64) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

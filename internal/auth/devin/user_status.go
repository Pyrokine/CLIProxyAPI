package devin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	DevinGetUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

	devinFingerprintHexLen = 732
	maxDevinStatusBodySize = 4 << 20
	devinClientName        = "chisel"
	devinClientVersion     = "3000.10.21"
)

// DevinUserStatus contains quota, plan, and account data from GetUserStatus.
type DevinUserStatus struct {
	Email                       string    `json:"email,omitempty"`
	UserName                    string    `json:"user_name,omitempty"`
	UserID                      string    `json:"user_id,omitempty"`
	TeamID                      string    `json:"team_id,omitempty"`
	OrgID                       string    `json:"org_id,omitempty"`
	OrgName                     string    `json:"org_name,omitempty"`
	Plan                        string    `json:"plan,omitempty"`
	DailyQuotaRemainingPercent  int64     `json:"daily_quota_remaining_percent"`
	WeeklyQuotaRemainingPercent int64     `json:"weekly_quota_remaining_percent"`
	DailyQuotaPresent           bool      `json:"daily_quota_present"`
	WeeklyQuotaPresent          bool      `json:"weekly_quota_present"`
	DailyQuotaResetAt           time.Time `json:"daily_quota_reset_at,omitempty"`
	WeeklyQuotaResetAt          time.Time `json:"weekly_quota_reset_at,omitempty"`
	PlanStart                   time.Time `json:"plan_start,omitempty"`
	PlanEnd                     time.Time `json:"plan_end,omitempty"`
}

// GenerateDeviceFingerprint creates the fixed-width Devin device identifier.
func GenerateDeviceFingerprint(seed string) string {
	if seed == "" {
		var random [devinFingerprintHexLen / 2]byte
		if _, err := rand.Read(random[:]); err == nil {
			return hex.EncodeToString(random[:])
		}
		seed = uuid.NewString()
	}
	var fingerprint strings.Builder
	for counter := 0; fingerprint.Len() < devinFingerprintHexLen; counter++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed, counter)))
		fingerprint.WriteString(hex.EncodeToString(digest[:]))
	}
	return fingerprint.String()[:devinFingerprintHexLen]
}

func appendDevinStatusString(dst []byte, number protowire.Number, value string) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendString(dst, value)
}

// BuildGetUserStatusRequest encodes a unary GetUserStatus protobuf request.
func BuildGetUserStatusRequest(sessionToken, deviceFingerprint string) []byte {
	if deviceFingerprint == "" {
		deviceFingerprint = GenerateDeviceFingerprint(sessionToken)
	}
	metadata := appendDevinStatusString(nil, 1, devinClientName)
	metadata = appendDevinStatusString(metadata, 2, devinClientVersion)
	metadata = appendDevinStatusString(metadata, 3, sessionToken)
	metadata = appendDevinStatusString(metadata, 4, "en")
	metadata = appendDevinStatusString(metadata, 5, runtime.GOOS)
	metadata = appendDevinStatusString(metadata, 7, devinClientVersion)
	metadata = appendDevinStatusString(metadata, 12, devinClientName)
	metadata = appendDevinStatusString(metadata, 31, deviceFingerprint)
	request := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(request, metadata)
}

// ParseGetUserStatusResponse decodes a unary GetUserStatus protobuf response.
func ParseGetUserStatusResponse(data []byte) (*DevinUserStatus, error) {
	if len(data) == 0 {
		return nil, errors.New("empty Devin user status response")
	}
	status := &DevinUserStatus{}
	foundStatus := false
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return nil, protowire.ParseError(consumed)
		}
		position += consumed
		if number == 1 {
			if wireType != protowire.BytesType {
				return nil, fmt.Errorf("user status field 1 has wire type %d, want bytes", wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return nil, protowire.ParseError(size)
			}
			position += size
			if err := parseDevinUserStatus(value, status); err != nil {
				return nil, err
			}
			foundStatus = true
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return nil, protowire.ParseError(size)
		}
		position += size
	}
	if !foundStatus {
		return nil, errors.New("devin user status response is missing field 1")
	}
	return status, nil
}

func parseDevinUserStatus(data []byte, status *DevinUserStatus) error {
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return protowire.ParseError(consumed)
		}
		position += consumed
		if number == 3 || number == 5 || number == 7 || number == 13 || number == 36 {
			if wireType != protowire.BytesType {
				return fmt.Errorf("user status field %d has wire type %d, want bytes", number, wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return protowire.ParseError(size)
			}
			position += size
			switch number {
			case 3:
				status.UserName = string(value)
			case 5:
				status.TeamID = string(value)
			case 7:
				status.Email = string(value)
			case 13:
				if err := parseDevinPlanStatus(value, status); err != nil {
					return err
				}
			case 36:
				status.UserID = string(value)
			}
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return protowire.ParseError(size)
		}
		position += size
	}
	return nil
}

func parseDevinPlanStatus(data []byte, status *DevinUserStatus) error {
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return protowire.ParseError(consumed)
		}
		position += consumed
		switch number {
		case 1, 2, 3:
			if wireType != protowire.BytesType {
				return fmt.Errorf("plan status field %d has wire type %d, want bytes", number, wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return protowire.ParseError(size)
			}
			position += size
			switch number {
			case 1:
				if err := parseDevinPlanInfo(value, status); err != nil {
					return err
				}
			case 2:
				seconds, err := parseDevinSeconds(value)
				if err != nil {
					return fmt.Errorf("parse plan start: %w", err)
				}
				status.PlanStart = time.Unix(seconds, 0).UTC()
			case 3:
				seconds, err := parseDevinSeconds(value)
				if err != nil {
					return fmt.Errorf("parse plan end: %w", err)
				}
				status.PlanEnd = time.Unix(seconds, 0).UTC()
			}
		case 14, 15, 17, 18:
			if wireType != protowire.VarintType {
				return fmt.Errorf("plan status field %d has wire type %d, want varint", number, wireType)
			}
			value, size := protowire.ConsumeVarint(data[position:])
			if size <= 0 {
				return protowire.ParseError(size)
			}
			position += size
			if value > math.MaxInt64 {
				return fmt.Errorf("plan status field %d overflows int64", number)
			}
			switch number {
			case 14, 15:
				if value > 100 {
					return fmt.Errorf("quota percentage field %d exceeds 100", number)
				}
				if number == 14 {
					status.DailyQuotaRemainingPercent = int64(value)
					status.DailyQuotaPresent = true
				} else {
					status.WeeklyQuotaRemainingPercent = int64(value)
					status.WeeklyQuotaPresent = true
				}
			case 17:
				if value > 0 {
					status.DailyQuotaResetAt = time.Unix(int64(value), 0).UTC()
				}
			case 18:
				if value > 0 {
					status.WeeklyQuotaResetAt = time.Unix(int64(value), 0).UTC()
				}
			}
		default:
			size := protowire.ConsumeFieldValue(number, wireType, data[position:])
			if size <= 0 {
				return protowire.ParseError(size)
			}
			position += size
		}
	}
	return nil
}

func parseDevinPlanInfo(data []byte, status *DevinUserStatus) error {
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return protowire.ParseError(consumed)
		}
		position += consumed
		if number == 2 || number == 33 {
			if wireType != protowire.BytesType {
				return fmt.Errorf("plan info field %d has wire type %d, want bytes", number, wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return protowire.ParseError(size)
			}
			position += size
			if number == 2 {
				status.Plan = string(value)
			} else if err := parseDevinOrganization(value, status); err != nil {
				return err
			}
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return protowire.ParseError(size)
		}
		position += size
	}
	return nil
}

func parseDevinOrganization(data []byte, status *DevinUserStatus) error {
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return protowire.ParseError(consumed)
		}
		position += consumed
		if number == 4 || number == 8 {
			if wireType != protowire.BytesType {
				return fmt.Errorf("organization field %d has wire type %d, want bytes", number, wireType)
			}
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return protowire.ParseError(size)
			}
			position += size
			if number == 4 {
				status.OrgID = string(value)
			} else {
				status.OrgName = string(value)
			}
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return protowire.ParseError(size)
		}
		position += size
	}
	return nil
}

func parseDevinSeconds(data []byte) (int64, error) {
	var seconds int64
	found := false
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return 0, protowire.ParseError(consumed)
		}
		position += consumed
		if number == 1 {
			if wireType != protowire.VarintType {
				return 0, fmt.Errorf("timestamp field 1 has wire type %d, want varint", wireType)
			}
			value, size := protowire.ConsumeVarint(data[position:])
			if size <= 0 {
				return 0, protowire.ParseError(size)
			}
			position += size
			if value > math.MaxInt64 {
				return 0, errors.New("timestamp overflows int64")
			}
			seconds = int64(value)
			found = true
			continue
		}
		size := protowire.ConsumeFieldValue(number, wireType, data[position:])
		if size <= 0 {
			return 0, protowire.ParseError(size)
		}
		position += size
	}
	if !found {
		return 0, errors.New("timestamp is missing field 1")
	}
	return seconds, nil
}

// FetchUserStatus queries Devin's unary Connect endpoint without chat trace headers.
func (service *DevinAuthService) FetchUserStatus(ctx context.Context, sessionToken, deviceSeed string) (*DevinUserStatus, error) {
	if service == nil || service.client == nil {
		return nil, errors.New("devin auth service is not initialized")
	}
	sessionToken = FormatSessionToken(sessionToken)
	if sessionToken == "" {
		return nil, errors.New("devin session token is required")
	}
	requestBody := BuildGetUserStatusRequest(sessionToken, GenerateDeviceFingerprint(deviceSeed))
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(service.serverBaseURL, "/")+DevinGetUserStatusPath,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return nil, fmt.Errorf("create Devin user status request: %w", err)
	}
	request.Header.Set("Authorization", fmt.Sprintf("Basic %s-%s", sessionToken, sessionToken))
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Accept", "*/*")
	request.Header["User-Agent"] = []string{""}

	response, err := service.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Devin user status: %w", err)
	}
	defer response.Body.Close()
	if response.ContentLength > maxDevinStatusBodySize {
		return nil, fmt.Errorf("devin user status response exceeds %d bytes", maxDevinStatusBodySize)
	}
	responseBody, err := readBoundedDevinBody(response.Body, maxDevinStatusBodySize)
	if err != nil {
		return nil, fmt.Errorf("read Devin user status response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("devin user status returned status %d", response.StatusCode)
	}
	return ParseGetUserStatusResponse(responseBody)
}

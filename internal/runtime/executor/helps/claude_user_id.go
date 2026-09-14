package helps

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"

	"github.com/google/uuid"
)

var claudeMetadataDeviceIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type claudeMetadataUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

func generateFakeUserIDWithSessionID(sessionID string) string {
	if _, errParse := uuid.Parse(sessionID); errParse != nil {
		sessionID = uuid.NewString()
	}
	deviceID := make([]byte, 32)
	_, _ = rand.Read(deviceID)
	value, _ := json.Marshal(
		claudeMetadataUserID{
			DeviceID:  hex.EncodeToString(deviceID),
			SessionID: sessionID,
		},
	)
	return string(value)
}

func GenerateFakeUserIDWithSessionID(sessionID string) string {
	return generateFakeUserIDWithSessionID(sessionID)
}

func isValidUserID(userID string) bool {
	var value claudeMetadataUserID
	if errUnmarshal := json.Unmarshal([]byte(userID), &value); errUnmarshal != nil {
		return false
	}
	if !claudeMetadataDeviceIDPattern.MatchString(value.DeviceID) {
		return false
	}
	if _, errParse := uuid.Parse(value.SessionID); errParse != nil {
		return false
	}
	if value.AccountUUID == "" {
		return true
	}
	_, errParse := uuid.Parse(value.AccountUUID)
	return errParse == nil
}

func IsValidUserID(userID string) bool {
	return isValidUserID(userID)
}

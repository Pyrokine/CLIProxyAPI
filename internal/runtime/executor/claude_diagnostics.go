package executor

import (
	"bytes"
	"strings"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeDiagnosticsRequestState struct {
	key      string
	sequence uint64
}

func injectClaudeDiagnostics(
	body []byte,
	auth *cliproxyauth.Auth,
	sessionID string,
) ([]byte, claudeDiagnosticsRequestState) {
	key, sequence, previousMessageID := helps.BeginClaudeDiagnostics(
		claudeDiagnosticsCredentialIdentity(auth),
		sessionID,
	)
	if key == "" {
		return body, claudeDiagnosticsRequestState{}
	}
	value := `{"previous_message_id":null}`
	if previousMessageID != "" {
		value = `{"previous_message_id":` + marshalClaudeJSONString(previousMessageID) + `}`
	}

	if diagnostics := gjson.GetBytes(body, "diagnostics"); diagnostics.Exists() {
		updated, errSet := sjson.SetRawBytes(body, "diagnostics", []byte(value))
		if errSet == nil {
			return updated, claudeDiagnosticsRequestState{key: key, sequence: sequence}
		}
	}
	if contextManagement := gjson.GetBytes(body, "context_management"); contextManagement.Exists() {
		start := contextManagement.Index
		insertAt := start + len(contextManagement.Raw)
		if start >= 0 && insertAt >= start && insertAt <= len(body) &&
			bytes.Equal(body[start:insertAt], []byte(contextManagement.Raw)) {
			updated := make([]byte, 0, len(body)+len(value)+len(`,"diagnostics":`))
			updated = append(updated, body[:insertAt]...)
			updated = append(updated, `,"diagnostics":`...)
			updated = append(updated, value...)
			updated = append(updated, body[insertAt:]...)
			return updated, claudeDiagnosticsRequestState{key: key, sequence: sequence}
		}
	}
	updated, errSet := sjson.SetRawBytes(body, "diagnostics", []byte(value))
	if errSet != nil {
		return body, claudeDiagnosticsRequestState{}
	}
	return updated, claudeDiagnosticsRequestState{key: key, sequence: sequence}
}

func claudeDiagnosticsCredentialIdentity(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return "index:" + index
	}
	deviceIDs := claudeauth.NormalizeDeviceIDPool(claudeauth.ReadDeviceIDPool(&auth.Metadata))
	if len(deviceIDs) > 0 {
		return "device:" + deviceIDs[0]
	}
	if accountUUID := helps.ClaudeCredentialAccountUUID(auth); accountUUID != "" {
		return "account:" + accountUUID
	}
	return ""
}

func commitClaudeDiagnostics(state claudeDiagnosticsRequestState, messageID string) {
	helps.CommitClaudeDiagnostics(state.key, state.sequence, messageID)
}

func claudeMessageIDFromResponse(data []byte) string {
	return strings.TrimSpace(gjson.GetBytes(data, "id").String())
}

func observeClaudeStreamLine(line []byte, messageID *string, completed *bool) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "message_start":
		if id := strings.TrimSpace(root.Get("message.id").String()); id != "" {
			*messageID = id
		}
	case "message_stop":
		*completed = true
	}
}

func claudeMessageIDFromSSE(data []byte) string {
	var messageID string
	completed := false
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		observeClaudeStreamLine(line, &messageID, &completed)
	}
	if !completed {
		return ""
	}
	return messageID
}

type claudeDiagnosticsStreamObserver struct {
	state     claudeDiagnosticsRequestState
	messageID string
	completed bool
}

func newClaudeDiagnosticsStreamObserver(
	state claudeDiagnosticsRequestState,
) claudeStreamObserver {
	if state.key == "" || state.sequence == 0 {
		return nil
	}
	return &claudeDiagnosticsStreamObserver{state: state}
}

func (observer *claudeDiagnosticsStreamObserver) ObserveClaudeSSELine(line []byte) {
	if observer == nil {
		return
	}
	observeClaudeStreamLine(line, &observer.messageID, &observer.completed)
}

func (observer *claudeDiagnosticsStreamObserver) FinishClaudeStream(outcome claudeStreamOutcome) {
	if observer == nil || outcome != claudeStreamOutcomeComplete || !observer.completed {
		return
	}
	commitClaudeDiagnostics(observer.state, observer.messageID)
}

type claudeStreamObservers []claudeStreamObserver

func combineClaudeStreamObservers(observers ...claudeStreamObserver) claudeStreamObserver {
	combined := make(claudeStreamObservers, 0, len(observers))
	for _, observer := range observers {
		if observer != nil {
			combined = append(combined, observer)
		}
	}
	if len(combined) == 0 {
		return nil
	}
	return combined
}

func (observers claudeStreamObservers) ObserveClaudeSSELine(line []byte) {
	for _, observer := range observers {
		observer.ObserveClaudeSSELine(line)
	}
}

func (observers claudeStreamObservers) FinishClaudeStream(outcome claudeStreamOutcome) {
	for _, observer := range observers {
		observer.FinishClaudeStream(outcome)
	}
}

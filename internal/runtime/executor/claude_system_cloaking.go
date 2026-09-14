package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func getClaudeWorkloadFromContext(ctx context.Context) string {
	if ginCtx, ok := util.GinContextValue(ctx).(interface{ GetHeader(string) string }); ok && ginCtx != nil {
		return strings.TrimSpace(ginCtx.GetHeader("X-CPA-Claude-Workload"))
	}
	return ""
}

func injectClaudeFakeUserID(ctx context.Context, payload []byte, apiKey string, useCache bool) ([]byte, error) {
	generateID := func() (string, error) {
		if useCache {
			return helps.CachedUserIDRequired(ctx, apiKey)
		}
		sessionID, errSessionID := helps.CachedSessionIDRequired(ctx, apiKey)
		if errSessionID != nil {
			return "", errSessionID
		}
		return helps.GenerateFakeUserIDWithSessionID(sessionID), nil
	}

	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		userID, errUserID := generateID()
		if errUserID != nil {
			return nil, errUserID
		}
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", userID)
		return payload, nil
	}

	existingUserID := gjson.GetBytes(payload, "metadata.user_id").String()
	if existingUserID == "" || !helps.IsValidUserID(existingUserID) {
		userID, errUserID := generateID()
		if errUserID != nil {
			return nil, errUserID
		}
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", userID)
	}
	return payload, nil
}

func claudeBillingFingerprintMessageText(payload []byte) string {
	messageText := ""
	gjson.GetBytes(payload, "messages").ForEach(
		func(_, message gjson.Result) bool {
			if message.Get("role").String() != "user" {
				return true
			}
			content := message.Get("content")
			candidate := ""
			if content.Type == gjson.String {
				candidate = content.String()
			} else if content.IsArray() {
				content.ForEach(
					func(_, part gjson.Result) bool {
						if part.Get("type").String() == "text" {
							candidate = part.Get("text").String()
						}
						return true
					},
				)
			}
			if candidate != "" {
				messageText = candidate
			}
			return true
		},
	)
	return messageText
}

func computeClaudeFingerprint(messageText, version string) string {
	indices := [3]int{4, 7, 20}
	runes := []rune(messageText)
	var selected strings.Builder
	for _, index := range indices {
		if index < len(runes) {
			selected.WriteRune(runes[index])
		} else {
			selected.WriteRune('0')
		}
	}
	hash := sha256.Sum256([]byte(claudeFingerprintSalt + selected.String() + version))
	return hex.EncodeToString(hash[:])[:3]
}

func generateClaudeBillingHeader(cchSigning bool, version, messageText, entrypoint, workload string) string {
	if entrypoint == "" {
		entrypoint = "cli"
	}
	buildHash := computeClaudeFingerprint(messageText, version)
	workloadPart := ""
	if workload != "" {
		workloadPart = fmt.Sprintf(" cc_workload=%s;", workload)
	}
	if cchSigning {
		return fmt.Sprintf(
			"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=00000;%s", version, buildHash,
			entrypoint, workloadPart,
		)
	}
	return fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s;%s", version, buildHash, entrypoint,
		workloadPart,
	)
}

const claudeCodeCLIIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

func checkClaudeSystemInstructionsWithSigningModeAt(
	payload []byte,
	strictMode bool,
	cchSigning bool,
	version string,
	entrypoint string,
	workload string,
	now time.Time,
) []byte {
	system := gjson.GetBytes(payload, "system")
	messageText := claudeBillingFingerprintMessageText(payload)
	billingText := generateClaudeBillingHeader(cchSigning, version, messageText, entrypoint, workload)
	billingBlock := buildClaudeTextBlock(billingText, nil)
	agentBlock := buildClaudeTextBlock(claudeCodeCLIIdentity, map[string]string{"type": "ephemeral"})
	payload, _ = sjson.SetRawBytes(payload, "system", []byte("["+billingBlock+","+agentBlock+"]"))
	if strictMode {
		return injectClaudeCodeCurrentDate(payload, now)
	}

	forwardedSystemBlocks := collectForwardedClaudeSystemPromptBlocks(system)
	if len(forwardedSystemBlocks) == 0 {
		return injectClaudeCodeCurrentDate(payload, now)
	}
	if claudeUsesLegacySystemReminder(payload) {
		payload = prependClaudeSystemRemindersToFirstUserMessage(payload, forwardedSystemBlocks)
	} else {
		payload = insertClaudeMidConversationSystemMessages(payload, forwardedSystemBlocks)
	}
	return injectClaudeCodeCurrentDate(payload, now)
}

func relocateClaudeSystemPromptForCountTokens(payload []byte, strictMode bool) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}
	var forwardedSystemBlocks []string
	if !strictMode {
		forwardedSystemBlocks = collectForwardedClaudeSystemPromptBlocks(system)
	}
	updated, errDelete := sjson.DeleteBytes(payload, "system")
	if errDelete != nil {
		return payload
	}
	payload = updated
	if len(forwardedSystemBlocks) == 0 {
		return payload
	}
	if claudeUsesLegacySystemReminder(payload) {
		return prependClaudeSystemRemindersToFirstUserMessage(payload, forwardedSystemBlocks)
	}
	return insertClaudeMidConversationSystemMessages(payload, forwardedSystemBlocks)
}

var claudeLegacySystemReminderModels = map[string]struct{}{
	"claude-3-5-haiku-20241022":  {},
	"claude-3-5-haiku-latest":    {},
	"claude-3-7-sonnet-20250219": {},
	"claude-3-7-sonnet-latest":   {},
	"claude-haiku-4-5":           {},
	"claude-haiku-4-5-20251001":  {},
	"claude-opus-4":              {},
	"claude-opus-4-20250514":     {},
	"claude-opus-4-1":            {},
	"claude-opus-4-1-20250805":   {},
	"claude-opus-4-5":            {},
	"claude-opus-4-5-20251101":   {},
	"claude-opus-4-6":            {},
	"claude-opus-4-7":            {},
	"claude-sonnet-4":            {},
	"claude-sonnet-4-20250514":   {},
	"claude-sonnet-4-5":          {},
	"claude-sonnet-4-5-20250929": {},
	"claude-sonnet-4-6":          {},
}

func claudeUsesLegacySystemReminder(payload []byte) bool {
	model := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "model").String()))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	_, legacy := claudeLegacySystemReminderModels[model]
	return legacy
}

type claudeCallerSystemBlockError struct {
	statusErr
}

func (claudeCallerSystemBlockError) IsRequestScoped() bool {
	return true
}

func newClaudeCallerSystemBlockError(index int, blockType string) error {
	if blockType == "" {
		blockType = "unknown"
	}
	return claudeCallerSystemBlockError{
		statusErr{
			code: http.StatusBadRequest,
			msg: fmt.Sprintf(
				"invalid_request_error: system.%d.type: Input should be 'text'. "+
					"System instructions support text only, but this block has type %q. "+
					"Move non-text content into a user message.", index, blockType,
			),
		},
	}
}

func validateClaudeCallerSystemBlocks(system gjson.Result) error {
	if !system.IsArray() {
		return nil
	}
	var blockErr error
	index := 0
	system.ForEach(
		func(_, part gjson.Result) bool {
			if strings.TrimSpace(part.Get("type").String()) != "text" {
				blockErr = newClaudeCallerSystemBlockError(index, strings.TrimSpace(part.Get("type").String()))
				return false
			}
			index++
			return true
		},
	)
	return blockErr
}

func collectForwardedClaudeSystemPromptBlocks(system gjson.Result) []string {
	var blocks []string
	appendText := func(text string) {
		if strings.TrimSpace(text) == "" || util.IsClaudeCodeAttributionSystemText(text) || text == claudeCodeCLIIdentity {
			return
		}
		blocks = append(blocks, text)
	}
	if system.IsArray() {
		system.ForEach(
			func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					appendText(part.Get("text").String())
				}
				return true
			},
		)
	} else if system.Type == gjson.String {
		appendText(system.String())
	}
	return blocks
}

func buildClaudeTextBlock(text string, cacheControl map[string]string) string {
	block := `{"type":"text","text":` + marshalClaudeJSONString(text)
	if len(cacheControl) > 0 {
		block += `,"cache_control":{"type":"ephemeral"`
		if ttl := strings.TrimSpace(cacheControl["ttl"]); ttl != "" {
			block += `,"ttl":` + marshalClaudeJSONString(ttl)
		}
		block += "}"
	}
	return block + "}"
}

func marshalClaudeJSONString(value string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(encoded.String(), "\n")
}

func prependClaudeSystemRemindersToFirstUserMessage(payload []byte, texts []string) []byte {
	firstUserIdx := firstClaudeUserMessageIndex(payload)
	if firstUserIdx < 0 || len(texts) == 0 {
		return payload
	}
	reminderTexts := make([]string, 0, len(texts))
	for _, text := range texts {
		reminderTexts = append(reminderTexts, claudeCallerSystemReminder(text))
	}
	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)
	if content.IsArray() {
		blocks := content.Array()
		existing := make(map[string]int, len(blocks))
		for _, block := range blocks {
			if block.Get("type").String() == "text" {
				existing[block.Get("text").String()]++
			}
		}
		reminderBlocks := make([]string, 0, len(reminderTexts))
		for _, reminderText := range reminderTexts {
			if existing[reminderText] > 0 {
				existing[reminderText]--
				continue
			}
			reminderBlocks = append(reminderBlocks, buildClaudeTextBlock(reminderText, nil))
		}
		if len(reminderBlocks) == 0 {
			return payload
		}
		insertAt := 0
		for insertAt < len(blocks) && blocks[insertAt].Get("type").String() == "tool_result" {
			insertAt++
		}
		rawBlocks := make([]string, 0, len(blocks)+len(reminderBlocks))
		for index, block := range blocks {
			if index == insertAt {
				rawBlocks = append(rawBlocks, reminderBlocks...)
			}
			rawBlocks = append(rawBlocks, block.Raw)
		}
		if insertAt == len(blocks) {
			rawBlocks = append(rawBlocks, reminderBlocks...)
		}
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte("["+strings.Join(rawBlocks, ",")+"]"))
	} else if content.Type == gjson.String {
		rawBlocks := make([]string, 0, len(reminderTexts)+1)
		for _, reminderText := range reminderTexts {
			rawBlocks = append(rawBlocks, buildClaudeTextBlock(reminderText, nil))
		}
		rawBlocks = append(rawBlocks, buildClaudeTextBlock(content.String(), nil))
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte("["+strings.Join(rawBlocks, ",")+"]"))
	}
	return payload
}

func claudeCallerSystemReminder(text string) string {
	var reminder strings.Builder
	reminder.WriteString("<system-reminder>\n")
	reminder.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		reminder.WriteByte('\n')
	}
	reminder.WriteString("</system-reminder>")
	return reminder.String()
}

func insertClaudeMidConversationSystemMessages(payload []byte, texts []string) []byte {
	firstUserIdx := firstClaudeUserMessageIndex(payload)
	if firstUserIdx < 0 || len(texts) == 0 {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	messageBlocks := messages.Array()
	insertAt := firstUserIdx + 1
	for insertAt < len(messageBlocks) && messageBlocks[insertAt].Get("role").String() == "user" {
		insertAt++
	}
	if len(messageBlocks)-insertAt >= len(texts) {
		matches := true
		for index, text := range texts {
			message := messageBlocks[insertAt+index]
			if message.Get("role").String() != "system" || claudeMessageContentText(message.Get("content")) != text {
				matches = false
				break
			}
		}
		if matches {
			return payload
		}
	}
	systemMessages := make([]string, 0, len(texts))
	for _, text := range texts {
		content := "[" + buildClaudeTextBlock(text, map[string]string{"type": "ephemeral"}) + "]"
		systemMessages = append(systemMessages, `{"role":"system","content":`+content+"}")
	}
	rawMessages := make([]string, 0, len(messageBlocks)+len(systemMessages))
	for index, message := range messageBlocks {
		if index == insertAt {
			rawMessages = append(rawMessages, systemMessages...)
		}
		rawMessages = append(rawMessages, message.Raw)
	}
	if insertAt == len(messageBlocks) {
		rawMessages = append(rawMessages, systemMessages...)
	}
	payload, _ = sjson.SetRawBytes(payload, "messages", []byte("["+strings.Join(rawMessages, ",")+"]"))
	return payload
}

func claudeMessageContentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var parts []string
	content.ForEach(
		func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				parts = append(parts, block.Get("text").String())
			}
			return true
		},
	)
	return strings.Join(parts, "\n\n")
}

func claudeCodeCurrentTime(cfg *config.Config, auth *cliproxyauth.Auth) time.Time {
	return time.Now().In(claudeCodeTimezone(cfg, auth))
}

func claudeCodeTimezone(cfg *config.Config, auth *cliproxyauth.Auth) *time.Location {
	if timezone := claudeCredentialTimezone(auth); timezone != "" {
		if location, errLocation := time.LoadLocation(timezone); errLocation == nil {
			return location
		}
	}
	if cfg == nil {
		return time.Local
	}
	timezone := strings.TrimSpace(cfg.ClaudeHeaderDefaults.Timezone)
	if timezone == "" {
		return time.Local
	}
	location, errLocation := time.LoadLocation(timezone)
	if errLocation != nil {
		return time.Local
	}
	return location
}

func claudeCredentialTimezone(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if timezone := strings.TrimSpace(auth.Attributes["timezone"]); timezone != "" {
			return timezone
		}
	}
	return strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "timezone"))
}

func claudeCodeCurrentDateReminder(now time.Time) string {
	year, month, day := now.Date()
	return fmt.Sprintf(
		`<system-reminder>
As you answer the user's questions, you can use the following context:
# currentDate
Today's date is %04d-%02d-%02d.

      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>

`, year, int(month), day,
	)
}

func firstClaudeUserMessageIndex(payload []byte) int {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return -1
	}
	firstUserIdx := -1
	messages.ForEach(
		func(index, message gjson.Result) bool {
			if message.Get("role").String() == "user" {
				firstUserIdx = int(index.Int())
				return false
			}
			return true
		},
	)
	return firstUserIdx
}

func isClaudeCodeContextReminder(text string) bool {
	return strings.HasPrefix(text, "<system-reminder>") && strings.Contains(text, "</system-reminder>")
}

func isClaudeCodeCurrentDateReminder(text string) bool {
	return strings.HasPrefix(
		text,
		"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is ",
	)
}

func injectClaudeCodeCurrentDate(payload []byte, now time.Time) []byte {
	firstUserIdx := firstClaudeUserMessageIndex(payload)
	if firstUserIdx < 0 {
		return payload
	}
	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)
	dateBlock := buildClaudeTextBlock(claudeCodeCurrentDateReminder(now), nil)
	if content.Type == gjson.String {
		userBlock := buildClaudeTextBlock(content.String(), map[string]string{"type": "ephemeral"})
		payload, _ = sjson.SetRawBytes(payload, contentPath, []byte("["+dateBlock+","+userBlock+"]"))
		return payload
	}
	if !content.IsArray() {
		return payload
	}
	blocks := content.Array()
	rawBlocks := make([]string, 0, len(blocks)+1)
	actualTextCached := false
	for _, block := range blocks {
		if block.Get("type").String() == "text" {
			text := block.Get("text").String()
			if isClaudeCodeCurrentDateReminder(text) {
				continue
			}
			if !actualTextCached && !isClaudeCodeContextReminder(text) {
				rawBlocks = append(rawBlocks, withClaudeEphemeralCacheControl(block.Raw))
				actualTextCached = true
				continue
			}
		}
		rawBlocks = append(rawBlocks, block.Raw)
	}
	rawBlocks = append([]string{dateBlock}, rawBlocks...)
	payload, _ = sjson.SetRawBytes(payload, contentPath, []byte("["+strings.Join(rawBlocks, ",")+"]"))
	return payload
}

func withClaudeEphemeralCacheControl(rawBlock string) string {
	updated, err := sjson.SetRawBytes([]byte(rawBlock), "cache_control", []byte(`{"type":"ephemeral"}`))
	if err != nil {
		return rawBlock
	}
	return string(updated)
}

const claudeCodeContextManagement = `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`

func injectClaudeCodeContextManagement(payload []byte) ([]byte, bool) {
	if gjson.GetBytes(payload, "context_management").Exists() {
		return payload, false
	}
	if gjson.GetBytes(payload, "thinking.type").String() == "disabled" {
		return payload, false
	}
	updated, err := sjson.SetRawBytes(
		payload,
		"context_management",
		[]byte(claudeCodeContextManagement),
	)
	if err != nil {
		return payload, false
	}
	return updated, true
}

type claudeCodeContextManagementState struct {
	eligible              bool
	callerOwned           bool
	automaticallyInjected bool
	payloadRuleTouched    bool
}

func reconcileClaudeCodeContextManagement(
	payload []byte,
	state claudeCodeContextManagementState,
) []byte {
	thinkingType := gjson.GetBytes(payload, "thinking.type").String()
	contextManagement := gjson.GetBytes(payload, "context_management")

	if thinkingType == "disabled" {
		if state.callerOwned || !state.automaticallyInjected || state.payloadRuleTouched {
			return payload
		}
		if contextManagement.Raw != claudeCodeContextManagement {
			return payload
		}
		updated, err := sjson.DeleteBytes(payload, "context_management")
		if err != nil {
			return payload
		}
		return updated
	}

	if thinkingType != "enabled" && thinkingType != "adaptive" {
		return payload
	}
	if !state.eligible || state.callerOwned || state.payloadRuleTouched || contextManagement.Exists() {
		return payload
	}
	updated, err := sjson.SetRawBytes(
		payload,
		"context_management",
		[]byte(claudeCodeContextManagement),
	)
	if err != nil {
		return payload
	}
	return updated
}

type claudeWirePolicy struct {
	OAuth               bool
	ConfirmedClaudeCode bool
	Cloak               bool
}

type claudeCloakSettings struct {
	strictMode     bool
	sensitiveWords []string
	cacheUserID    bool
}

func resolveClaudeWirePolicy(
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	apiKey string,
	confirmedClaudeCode bool,
) (claudeWirePolicy, claudeCloakSettings) {
	cloakCfg := resolveClaudeKeyCloakConfig(cfg, auth)
	attrMode, attrStrict, attrWords, attrCache := getCloakConfigFromAuth(auth)
	cloakMode := "auto"
	if cfg != nil && cfg.DisableClaudeCloakMode {
		cloakMode = "never"
	}
	settings := claudeCloakSettings{
		strictMode:     attrStrict,
		sensitiveWords: attrWords,
		cacheUserID:    attrCache,
	}
	if attrMode != "" {
		cloakMode = attrMode
	}
	if cloakCfg != nil {
		if mode := strings.TrimSpace(cloakCfg.Mode); mode != "" {
			cloakMode = mode
		}
		if cloakCfg.StrictMode {
			settings.strictMode = true
		}
		if len(cloakCfg.SensitiveWords) > 0 {
			settings.sensitiveWords = cloakCfg.SensitiveWords
		}
		if cloakCfg.CacheUserID != nil {
			settings.cacheUserID = *cloakCfg.CacheUserID
		}
	}
	policy := claudeWirePolicy{
		OAuth:               isClaudeOAuthToken(apiKey),
		ConfirmedClaudeCode: confirmedClaudeCode,
		Cloak:               !confirmedClaudeCode,
	}
	if confirmedClaudeCode {
		return policy, settings
	}
	switch strings.ToLower(strings.TrimSpace(cloakMode)) {
	case "always":
		policy.Cloak = true
	case "never":
		policy.Cloak = false
	}
	return policy, settings
}

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
	"unicode/utf16"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
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
	index := firstClaudeUserMessageIndex(payload)
	if index < 0 {
		return ""
	}
	content := gjson.GetBytes(payload, fmt.Sprintf("messages.%d.content", index))
	if content.Type == gjson.String {
		return content.String()
	}
	for _, part := range content.Array() {
		if part.Get("type").String() == "text" {
			candidate := part.Get("text").String()
			if !isClaudeCodeCurrentDateReminder(candidate) && !isClaudeCodeContextReminder(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func computeClaudeFingerprint(messageText, version string) string {
	units := utf16.Encode([]rune(messageText))
	var sampled [3]uint16
	for i, index := range [...]int{4, 7, 20} {
		sampled[i] = '0'
		if index < len(units) {
			sampled[i] = units[index]
		}
	}
	input := claudeFingerprintSalt + string(utf16.Decode(sampled[:])) + version
	hash := sha256.Sum256([]byte(input))
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

func injectClaudeSubagentBillingTag(body []byte) []byte {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() || len(system.Array()) == 0 {
		return body
	}
	billing := system.Array()[0].Get("text").String()
	if !strings.HasPrefix(billing, "x-anthropic-billing-header:") || strings.Contains(billing, "cc_is_subagent=true;") {
		return body
	}
	billing = strings.TrimSpace(billing)
	if !strings.HasSuffix(billing, ";") {
		billing += ";"
	}
	updated, err := sjson.SetBytes(body, "system.0.text", billing+" cc_is_subagent=true;")
	if err != nil {
		return body
	}
	return updated
}

const claudeCodeCLIIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

const claudeCodeFableReportingOutcomes = `# Reporting outcomes

Report what actually happened, not what you intended. When you say something is done, sent, saved, fixed, or verified, that claim must rest on a result you observed in this session — tool output, the file as it now reads, the page as it now loads — not on what the step should have produced. If you did not check, say you did not check. If any step failed, was skipped, or came back different from what you expected, say so in the first sentence of your report, before anything else, even when the rest of the work succeeded. Never quietly work around a failure in a way that makes it look resolved; a problem the user can see is recoverable, one your summary hides is not. When you stop before the task is complete, your first line says so plainly and names what is left. Do not describe partial work as done, and do not let a summary read as more certain than the evidence behind it.`

func isClaudeFable51Model(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, target := range []string{
		"fable-5-1",
		"fable-5.1",
		"mythos-5-1",
		"mythos-5.1",
	} {
		index := strings.Index(model, target)
		if index < 0 {
			continue
		}
		nextIndex := index + len(target)
		if nextIndex >= len(model) || model[nextIndex] < '0' || model[nextIndex] > '9' {
			return true
		}
	}
	return false
}

func applyClaudeCloakThinkingDisplay(body []byte, callerOwned bool) []byte {
	if callerOwned || len(body) == 0 || gjson.GetBytes(body, "thinking.display").Exists() ||
		!claudeModelUsesProgressDisplay(gjson.GetBytes(body, "model").String()) {
		return body
	}
	switch gjson.GetBytes(body, "thinking.type").String() {
	case "adaptive", "enabled":
		body, _ = sjson.SetBytes(body, "thinking.display", "updates")
	}
	return body
}

func checkClaudeSystemInstructionsWithSigningModeAt(
	payload []byte,
	strictMode bool,
	cchSigning bool,
	version string,
	entrypoint string,
	workload string,
	now time.Time,
	explicitCacheMode bool,
	keepCallerSystemTopLevel ...bool,
) []byte {
	system := gjson.GetBytes(payload, "system")
	messageText := claudeBillingFingerprintMessageText(payload)
	billingText := generateClaudeBillingHeader(cchSigning, version, messageText, entrypoint, workload)
	billingBlock := buildClaudeTextBlock(billingText, nil)
	explicitCacheMode = explicitCacheMode || isExplicitPromptCacheMode(payload)
	var cacheControl map[string]string
	if !explicitCacheMode {
		cacheControl = map[string]string{"type": "ephemeral"}
	}
	agentBlock := buildClaudeTextBlock(claudeCodeCLIIdentity, cacheControl)
	systemBlocks := []string{billingBlock, agentBlock}
	if isClaudeFable51Model(gjson.GetBytes(payload, "model").String()) &&
		!helps.IsClaudeProbeOrHelperRequest(payload) {
		systemBlocks = append(
			systemBlocks,
			buildClaudeTextBlock(claudeCodeFableReportingOutcomes, nil),
		)
	}
	payload, _ = sjson.SetRawBytes(
		payload,
		"system",
		[]byte("["+strings.Join(systemBlocks, ",")+"]"),
	)
	if strictMode {
		return injectClaudeCodeCurrentDate(payload, now, explicitCacheMode)
	}

	forwardedSystemBlocks := collectForwardedClaudeSystemPromptBlocks(system, explicitCacheMode)
	if len(forwardedSystemBlocks) == 0 {
		return injectClaudeCodeCurrentDate(payload, now, explicitCacheMode)
	}
	if claudeHistoryHasAdvisorCallOrResult(payload) {
		for _, block := range forwardedSystemBlocks {
			systemBlocks = append(systemBlocks, block.textBlock(block.text, nil))
		}
		payload, _ = sjson.SetRawBytes(
			payload,
			"system",
			[]byte("["+strings.Join(systemBlocks, ",")+"]"),
		)
		return injectClaudeCodeCurrentDate(payload, now, explicitCacheMode)
	}
	if claudeUsesLegacySystemReminder(payload) {
		payload = prependClaudeSystemRemindersToFirstUserMessage(payload, forwardedSystemBlocks)
	} else if len(keepCallerSystemTopLevel) > 0 && keepCallerSystemTopLevel[0] &&
		claudeMidConversationSystemMessagesAtEnd(payload) {
		for _, block := range forwardedSystemBlocks {
			systemBlocks = append(systemBlocks, block.textBlock(block.text, cacheControl))
		}
		payload, _ = sjson.SetRawBytes(payload, "system", []byte("["+strings.Join(systemBlocks, ",")+"]"))
	} else {
		payload = insertClaudeMidConversationSystemMessages(payload, forwardedSystemBlocks, explicitCacheMode)
	}
	return injectClaudeCodeCurrentDate(payload, now, explicitCacheMode)
}

func relocateClaudeSystemPromptForCountTokens(payload []byte, strictMode, explicitCacheMode bool, keepCallerSystemTopLevel ...bool) []byte {
	system := gjson.GetBytes(payload, "system")
	explicitCacheMode = explicitCacheMode || isExplicitPromptCacheMode(payload)
	if !system.Exists() {
		return payload
	}
	var forwardedSystemBlocks []claudeForwardedSystemBlock
	if !strictMode {
		forwardedSystemBlocks = collectForwardedClaudeSystemPromptBlocks(system, explicitCacheMode)
	}
	if len(forwardedSystemBlocks) == 0 {
		updated, _ := sjson.DeleteBytes(payload, "system")
		return updated
	}
	if claudeHistoryHasAdvisorCallOrResult(payload) {
		blocks := make([]string, 0, len(forwardedSystemBlocks))
		for _, block := range forwardedSystemBlocks {
			blocks = append(blocks, block.textBlock(block.text, nil))
		}
		updated, _ := sjson.SetRawBytes(
			payload,
			"system",
			[]byte("["+strings.Join(blocks, ",")+"]"),
		)
		return updated
	}
	if len(keepCallerSystemTopLevel) > 0 && keepCallerSystemTopLevel[0] &&
		claudeMidConversationSystemMessagesAtEnd(payload) {
		blocks := make([]string, 0, len(forwardedSystemBlocks))
		var cacheControl map[string]string
		if !explicitCacheMode {
			cacheControl = map[string]string{"type": "ephemeral"}
		}
		for _, block := range forwardedSystemBlocks {
			blocks = append(blocks, block.textBlock(block.text, cacheControl))
		}
		updated, _ := sjson.SetRawBytes(payload, "system", []byte("["+strings.Join(blocks, ",")+"]"))
		return updated
	}
	updated, errDelete := sjson.DeleteBytes(payload, "system")
	if errDelete != nil {
		return payload
	}
	payload = updated
	if claudeUsesLegacySystemReminder(payload) {
		return prependClaudeSystemRemindersToFirstUserMessage(payload, forwardedSystemBlocks)
	}
	return insertClaudeMidConversationSystemMessages(payload, forwardedSystemBlocks, explicitCacheMode)
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

func claudePayloadHasMidSystemMessage(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	found := false
	messages.ForEach(
		func(_, message gjson.Result) bool {
			if strings.EqualFold(
				strings.TrimSpace(message.Get("role").String()),
				"system",
			) {
				found = true
				return false
			}
			return true
		},
	)
	return found
}

type claudeMidSystemMessageModelError struct {
	statusErr
}

func (claudeMidSystemMessageModelError) IsRequestScoped() bool {
	return true
}

func newClaudeMidSystemMessageModelError(model string) error {
	if model == "" {
		model = "unknown"
	}
	return claudeMidSystemMessageModelError{
		statusErr{
			code: http.StatusBadRequest,
			msg: fmt.Sprintf(
				"invalid_request_error: role 'system' is not supported on this model. "+
					"Model %q predates mid-conversation system turns, so system instructions must "+
					"stay in the top-level system field for it.",
				model,
			),
		},
	}
}

func validateClaudeMidSystemMessageModel(
	payload []byte,
	confirmedClaudeCode,
	firstPartyAnthropic bool,
) error {
	if confirmedClaudeCode || !firstPartyAnthropic {
		return nil
	}
	if !claudeUsesLegacySystemReminder(payload) || !claudePayloadHasMidSystemMessage(payload) {
		return nil
	}
	return newClaudeMidSystemMessageModelError(gjson.GetBytes(payload, "model").String())
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

type claudeForwardedSystemBlock struct {
	text         string
	cacheControl string
}

func (block claudeForwardedSystemBlock) textBlock(text string, cacheControl map[string]string) string {
	if block.cacheControl == "" {
		return buildClaudeTextBlock(text, cacheControl)
	}
	return `{"type":"text","text":` + marshalClaudeJSONString(text) + `,"cache_control":` + block.cacheControl + "}"
}

func collectForwardedClaudeSystemPromptBlocks(system gjson.Result, explicitCacheMode ...bool) []claudeForwardedSystemBlock {
	var blocks []claudeForwardedSystemBlock
	appendText := func(text string, injectedIdentity bool, cacheControl string) {
		if strings.TrimSpace(text) == "" || util.IsClaudeCodeAttributionSystemText(text) || injectedIdentity {
			return
		}
		blocks = append(blocks, claudeForwardedSystemBlock{text: text, cacheControl: cacheControl})
	}
	if system.IsArray() {
		system.ForEach(
			func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					text := part.Get("text").String()
					injectedIdentity := strings.TrimSpace(text) == claudeCodeCLIIdentity &&
						part.Get("cache_control.type").String() == "ephemeral"
					cacheControl := ""
					if len(explicitCacheMode) > 0 && explicitCacheMode[0] {
						cacheControl = part.Get("cache_control").Raw
					}
					appendText(text, injectedIdentity, cacheControl)
				}
				return true
			},
		)
	} else if system.Type == gjson.String {
		appendText(system.String(), false, "")
	}
	return blocks
}

func claudeHistoryHasAdvisorCallOrResult(payload []byte) bool {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	for _, message := range messages.Array() {
		content := message.Get("content")
		if content.IsArray() {
			for _, block := range content.Array() {
				switch block.Get("type").String() {
				case "advisor_tool_result", "advisor_redacted_result":
					return true
				case "server_tool_use":
					if block.Get("name").String() == "advisor" {
						return true
					}
				case "tool_result":
					inner := block.Get("content")
					if inner.IsArray() {
						for _, nested := range inner.Array() {
							if nested.Get("type").String() == "advisor_redacted_result" {
								return true
							}
						}
					} else if inner.IsObject() &&
						inner.Get("type").String() == "advisor_redacted_result" {
						return true
					}
				}
			}
		} else if content.IsObject() {
			switch content.Get("type").String() {
			case "advisor_tool_result", "advisor_redacted_result":
				return true
			}
		}
	}
	return false
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

func prependClaudeSystemRemindersToFirstUserMessage(payload []byte, texts []claudeForwardedSystemBlock) []byte {
	firstUserIdx := firstClaudeUserMessageIndex(payload)
	if firstUserIdx < 0 || len(texts) == 0 {
		return payload
	}
	reminderTexts := make([]claudeForwardedSystemBlock, 0, len(texts))
	for _, block := range texts {
		block.text = claudeCallerSystemReminder(block.text)
		reminderTexts = append(reminderTexts, block)
	}
	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)
	if content.IsArray() {
		blocks := content.Array()
		existing := make(map[string]int, len(blocks))
		for _, block := range blocks {
			if block.Get("type").String() == "text" {
				existing[block.Get("text").String()+"\x00"+block.Get("cache_control").Raw]++
			}
		}
		reminderBlocks := make([]string, 0, len(reminderTexts))
		for _, block := range reminderTexts {
			key := block.text + "\x00" + block.cacheControl
			if existing[key] > 0 {
				existing[key]--
				continue
			}
			reminderBlocks = append(reminderBlocks, block.textBlock(block.text, nil))
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
		for _, block := range reminderTexts {
			rawBlocks = append(rawBlocks, block.textBlock(block.text, nil))
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

func claudeLeadingUserRunEnd(messages []gjson.Result, firstUserIndex int) int {
	insertAt := firstUserIndex + 1
	for insertAt < len(messages) {
		message := messages[insertAt]
		content := message.Get("content")
		if message.Get("role").String() != "user" &&
			!(message.Get("role").String() == "system" && message.Get("output_config").Exists() &&
				content.IsArray() && len(content.Array()) == 0) {
			break
		}
		insertAt++
	}
	return insertAt
}

func claudeMidConversationSystemMessagesAtEnd(payload []byte) bool {
	firstUser := firstClaudeUserMessageIndex(payload)
	if firstUser < 0 {
		return false
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return false
	}
	items := messages.Array()
	insertAt := claudeLeadingUserRunEnd(items, firstUser)
	return insertAt == len(items) || insertAt > firstUser+1
}

func insertClaudeMidConversationSystemMessages(payload []byte, texts []claudeForwardedSystemBlock, explicitCacheMode ...bool) []byte {
	firstUserIdx := firstClaudeUserMessageIndex(payload)
	if firstUserIdx < 0 || len(texts) == 0 {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	messageBlocks := messages.Array()
	insertAt := claudeLeadingUserRunEnd(messageBlocks, firstUserIdx)
	if len(messageBlocks)-insertAt >= len(texts) {
		matches := true
		for index, block := range texts {
			message := messageBlocks[insertAt+index]
			if message.Get("role").String() != "system" || claudeMessageContentText(message.Get("content")) != block.text ||
				(block.cacheControl != "" && message.Get("content.0.cache_control").Raw != block.cacheControl) {
				matches = false
				break
			}
		}
		if matches {
			return payload
		}
	}
	systemMessages := make([]string, 0, len(texts))
	var cacheControl map[string]string
	if !isExplicitPromptCacheMode(payload) && (len(explicitCacheMode) == 0 || !explicitCacheMode[0]) {
		cacheControl = map[string]string{"type": "ephemeral"}
	}
	for _, block := range texts {
		content := "[" + block.textBlock(block.text, cacheControl) + "]"
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

type claudeCodeSystemPlacementState struct {
	insertAt    int
	insertedRaw []string
	blocks      []claudeForwardedSystemBlock
}

func captureClaudeCodeSystemPlacement(
	before,
	after []byte,
	cloaked bool,
	explicitCacheMode ...bool,
) claudeCodeSystemPlacementState {
	if !cloaked || claudeUsesLegacySystemReminder(before) {
		return claudeCodeSystemPlacementState{}
	}
	explicit := isExplicitPromptCacheMode(before, after) || (len(explicitCacheMode) > 0 && explicitCacheMode[0])
	texts := collectForwardedClaudeSystemPromptBlocks(gjson.GetBytes(before, "system"), explicit)
	if len(texts) == 0 {
		return claudeCodeSystemPlacementState{}
	}
	beforeMessages := gjson.GetBytes(before, "messages").Array()
	afterMessages := gjson.GetBytes(after, "messages").Array()
	if len(afterMessages) != len(beforeMessages)+len(texts) {
		return claudeCodeSystemPlacementState{}
	}
	firstUserIndex := firstClaudeUserMessageIndex(before)
	if firstUserIndex < 0 {
		return claudeCodeSystemPlacementState{}
	}
	insertAt := claudeLeadingUserRunEnd(beforeMessages, firstUserIndex)
	if insertAt+len(texts) > len(afterMessages) {
		return claudeCodeSystemPlacementState{}
	}
	insertedRaw := make([]string, len(texts))
	for index, text := range texts {
		message := afterMessages[insertAt+index]
		if message.Get("role").String() != "system" ||
			claudeMessageContentText(message.Get("content")) != text.text {
			return claudeCodeSystemPlacementState{}
		}
		insertedRaw[index] = message.Raw
	}
	return claudeCodeSystemPlacementState{
		insertAt:    insertAt,
		insertedRaw: insertedRaw,
		blocks:      texts,
	}
}

func reconcileClaudeCodeSystemPlacementAfterPayload(
	payload []byte,
	state claudeCodeSystemPlacementState,
) []byte {
	if len(state.insertedRaw) == 0 || !claudeUsesLegacySystemReminder(payload) {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages").Array()
	if state.insertAt < 0 || state.insertAt+len(state.insertedRaw) > len(messages) {
		return payload
	}
	for index, raw := range state.insertedRaw {
		if messages[state.insertAt+index].Raw != raw {
			return payload
		}
	}
	rawMessages := make([]string, 0, len(messages)-len(state.insertedRaw))
	for index, message := range messages {
		if index >= state.insertAt && index < state.insertAt+len(state.insertedRaw) {
			continue
		}
		rawMessages = append(rawMessages, message.Raw)
	}
	updated, errSet := sjson.SetRawBytes(
		payload,
		"messages",
		[]byte("["+strings.Join(rawMessages, ",")+"]"),
	)
	if errSet != nil {
		return payload
	}
	return prependClaudeSystemRemindersToFirstUserMessage(updated, state.blocks)
}

type claudeCodeFableState struct {
	injectedFallbacks bool
	injectedDisplay   bool
	injectedReporting bool
}

func hasClaudeFableReportingBlock(body []byte) bool {
	system := gjson.GetBytes(body, "system")
	if !system.IsArray() {
		text := strings.ReplaceAll(system.String(), "\u200b", "")
		return text == claudeCodeFableReportingOutcomes ||
			strings.Contains(text, claudeCodeFableReportingOutcomes)
	}
	for _, block := range system.Array() {
		text := strings.ReplaceAll(block.Get("text").String(), "\u200b", "")
		if text == claudeCodeFableReportingOutcomes {
			return true
		}
	}
	return false
}

func captureClaudeCodeFableState(
	before,
	after []byte,
	cloaked bool,
) claudeCodeFableState {
	if !cloaked || len(before) == 0 || len(after) == 0 {
		return claudeCodeFableState{}
	}
	return claudeCodeFableState{
		injectedFallbacks: !gjson.GetBytes(before, "fallbacks").Exists() &&
			gjson.GetBytes(after, "fallbacks").Exists(),
		injectedDisplay: !gjson.GetBytes(before, "thinking.display").Exists() &&
			gjson.GetBytes(after, "thinking.display").Exists(),
		injectedReporting: !hasClaudeFableReportingBlock(before) &&
			hasClaudeFableReportingBlock(after),
	}
}

func removeClaudeFableReportingBlock(body []byte) []byte {
	system := gjson.GetBytes(body, "system")
	if system.IsArray() {
		blocks := make([]string, 0, len(system.Array()))
		removed := false
		for _, block := range system.Array() {
			if strings.ReplaceAll(block.Get("text").String(), "\u200b", "") ==
				claudeCodeFableReportingOutcomes {
				removed = true
				continue
			}
			blocks = append(blocks, block.Raw)
		}
		if removed {
			body, _ = sjson.SetRawBytes(
				body,
				"system",
				[]byte("["+strings.Join(blocks, ",")+"]"),
			)
		}
		return body
	}
	if strings.ReplaceAll(system.String(), "\u200b", "") ==
		claudeCodeFableReportingOutcomes {
		body, _ = sjson.DeleteBytes(body, "system")
	}
	return body
}

func appendClaudeFableReportingBlock(body []byte) []byte {
	if hasClaudeFableReportingBlock(body) {
		return body
	}
	system := gjson.GetBytes(body, "system")
	blocks := make([]string, 0, len(system.Array())+1)
	switch {
	case system.IsArray():
		for _, block := range system.Array() {
			blocks = append(blocks, block.Raw)
		}
	case system.Type == gjson.String:
		blocks = append(blocks, buildClaudeTextBlock(system.String(), nil))
	case system.Exists():
		return body
	}
	blocks = append(blocks, buildClaudeTextBlock(claudeCodeFableReportingOutcomes, nil))
	body, _ = sjson.SetRawBytes(
		body,
		"system",
		[]byte("["+strings.Join(blocks, ",")+"]"),
	)
	return body
}

func reconcileClaudeCodeFableModelAfterPayload(
	body []byte,
	state claudeCodeFableState,
	payloadTouchedPaths map[string]bool,
	cloaked,
	isProbeOrHelper bool,
) []byte {
	if !cloaked || len(body) == 0 {
		return body
	}
	if isProbeOrHelper {
		if state.injectedFallbacks && !payloadTouchedPaths["fallbacks"] {
			body, _ = sjson.DeleteBytes(body, "fallbacks")
		}
		if state.injectedDisplay && !payloadTouchedPaths["thinking.display"] {
			body, _ = sjson.DeleteBytes(body, "thinking.display")
		}
		if state.injectedReporting && !payloadTouchedPaths["system"] {
			body = removeClaudeFableReportingBlock(body)
		}
		return body
	}
	model := gjson.GetBytes(body, "model").String()
	fallback := ""
	switch {
	case isClaudeFable51Model(model):
		fallback = "claude-opus-5"
	case isClaudeOpus55Model(model):
		fallback = "claude-opus-4-8"
	}
	if !payloadTouchedPaths["fallbacks"] {
		if state.injectedFallbacks && gjson.GetBytes(body, "fallbacks.0.model").String() != fallback {
			body, _ = sjson.DeleteBytes(body, "fallbacks")
		}
		if fallback != "" && !gjson.GetBytes(body, "fallbacks").Exists() {
			body, _ = sjson.SetRawBytes(body, "fallbacks", []byte(`[{"model":"`+fallback+`"}]`))
		}
	}
	if isClaudeFable51Model(model) {
		thinkingType := gjson.GetBytes(body, "thinking.type").String()
		if thinkingType != "adaptive" && thinkingType != "enabled" &&
			state.injectedDisplay && !payloadTouchedPaths["thinking.display"] {
			body, _ = sjson.DeleteBytes(body, "thinking.display")
		}
		body = applyClaudeCloakThinkingDisplay(body, payloadTouchedPaths["thinking.display"])
		if !payloadTouchedPaths["system"] {
			body = appendClaudeFableReportingBlock(body)
		}
		return body
	}
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	thinkingActive := thinkingType == "adaptive" || thinkingType == "enabled"
	if state.injectedDisplay && !payloadTouchedPaths["thinking.display"] &&
		(!thinkingActive || !claudeModelUsesProgressDisplay(gjson.GetBytes(body, "model").String())) {
		body, _ = sjson.DeleteBytes(body, "thinking.display")
	}
	body = applyClaudeCloakThinkingDisplay(body, payloadTouchedPaths["thinking.display"])
	if state.injectedReporting && !payloadTouchedPaths["system"] {
		body = removeClaudeFableReportingBlock(body)
	}
	return body
}

func claudeCodeCurrentTime(cfg *config.Config, auth *cliproxyauth.Auth) time.Time {
	return time.Now().In(claudeCodeTimezone(cfg, auth))
}

func claudeCodeTimezone(cfg *config.Config, auth *cliproxyauth.Auth) *time.Location {
	if timezone := claudeCredentialTimezone(auth); timezone != "" {
		if location, errLocation := time.LoadLocation(timezone); errLocation == nil {
			return location
		} else {
			log.Warnf("invalid Claude credential timezone %q: %v", timezone, errLocation)
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
		log.Warnf("invalid Claude configured timezone %q: %v", timezone, errLocation)
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

func injectClaudeCodeCurrentDate(payload []byte, now time.Time, explicitCacheMode ...bool) []byte {
	firstUserIdx := firstClaudeUserMessageIndex(payload)
	if firstUserIdx < 0 {
		return payload
	}
	contentPath := fmt.Sprintf("messages.%d.content", firstUserIdx)
	content := gjson.GetBytes(payload, contentPath)
	dateBlock := buildClaudeTextBlock(claudeCodeCurrentDateReminder(now), nil)
	var cacheControl map[string]string
	if !isExplicitPromptCacheMode(payload) && (len(explicitCacheMode) == 0 || !explicitCacheMode[0]) {
		cacheControl = map[string]string{"type": "ephemeral"}
	}
	if content.Type == gjson.String {
		userBlock := buildClaudeTextBlock(content.String(), cacheControl)
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
			if cacheControl != nil && !actualTextCached && !isClaudeCodeContextReminder(text) {
				rawBlocks = append(rawBlocks, withClaudeEphemeralCacheControl(block.Raw))
				actualTextCached = true
				continue
			}
		}
		rawBlocks = append(rawBlocks, block.Raw)
	}
	insertAt := 0
	for insertAt < len(rawBlocks) && gjson.Parse(rawBlocks[insertAt]).Get("type").String() == "tool_result" {
		insertAt++
	}
	blocksWithDate := make([]string, 0, len(rawBlocks)+1)
	blocksWithDate = append(blocksWithDate, rawBlocks[:insertAt]...)
	blocksWithDate = append(blocksWithDate, dateBlock)
	blocksWithDate = append(blocksWithDate, rawBlocks[insertAt:]...)
	payload, _ = sjson.SetRawBytes(payload, contentPath, []byte("["+strings.Join(blocksWithDate, ",")+"]"))
	return payload
}

func withClaudeEphemeralCacheControl(rawBlock string) string {
	if gjson.Get(rawBlock, "cache_control").Exists() {
		return rawBlock
	}
	updated, err := sjson.SetRawBytes([]byte(rawBlock), "cache_control", []byte(`{"type":"ephemeral"}`))
	if err != nil {
		return rawBlock
	}
	return string(updated)
}

const claudeCodeContextManagement = `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`

func claudeThinkingAcceptsClearThinking(payload []byte) bool {
	switch gjson.GetBytes(payload, "thinking.type").String() {
	case "enabled", "adaptive":
		return true
	default:
		return false
	}
}

func injectClaudeCodeContextManagement(payload []byte) ([]byte, bool) {
	if gjson.GetBytes(payload, "context_management").Exists() {
		return payload, false
	}
	if !claudeThinkingAcceptsClearThinking(payload) {
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
	contextManagement := gjson.GetBytes(payload, "context_management")

	if !claudeThinkingAcceptsClearThinking(payload) {
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
	Cloak bool
	OAuth bool
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
	fingerprint := resolveClaudeFingerprintPolicy(cfg, auth, apiKey)
	policy := claudeWirePolicy{
		Cloak: !confirmedClaudeCode && fingerprint.ProfileClaudeCodeCLI,
		OAuth: fingerprint.AuthIsOAuthToken,
	}
	if confirmedClaudeCode {
		return policy, settings
	}
	switch strings.ToLower(strings.TrimSpace(cloakMode)) {
	case "always":
		policy.Cloak = true
	case "never":
		policy.Cloak = false
	case "auto":
		policy.Cloak = policy.Cloak || attrMode != "" || cloakCfg != nil
	}
	return policy, settings
}

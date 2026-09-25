package helps

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityCompactionCapsulePrefix  = "cpa-ag-compact-v1:"
	antigravityCompactionCapsuleVersion = 1
	antigravityCompactionDefaultTTL     = 30 * 24 * time.Hour
	antigravityCompactionClockSkew      = 5 * time.Minute
)

// AntigravityCompactionScope binds a capsule to one provider request owner.
type AntigravityCompactionScope struct {
	Provider    string
	Model       string
	Session     string
	Owner       string
	KeyMaterial []byte
	Now         time.Time
	TTL         time.Duration
}

type antigravityCompactionCapsuleData struct {
	Version   int    `json:"version"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Session   string `json:"session"`
	Owner     string `json:"owner"`
	IssuedAt  int64  `json:"issued_at"`
	ExpiresAt int64  `json:"expires_at"`
	Summary   string `json:"summary"`
}

var antigravityCompactionProcessKey struct {
	once sync.Once
	key  []byte
	err  error
}

// HasResponsesCompactionTrigger checks whether input contains a compaction_trigger item.
func HasResponsesCompactionTrigger(payload []byte) bool {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction_trigger" {
			return true
		}
	}
	return false
}

// HasResponsesCompactionItem checks whether input contains a compaction item.
func HasResponsesCompactionItem(payload []byte) bool {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction" {
			return true
		}
	}
	return false
}

// PrepareAntigravityCompactionSummaryPayload prepares a payload for non-stream Antigravity summary generation.
func PrepareAntigravityCompactionSummaryPayload(payload []byte, modelName string) []byte {
	out := payload
	summaryPrompt := `{"type":"message","role":"user","content":[{"type":"input_text","text":"Please provide a concise and comprehensive summary of the preceding conversation and task progress so far, including user goals, key findings, actions taken, and current status, so that work can continue smoothly."}]}`

	input := gjson.GetBytes(out, "input")
	if input.IsArray() {
		var filtered []string
		for _, item := range input.Array() {
			itemType := item.Get("type").String()
			if itemType == "compaction_trigger" {
				continue
			}
			filtered = append(filtered, item.Raw)
		}
		filtered = append(filtered, summaryPrompt)
		newInput := "[" + strings.Join(filtered, ",") + "]"
		out, _ = sjson.SetRawBytes(out, "input", []byte(newInput))
	} else if input.Type == gjson.String && input.String() != "" {
		userMsg := []byte(`{"type":"message","role":"user","content":[{"type":"input_text"}]}`)
		userMsg, _ = sjson.SetBytes(userMsg, "content.0.text", input.String())
		newInput := "[" + string(userMsg) + "," + summaryPrompt + "]"
		out, _ = sjson.SetRawBytes(out, "input", []byte(newInput))
	} else {
		newInput := "[" + summaryPrompt + "]"
		out, _ = sjson.SetRawBytes(out, "input", []byte(newInput))
	}

	// Remove fields not needed or incompatible with summary generation
	for _, field := range []string{
		"stream",
		"tools",
		"tool_choice",
		"previous_response_id",
		"parallel_tool_calls",
		"additional_tools",
		"truncation",
		"metadata",
	} {
		out, _ = sjson.DeleteBytes(out, field)
	}

	out, _ = sjson.SetBytes(out, "stream", false)
	return out
}

func normalizeAntigravityCompactionScope(scope AntigravityCompactionScope) (
	AntigravityCompactionScope,
	error,
) {
	scope.Provider = strings.TrimSpace(scope.Provider)
	scope.Model = strings.TrimSpace(scope.Model)
	scope.Session = strings.TrimSpace(scope.Session)
	scope.Owner = strings.TrimSpace(scope.Owner)
	if scope.Provider == "" || scope.Model == "" || scope.Session == "" || scope.Owner == "" {
		return AntigravityCompactionScope{}, fmt.Errorf("provider, model, session, and owner are required")
	}
	if scope.Now.IsZero() {
		scope.Now = time.Now()
	}
	if scope.TTL == 0 {
		scope.TTL = antigravityCompactionDefaultTTL
	}
	if scope.TTL < 0 {
		return AntigravityCompactionScope{}, fmt.Errorf("capsule TTL must be positive")
	}
	return scope, nil
}

func antigravityCompactionMasterKey(scope AntigravityCompactionScope) ([]byte, error) {
	if len(scope.KeyMaterial) > 0 {
		return bytes.Clone(scope.KeyMaterial), nil
	}
	antigravityCompactionProcessKey.once.Do(func() {
		antigravityCompactionProcessKey.key = make([]byte, sha256.Size)
		_, antigravityCompactionProcessKey.err = io.ReadFull(rand.Reader, antigravityCompactionProcessKey.key)
	})
	if antigravityCompactionProcessKey.err != nil {
		return nil, fmt.Errorf("generate process compaction key: %w", antigravityCompactionProcessKey.err)
	}
	return bytes.Clone(antigravityCompactionProcessKey.key), nil
}

func deriveAntigravityCompactionKey(scope AntigravityCompactionScope) ([]byte, error) {
	masterKey, errKey := antigravityCompactionMasterKey(scope)
	if errKey != nil {
		return nil, errKey
	}
	mac := hmac.New(sha256.New, masterKey)
	for _, value := range []string{
		"cpa-antigravity-compaction-v1",
		scope.Provider,
		scope.Model,
		scope.Session,
		scope.Owner,
	} {
		_, _ = io.WriteString(mac, value)
		_, _ = mac.Write([]byte{0})
	}
	return mac.Sum(nil), nil
}

func antigravityCompactionAdditionalData(scope AntigravityCompactionScope) []byte {
	return []byte(strings.Join([]string{
		antigravityCompactionCapsulePrefix,
		scope.Provider,
		scope.Model,
		scope.Session,
		scope.Owner,
	}, "\x00"))
}

// SealAntigravityCompaction encrypts summary data into an owner-bound opaque capsule.
func SealAntigravityCompaction(summary string, scope AntigravityCompactionScope) (string, error) {
	scope, errScope := normalizeAntigravityCompactionScope(scope)
	if errScope != nil {
		return "", fmt.Errorf("invalid compaction scope: %w", errScope)
	}
	issuedAt := scope.Now.Unix()
	data := antigravityCompactionCapsuleData{
		Version:   antigravityCompactionCapsuleVersion,
		Provider:  scope.Provider,
		Model:     scope.Model,
		Session:   scope.Session,
		Owner:     scope.Owner,
		IssuedAt:  issuedAt,
		ExpiresAt: scope.Now.Add(scope.TTL).Unix(),
		Summary:   summary,
	}
	plaintext, errMarshal := json.Marshal(data)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal compaction capsule: %w", errMarshal)
	}

	key, errKey := deriveAntigravityCompactionKey(scope)
	if errKey != nil {
		return "", errKey
	}
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return "", fmt.Errorf("create cipher: %w", errCipher)
	}
	gcm, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return "", fmt.Errorf("create gcm: %w", errGCM)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, errRead := io.ReadFull(rand.Reader, nonce); errRead != nil {
		return "", fmt.Errorf("generate nonce: %w", errRead)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, antigravityCompactionAdditionalData(scope))
	return antigravityCompactionCapsulePrefix + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// UnsealAntigravityCompaction decrypts and validates an owner-bound opaque capsule.
func UnsealAntigravityCompaction(
	encryptedContent string,
	scope AntigravityCompactionScope,
) (string, error) {
	scope, errScope := normalizeAntigravityCompactionScope(scope)
	if errScope != nil {
		return "", fmt.Errorf("invalid compaction scope: %w", errScope)
	}
	if !strings.HasPrefix(encryptedContent, antigravityCompactionCapsulePrefix) {
		return "", fmt.Errorf("unrecognized compaction capsule format")
	}

	raw := strings.TrimPrefix(encryptedContent, antigravityCompactionCapsulePrefix)
	ciphertext, errDecode := base64.RawURLEncoding.DecodeString(raw)
	if errDecode != nil {
		return "", fmt.Errorf("decode compaction capsule: %w", errDecode)
	}
	key, errKey := deriveAntigravityCompactionKey(scope)
	if errKey != nil {
		return "", errKey
	}
	block, errCipher := aes.NewCipher(key)
	if errCipher != nil {
		return "", fmt.Errorf("create cipher: %w", errCipher)
	}
	gcm, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return "", fmt.Errorf("create gcm: %w", errGCM)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize+gcm.Overhead() {
		return "", fmt.Errorf("compaction capsule ciphertext too short")
	}
	nonce, encrypted := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, errOpen := gcm.Open(
		nil,
		nonce,
		encrypted,
		antigravityCompactionAdditionalData(scope),
	)
	if errOpen != nil {
		return "", fmt.Errorf("invalid compaction capsule for current request scope: %w", errOpen)
	}

	var data antigravityCompactionCapsuleData
	if errUnmarshal := json.Unmarshal(plaintext, &data); errUnmarshal != nil {
		return "", fmt.Errorf("unmarshal compaction capsule: %w", errUnmarshal)
	}
	if data.Version != antigravityCompactionCapsuleVersion {
		return "", fmt.Errorf("unsupported compaction capsule version %d", data.Version)
	}
	if data.Provider != scope.Provider || data.Model != scope.Model || data.Session != scope.Session ||
		data.Owner != scope.Owner {
		return "", fmt.Errorf("compaction capsule ownership mismatch")
	}
	now := scope.Now.Unix()
	if data.IssuedAt <= 0 || data.ExpiresAt <= data.IssuedAt {
		return "", fmt.Errorf("invalid compaction capsule lifetime")
	}
	if data.IssuedAt > scope.Now.Add(antigravityCompactionClockSkew).Unix() {
		return "", fmt.Errorf("compaction capsule issued in the future")
	}
	if now >= data.ExpiresAt {
		return "", fmt.Errorf("compaction capsule expired")
	}
	return data.Summary, nil
}

// ExpandAntigravityCompactionCapsules expands compaction items in input into developer context.
func ExpandAntigravityCompactionCapsules(
	payload []byte,
	scope AntigravityCompactionScope,
) ([]byte, error) {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload, nil
	}

	var expandedItems []string
	changed := false
	for _, item := range input.Array() {
		if item.Get("type").String() == "compaction" {
			encryptedContent := item.Get("encrypted_content").String()
			summary, errUnseal := UnsealAntigravityCompaction(encryptedContent, scope)
			if errUnseal != nil {
				return nil, fmt.Errorf("invalid compaction capsule: %w", errUnseal)
			}
			devMsg := []byte(`{"type":"message","role":"developer","content":[{"type":"input_text"}]}`)
			devMsg, _ = sjson.SetBytes(devMsg, "content.0.text", "Context summary from previous turns:\n"+summary)
			expandedItems = append(expandedItems, string(devMsg))
			changed = true
			continue
		}
		expandedItems = append(expandedItems, item.Raw)
	}

	if !changed {
		return payload, nil
	}

	newInput := "[" + strings.Join(expandedItems, ",") + "]"
	return sjson.SetRawBytes(payload, "input", []byte(newInput))
}

// ExtractAntigravitySummaryText extracts summary text from Antigravity or Responses response.
func ExtractAntigravitySummaryText(respPayload []byte) (string, error) {
	// 1. OpenAI Responses format: iterate through output array and skip reasoning
	output := gjson.GetBytes(respPayload, "output")
	if output.IsArray() {
		var textParts []string
		for _, item := range output.Array() {
			if item.Get("type").String() == "message" {
				content := item.Get("content")
				if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "output_text" {
							if t := part.Get("text").String(); t != "" {
								textParts = append(textParts, t)
							}
						}
					}
				} else if content.Type == gjson.String && content.String() != "" {
					textParts = append(textParts, content.String())
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n"), nil
		}
	}

	// 2. Antigravity Gemini format: response.candidates.0.content.parts or candidates.0.content.parts
	candidates := gjson.GetBytes(respPayload, "response.candidates")
	if !candidates.Exists() {
		candidates = gjson.GetBytes(respPayload, "candidates")
	}
	if candidates.IsArray() && len(candidates.Array()) > 0 {
		var textParts []string
		parts := candidates.Array()[0].Get("content.parts")
		if parts.IsArray() {
			for _, part := range parts.Array() {
				if part.Get("thought").Bool() {
					continue
				}
				if t := part.Get("text").String(); t != "" {
					textParts = append(textParts, t)
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n"), nil
		}
	}

	// 3. Claude format: content array
	content := gjson.GetBytes(respPayload, "content")
	if content.IsArray() {
		var textParts []string
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				if t := block.Get("text").String(); t != "" {
					textParts = append(textParts, t)
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n"), nil
		}
	}

	// 4. OpenAI Chat format: choices.0.message.content
	if text := gjson.GetBytes(respPayload, "choices.0.message.content").String(); text != "" {
		return text, nil
	}

	return "", fmt.Errorf("no summary text found in upstream response")
}

// BuildAntigravityCompactionStreamChunks creates SSE frames for compaction stream response.
func BuildAntigravityCompactionStreamChunks(modelName, capsule string, inputTokens, outputTokens, totalTokens int) [][]byte {
	now := time.Now().Unix()
	responseID := fmt.Sprintf("resp_ag_compact_%d", time.Now().UnixNano())
	itemID := fmt.Sprintf("cmp_ag_compact_%d", time.Now().UnixNano())

	itemInProgress := []byte(`{"type":"compaction","status":"in_progress"}`)
	itemInProgress, _ = sjson.SetBytes(itemInProgress, "id", itemID)
	itemInProgress, _ = sjson.SetBytes(itemInProgress, "encrypted_content", capsule)

	itemCompleted := []byte(`{"type":"compaction","status":"completed"}`)
	itemCompleted, _ = sjson.SetBytes(itemCompleted, "id", itemID)
	itemCompleted, _ = sjson.SetBytes(itemCompleted, "encrypted_content", capsule)

	usageNode := []byte(`{"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`)
	usageNode, _ = sjson.SetBytes(usageNode, "input_tokens", inputTokens)
	usageNode, _ = sjson.SetBytes(usageNode, "output_tokens", outputTokens)
	usageNode, _ = sjson.SetBytes(usageNode, "total_tokens", totalTokens)

	createdResponse := []byte(`{"object":"response","status":"in_progress","background":false,"error":null,"output":[]}`)
	createdResponse, _ = sjson.SetBytes(createdResponse, "id", responseID)
	createdResponse, _ = sjson.SetBytes(createdResponse, "created_at", now)
	createdResponse, _ = sjson.SetBytes(createdResponse, "model", modelName)

	inProgressResponse := createdResponse

	completedResponse := []byte(`{"object":"response","status":"completed","background":false,"error":null}`)
	completedResponse, _ = sjson.SetBytes(completedResponse, "id", responseID)
	completedResponse, _ = sjson.SetBytes(completedResponse, "created_at", now)
	completedResponse, _ = sjson.SetBytes(completedResponse, "completed_at", now)
	completedResponse, _ = sjson.SetBytes(completedResponse, "model", modelName)
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "output", []byte("["+string(itemCompleted)+"]"))
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "usage", usageNode)

	createdPayload := []byte(`{"type":"response.created","sequence_number":0}`)
	createdPayload, _ = sjson.SetRawBytes(createdPayload, "response", createdResponse)

	inProgressPayload := []byte(`{"type":"response.in_progress","sequence_number":1}`)
	inProgressPayload, _ = sjson.SetRawBytes(inProgressPayload, "response", inProgressResponse)

	addedPayload := []byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":0}`)
	addedPayload, _ = sjson.SetRawBytes(addedPayload, "item", itemInProgress)

	donePayload := []byte(`{"type":"response.output_item.done","sequence_number":3,"output_index":0}`)
	donePayload, _ = sjson.SetRawBytes(donePayload, "item", itemCompleted)

	completedPayload := []byte(`{"type":"response.completed","sequence_number":4}`)
	completedPayload, _ = sjson.SetRawBytes(completedPayload, "response", completedResponse)

	frames := [][]byte{
		buildSSEFrame("response.created", createdPayload),
		buildSSEFrame("response.in_progress", inProgressPayload),
		buildSSEFrame("response.output_item.added", addedPayload),
		buildSSEFrame("response.output_item.done", donePayload),
		buildSSEFrame("response.completed", completedPayload),
	}
	return frames
}

// BuildAntigravityCompactionResponse creates JSON response for non-stream response.compaction.
func BuildAntigravityCompactionResponse(modelName, capsule string, inputTokens, outputTokens, totalTokens int) []byte {
	now := time.Now().Unix()
	responseID := fmt.Sprintf("resp_ag_compact_%d", time.Now().UnixNano())
	itemID := fmt.Sprintf("cmp_ag_compact_%d", time.Now().UnixNano())

	itemJSON := []byte(`{"type":"compaction","status":"completed"}`)
	itemJSON, _ = sjson.SetBytes(itemJSON, "id", itemID)
	itemJSON, _ = sjson.SetBytes(itemJSON, "encrypted_content", capsule)

	usageJSON := []byte(`{"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}`)
	usageJSON, _ = sjson.SetBytes(usageJSON, "input_tokens", inputTokens)
	usageJSON, _ = sjson.SetBytes(usageJSON, "output_tokens", outputTokens)
	usageJSON, _ = sjson.SetBytes(usageJSON, "total_tokens", totalTokens)

	respJSON := []byte(`{"object":"response.compaction","status":"completed"}`)
	respJSON, _ = sjson.SetBytes(respJSON, "id", responseID)
	respJSON, _ = sjson.SetBytes(respJSON, "created_at", now)
	respJSON, _ = sjson.SetBytes(respJSON, "model", modelName)
	respJSON, _ = sjson.SetRawBytes(respJSON, "output", []byte("["+string(itemJSON)+"]"))
	respJSON, _ = sjson.SetRawBytes(respJSON, "usage", usageJSON)

	return respJSON
}

func buildSSEFrame(event string, data []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

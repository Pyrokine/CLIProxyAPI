package session

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	canonicalTurnVersion      = "cpa-session-turn-v1"
	largePartThreshold        = 16 * 1024
	sparseFingerprintBytes    = 12 * 1024
	defaultMatcherMaxTurns    = 1024
	defaultMatcherMaxGroups   = 4096
	defaultMatcherMaxPrefixes = 262144
	maxCanonicalTurns         = 4096
	maxCanonicalPartsPerTurn  = 256
)

var (
	iso8601Pattern = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	uuidPattern    = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\b`)
	thinkPattern   = regexp.MustCompile(`(?is)\s*<(?:think|thinking)>.*?</(?:think|thinking)>\s*`)
)

// CanonicalPart is a bounded protocol-independent conversation part.
type CanonicalPart struct {
	Kind         string `json:"kind"`
	MIME         string `json:"mime,omitempty"`
	Value        string `json:"value"`
	Digest       string `json:"digest,omitempty"`
	OriginalSize int    `json:"original_size,omitempty"`
	Sampled      bool   `json:"sampled,omitempty"`
}

// CanonicalTurn is a protocol-independent conversation turn.
type CanonicalTurn struct {
	Role  string          `json:"role"`
	Parts []CanonicalPart `json:"parts"`
}

// FastTurnFingerprint returns a deterministic bounded fingerprint for one turn.
func FastTurnFingerprint(turn CanonicalTurn) string {
	turn = normalizeCanonicalTurn(turn)
	hash := sha256.New()
	writeFingerprintField(hash, canonicalTurnVersion)
	writeFingerprintField(hash, turn.Role)
	for _, part := range turn.Parts {
		writeFingerprintField(hash, part.Kind)
		writeFingerprintField(hash, part.MIME)
		writeFingerprintField(hash, strconv.Itoa(part.OriginalSize))
		if turn.Role != "system" || !part.Sampled {
			writeFingerprintField(hash, part.Digest)
		}
		value := part.Value
		if !part.Sampled && (part.OriginalSize > largePartThreshold || len(value) > largePartThreshold) {
			value = sparseSample(value, sparseFingerprintBytes)
		}
		writeFingerprintField(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeFingerprintField(hash interface{ Write([]byte) (int, error) }, value string) {
	_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hash.Write([]byte(":"))
	_, _ = hash.Write([]byte(value))
	_, _ = hash.Write([]byte{0})
}

// ExtractCanonicalTurns parses supported request protocols into bounded logical turns.
func ExtractCanonicalTurns(format sdktranslator.Format, payload []byte) []CanonicalTurn {
	if len(payload) == 0 {
		return nil
	}
	root := util.ParseGJSONBytesNoCopy(payload)
	if !root.Exists() {
		return nil
	}
	if format.String() == "" {
		format = inferCanonicalFormat(root)
	}
	turns := make([]CanonicalTurn, 0)
	switch {
	case formatEqual(format, sdktranslator.FormatClaude):
		appendMessagesTurns(&turns, root, true)
	case formatEqual(format, sdktranslator.FormatGemini), formatEqual(format, sdktranslator.FormatAntigravity):
		appendGeminiTurns(&turns, root)
	case formatEqual(format, sdktranslator.FormatInteractions):
		appendInteractionTurns(&turns, root)
	case formatEqual(format, sdktranslator.FormatOpenAIResponse), formatEqual(format, sdktranslator.FormatCodex):
		appendResponsesTurns(&turns, root)
	default:
		appendMessagesTurns(&turns, root, false)
	}
	return normalizeCanonicalTurns(turns)
}

func inferCanonicalFormat(root gjson.Result) sdktranslator.Format {
	if request := root.Get("request"); request.Exists() && !root.Get("contents").Exists() {
		root = request
	}
	if root.Get("contents").IsArray() || root.Get("systemInstruction").Exists() || root.Get("system_instruction").Exists() {
		return sdktranslator.FormatGemini
	}
	if root.Get("instructions").Exists() {
		return sdktranslator.FormatOpenAIResponse
	}
	if input := root.Get("input"); input.Exists() {
		if input.Type == gjson.String {
			return sdktranslator.FormatInteractions
		}
		for _, item := range input.Array() {
			kind := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			if strings.Contains(kind, "user_input") || strings.Contains(kind, "instruction") {
				return sdktranslator.FormatInteractions
			}
		}
		return sdktranslator.FormatOpenAIResponse
	}
	if root.Get("system").Exists() {
		return sdktranslator.FormatClaude
	}
	return sdktranslator.FormatOpenAI
}

func appendMessagesTurns(turns *[]CanonicalTurn, root gjson.Result, includeTopLevelSystem bool) {
	if includeTopLevelSystem {
		if system := root.Get("system"); system.Exists() {
			appendTurn(turns, "system", canonicalPartsFromJSON(system))
		}
	}
	root.Get("messages").ForEach(func(_, message gjson.Result) bool {
		if !hasCanonicalTurnCapacity(turns) {
			return false
		}
		role := canonicalRole(message.Get("role").String())
		if role == "" {
			role = "unknown"
		}
		content := message.Get("content")
		parts := canonicalPartsFromJSON(content)
		for _, key := range []string{"tool_calls", "tool_call", "function_call", "tool_use"} {
			if value := message.Get(key); value.Exists() {
				parts = append(parts, canonicalPartsFromJSON(value)...)
			}
		}
		if len(parts) == 0 && !content.Exists() {
			parts = canonicalPartsFromJSON(message)
		}
		appendTurn(turns, role, parts)
		return true
	})
}

func appendResponsesTurns(turns *[]CanonicalTurn, root gjson.Result) {
	if instructions := root.Get("instructions"); instructions.Exists() {
		appendTurn(turns, "system", canonicalPartsFromJSON(instructions))
	}
	input := root.Get("input")
	if !input.Exists() || !hasCanonicalTurnCapacity(turns) {
		return
	}
	if input.Type == gjson.String {
		appendTurn(turns, "user", canonicalPartsFromJSON(input))
		return
	}
	input.ForEach(func(_, item gjson.Result) bool {
		if !hasCanonicalTurnCapacity(turns) {
			return false
		}
		kind := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if kind == "reasoning" || kind == "response.output_text" {
			return true
		}
		role := canonicalRole(item.Get("role").String())
		switch {
		case role != "":
		case strings.Contains(kind, "function_call_output") || strings.Contains(kind, "tool_result"):
			role = "tool"
		case strings.Contains(kind, "function_call") || strings.Contains(kind, "tool_call"):
			role = "assistant"
		case strings.Contains(kind, "compaction"):
			role = "system"
		default:
			role = "unknown"
		}
		content := item.Get("content")
		parts := canonicalPartsFromJSON(content)
		if len(parts) == 0 && !content.Exists() {
			parts = canonicalPartsFromJSON(item)
		}
		appendTurn(turns, role, parts)
		return true
	})
}

func appendGeminiTurns(turns *[]CanonicalTurn, root gjson.Result) {
	if request := root.Get("request"); request.Exists() && !root.Get("contents").Exists() {
		root = request
	}
	cached := root.Get("cachedContent")
	if !cached.Exists() {
		cached = root.Get("cached_content")
	}
	if cached.Exists() {
		appendTurn(turns, "system", []CanonicalPart{canonicalResourcePart(cached)})
	}
	if system := root.Get("systemInstruction"); system.Exists() {
		appendTurn(turns, "system", canonicalPartsFromJSON(system))
	} else if system := root.Get("system_instruction"); system.Exists() {
		appendTurn(turns, "system", canonicalPartsFromJSON(system))
	}
	root.Get("contents").ForEach(func(_, content gjson.Result) bool {
		if !hasCanonicalTurnCapacity(turns) {
			return false
		}
		role := canonicalRole(content.Get("role").String())
		if role == "" {
			role = "unknown"
		}
		contentParts := content.Get("parts")
		parts := canonicalPartsFromJSON(contentParts)
		if len(parts) == 0 && !contentParts.Exists() {
			parts = canonicalPartsFromJSON(content)
		}
		appendTurn(turns, role, parts)
		return true
	})
}

func appendInteractionTurns(turns *[]CanonicalTurn, root gjson.Result) {
	if system := root.Get("system_instruction"); system.Exists() {
		appendTurn(turns, "system", canonicalPartsFromJSON(system))
	} else if system := root.Get("systemInstruction"); system.Exists() {
		appendTurn(turns, "system", canonicalPartsFromJSON(system))
	}
	appendInteractionValue(turns, root.Get("input"), "")
}

func appendInteractionValue(turns *[]CanonicalTurn, value gjson.Result, inheritedRole string) bool {
	if !value.Exists() || !hasCanonicalTurnCapacity(turns) {
		return hasCanonicalTurnCapacity(turns)
	}
	if value.Type == gjson.JSON && value.IsArray() {
		value.ForEach(func(_, child gjson.Result) bool {
			return appendInteractionValue(turns, child, inheritedRole)
		})
		return hasCanonicalTurnCapacity(turns)
	}
	if value.Type != gjson.JSON {
		appendTurn(turns, defaultInteractionRole(inheritedRole), canonicalPartsFromJSON(value))
		return hasCanonicalTurnCapacity(turns)
	}
	if steps := value.Get("steps"); steps.IsArray() {
		role := canonicalRole(value.Get("role").String())
		if role == "" {
			role = inheritedRole
		}
		steps.ForEach(func(_, child gjson.Result) bool {
			return appendInteractionValue(turns, child, role)
		})
		return hasCanonicalTurnCapacity(turns)
	}
	kind := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
	role := canonicalRole(value.Get("role").String())
	if role == "" {
		switch {
		case strings.Contains(kind, "system") || strings.Contains(kind, "developer"):
			role = "system"
		case strings.Contains(kind, "user"):
			role = "user"
		case strings.Contains(kind, "model") || strings.Contains(kind, "assistant"):
			role = "assistant"
		case strings.Contains(kind, "tool") || strings.Contains(kind, "function"):
			role = "tool"
		default:
			role = defaultInteractionRole(inheritedRole)
		}
	}
	content := value.Get("content")
	parts := canonicalPartsFromJSON(content)
	if len(parts) == 0 && !content.Exists() {
		parts = canonicalPartsFromJSON(value)
	}
	appendTurn(turns, role, parts)
	return hasCanonicalTurnCapacity(turns)
}

func defaultInteractionRole(inheritedRole string) string {
	if role := canonicalRole(inheritedRole); role != "" {
		return role
	}
	return "user"
}

func appendTurn(turns *[]CanonicalTurn, role string, parts []CanonicalPart) {
	if !hasCanonicalTurnCapacity(turns) || len(parts) == 0 {
		return
	}
	*turns = append(*turns, CanonicalTurn{Role: role, Parts: limitCanonicalParts(nil, parts)})
}

func hasCanonicalTurnCapacity(turns *[]CanonicalTurn) bool {
	return turns != nil && len(*turns) < maxCanonicalTurns
}

func canonicalPartsFromJSON(value gjson.Result) []CanonicalPart {
	if !value.Exists() {
		return nil
	}
	switch value.Type {
	case gjson.String:
		return []CanonicalPart{canonicalTextPart(value)}
	case gjson.Number, gjson.True, gjson.False:
		return []CanonicalPart{{Kind: "value", Value: value.Raw, OriginalSize: len(value.Raw)}}
	case gjson.JSON:
		if isReasoningJSONPart(value) {
			return nil
		}
		if value.IsArray() {
			parts := make([]CanonicalPart, 0, maxCanonicalPartsPerTurn+1)
			dropped := 0
			value.ForEach(func(_, child gjson.Result) bool {
				if len(parts) >= maxCanonicalPartsPerTurn {
					dropped++
					return true
				}
				parts = limitCanonicalParts(parts, canonicalPartsFromJSON(child))
				return true
			})
			return limitCanonicalPartsCount(parts, dropped)
		}
		if text := value.Get("text"); text.Type == gjson.String {
			return []CanonicalPart{canonicalTextPart(text)}
		}
		if content := value.Get("content"); content.Exists() {
			return canonicalPartsFromJSON(content)
		}
		if parts := value.Get("parts"); parts.Exists() {
			return canonicalPartsFromJSON(parts)
		}
		kind := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		if kind == "input_text" || kind == "output_text" || kind == "text" {
			if text := value.Get("text"); text.Exists() {
				return []CanonicalPart{canonicalTextPart(text)}
			}
		}
		if isToolPartType(kind) {
			return []CanonicalPart{canonicalJSONPart("tool:"+kind, value)}
		}
		if toolKind := geminiToolPartKind(value); toolKind != "" {
			return []CanonicalPart{canonicalJSONPart(toolKind, value)}
		}
		if value.Get("image_url").Exists() || value.Get("inlineData").Exists() ||
			value.Get("inline_data").Exists() || value.Get("fileData").Exists() ||
			value.Get("file_data").Exists() || value.Get("source").Exists() {
			return []CanonicalPart{canonicalJSONPart("media", value)}
		}
		return []CanonicalPart{canonicalJSONPart("json", value)}
	default:
		return []CanonicalPart{canonicalJSONPart("value", value)}
	}
}

func canonicalTextPart(value gjson.Result) CanonicalPart {
	text := normalizeText(value.String(), false)
	if len(text) > largePartThreshold {
		return CanonicalPart{
			Kind:         "text",
			Value:        sparseSample(text, sparseFingerprintBytes),
			Digest:       computeHexSHA256(text),
			OriginalSize: len(text),
			Sampled:      true,
		}
	}
	return CanonicalPart{Kind: "text", Value: text, OriginalSize: len(text)}
}

func canonicalResourcePart(value gjson.Result) CanonicalPart {
	part := canonicalTextPart(value)
	part.Kind = "resource"
	return part
}

func canonicalJSONPart(kind string, value gjson.Result) CanonicalPart {
	raw := value.Raw
	if len(raw) > largePartThreshold {
		return CanonicalPart{
			Kind:         kind,
			Value:        sparseSample(raw, sparseFingerprintBytes),
			Digest:       computeHexSHA256(raw),
			OriginalSize: len(raw),
			Sampled:      true,
		}
	}
	var decoded any
	if json.Unmarshal([]byte(raw), &decoded) == nil {
		if encoded, err := json.Marshal(decoded); err == nil {
			raw = string(encoded)
		}
	}
	return CanonicalPart{Kind: kind, Value: raw, OriginalSize: len(raw)}
}

func computeHexSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func limitCanonicalParts(parts, added []CanonicalPart) []CanonicalPart {
	if len(added) == 0 {
		return parts
	}
	existingDropped := 0
	if last := added[len(added)-1]; last.Kind == "value" && strings.HasPrefix(last.Value, "<truncated:") {
		existingDropped = last.OriginalSize
		added = added[:len(added)-1]
	}
	space := maxCanonicalPartsPerTurn - len(parts)
	if space <= 0 {
		return limitCanonicalPartsCount(parts, len(added)+existingDropped)
	}
	if len(added) > space {
		parts = append(parts, added[:space]...)
		return limitCanonicalPartsCount(parts, len(added)-space+existingDropped)
	}
	parts = append(parts, added...)
	return limitCanonicalPartsCount(parts, existingDropped)
}

func limitCanonicalPartsCount(parts []CanonicalPart, dropped int) []CanonicalPart {
	if dropped <= 0 {
		return parts
	}
	if len(parts) > 0 && parts[len(parts)-1].Kind == "value" && strings.HasPrefix(parts[len(parts)-1].Value, "<truncated:") {
		parts[len(parts)-1].OriginalSize += dropped
		parts[len(parts)-1].Value = "<truncated:" + strconv.Itoa(parts[len(parts)-1].OriginalSize) + " parts>"
		return parts
	}
	return append(parts, CanonicalPart{
		Kind:         "value",
		Value:        "<truncated:" + strconv.Itoa(dropped) + " parts>",
		OriginalSize: dropped,
	})
}

func isReasoningJSONPart(value gjson.Result) bool {
	if value.Type != gjson.JSON || value.IsArray() {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
	return kind == "thinking" || kind == "reasoning" || kind == "thought" ||
		strings.Contains(kind, "reasoning") || (value.Get("thought").Type == gjson.True && value.Get("thought").Bool())
}

func isToolPartType(value string) bool {
	return strings.Contains(value, "tool") || strings.Contains(value, "function_call") || value == "function"
}

func geminiToolPartKind(value gjson.Result) string {
	for _, field := range []string{"functionCall", "function_call"} {
		if value.Get(field).Exists() {
			return "tool:function_call"
		}
	}
	for _, field := range []string{"functionResponse", "function_response"} {
		if value.Get(field).Exists() {
			return "tool:function_response"
		}
	}
	return ""
}

func canonicalRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return "system"
	case "assistant", "model", "ai":
		return "assistant"
	case "tool", "function":
		return "tool"
	case "user":
		return "user"
	default:
		return strings.ToLower(strings.TrimSpace(role))
	}
}

func normalizeCanonicalTurns(turns []CanonicalTurn) []CanonicalTurn {
	if len(turns) == 0 {
		return nil
	}
	capacity := min(len(turns), maxCanonicalTurns)
	normalized := make([]CanonicalTurn, 0, capacity)
	for _, turn := range turns {
		turn = normalizeCanonicalTurn(turn)
		if turn.Role == "" || len(turn.Parts) == 0 {
			continue
		}
		normalized = append(normalized, turn)
		if len(normalized) == maxCanonicalTurns {
			break
		}
	}
	return normalized
}

func normalizeCanonicalTurn(turn CanonicalTurn) CanonicalTurn {
	turn.Role = canonicalRole(turn.Role)
	parts := make([]CanonicalPart, 0, min(len(turn.Parts), maxCanonicalPartsPerTurn+1))
	for _, part := range turn.Parts {
		if part.Value == "" {
			continue
		}
		part.Kind = strings.ToLower(strings.TrimSpace(part.Kind))
		part.MIME = strings.ToLower(strings.TrimSpace(part.MIME))
		if part.OriginalSize <= 0 {
			part.OriginalSize = len(part.Value)
		}
		if part.Kind == "text" {
			if turn.Role == "system" {
				part.Value = normalizeText(part.Value, true)
			} else if !part.Sampled {
				part.Value = normalizeText(part.Value, false)
			}
			if !part.Sampled {
				part.OriginalSize = len(part.Value)
			}
		}
		if !part.Sampled && len(part.Value) > largePartThreshold {
			fullValue := part.Value
			part.Value = sparseSample(fullValue, sparseFingerprintBytes)
			part.Digest = computeHexSHA256(fullValue)
			part.OriginalSize = len(fullValue)
			part.Sampled = true
		}
		if part.Value != "" {
			parts = append(parts, part)
		}
	}
	parts = limitCanonicalParts(nil, parts)
	indexes := make([]int, 0)
	toolParts := make([]CanonicalPart, 0)
	for index, part := range parts {
		if strings.HasPrefix(part.Kind, "tool:") || strings.Contains(part.Kind, "function_call") {
			indexes = append(indexes, index)
			toolParts = append(toolParts, part)
		}
	}
	if len(toolParts) > 1 {
		sort.SliceStable(toolParts, func(i, j int) bool {
			if toolParts[i].Value != toolParts[j].Value {
				return toolParts[i].Value < toolParts[j].Value
			}
			return toolParts[i].Digest < toolParts[j].Digest
		})
		for index, partIndex := range indexes {
			parts[partIndex] = toolParts[index]
		}
	}
	turn.Parts = parts
	return turn
}

func normalizeText(value string, maskSystemDynamics bool) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = thinkPattern.ReplaceAllString(value, " ")
	if maskSystemDynamics {
		value = iso8601Pattern.ReplaceAllString(value, "<timestamp>")
		value = uuidPattern.ReplaceAllString(value, "<uuid>")
	}
	return strings.TrimSpace(value)
}

func sparseSample(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	head := limit / 3
	middle := limit / 3
	tail := limit - head - middle
	middleStart := (len(value) - middle) / 2
	return value[:head] + value[middleStart:middleStart+middle] + value[len(value)-tail:]
}

func formatEqual(left, right sdktranslator.Format) bool {
	return strings.EqualFold(strings.TrimSpace(left.String()), strings.TrimSpace(right.String()))
}

// MerklePrefixMatch describes the longest active rolling-prefix match.
type MerklePrefixMatch struct {
	AuthID          string
	SessionID       string
	ParentSessionID string
	PrefixLength    int
	IsFork          bool
	AccessNumber    uint64
}

// MerklePrefixBindResult describes identities produced while binding one trajectory.
type MerklePrefixBindResult struct {
	SessionID       string
	ParentSessionID string
	IsFork          bool
	AccessNumber    uint64
}

// MerklePrefixMatcherConfig controls the bounded in-memory matcher.
type MerklePrefixMatcherConfig struct {
	TTL         time.Duration
	MaxTurns    int
	MaxGroups   int
	MaxPrefixes int
	NowFunc     func() time.Time
}

// MerklePrefixMatcher stores flat auth bindings indexed by rolling Merkle prefixes.
type MerklePrefixMatcher struct {
	mu            sync.Mutex
	ttl           time.Duration
	maxTurns      int
	maxGroups     int
	maxPrefixes   int
	nowFunc       func() time.Time
	namespaces    map[string]*lcpNamespace
	lru           *list.List
	lruElements   map[*lcpGroup]*list.Element
	groupCount    int
	prefixCount   int
	accessCounter uint64
	operations    uint64
}

type lcpNamespace struct {
	groups   map[string]*lcpGroup
	prefixes map[string]map[string]*lcpGroup
}

type lcpGroup struct {
	key              string
	namespace        string
	authID           string
	sessionID        string
	parentSessionID  string
	minPrefixLength  int
	fingerprints     []string
	prefixKeys       []string
	expiresAt        time.Time
	lastAccessNumber uint64
}

func NewMerklePrefixMatcher(ttl time.Duration) *MerklePrefixMatcher {
	return NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{TTL: ttl})
}

func NewMerklePrefixMatcherWithConfig(config MerklePrefixMatcherConfig) *MerklePrefixMatcher {
	if config.TTL <= 0 {
		config.TTL = time.Hour
	}
	if config.MaxTurns <= 0 {
		config.MaxTurns = defaultMatcherMaxTurns
	}
	if config.MaxGroups <= 0 {
		config.MaxGroups = defaultMatcherMaxGroups
	}
	if config.MaxPrefixes <= 0 {
		config.MaxPrefixes = defaultMatcherMaxPrefixes
	}
	if config.MaxPrefixes < config.MaxTurns {
		config.MaxPrefixes = config.MaxTurns
	}
	if config.NowFunc == nil {
		config.NowFunc = time.Now
	}
	return &MerklePrefixMatcher{
		ttl:         config.TTL,
		maxTurns:    config.MaxTurns,
		maxGroups:   config.MaxGroups,
		maxPrefixes: config.MaxPrefixes,
		nowFunc:     config.NowFunc,
		namespaces:  make(map[string]*lcpNamespace),
		lru:         list.New(),
		lruElements: make(map[*lcpGroup]*list.Element),
	}
}

func (matcher *MerklePrefixMatcher) now() time.Time {
	if matcher != nil && matcher.nowFunc != nil {
		return matcher.nowFunc()
	}
	return time.Now()
}

func (matcher *MerklePrefixMatcher) Prepare(turns []CanonicalTurn) ([]string, int) {
	if matcher == nil {
		return nil, 0
	}
	return matcher.fingerprints(turns), minimumAffinityPrefixLength(turns)
}

func (matcher *MerklePrefixMatcher) Match(namespace string, turns []CanonicalTurn) (MerklePrefixMatch, bool) {
	fingerprints, minimum := matcher.Prepare(turns)
	return matcher.MatchFingerprints(namespace, fingerprints, minimum)
}

func (matcher *MerklePrefixMatcher) MatchFingerprints(
	namespace string,
	fingerprints []string,
	minimum int,
) (MerklePrefixMatch, bool) {
	if matcher == nil || strings.TrimSpace(namespace) == "" {
		return MerklePrefixMatch{}, false
	}
	var ok bool
	fingerprints, minimum, ok = matcher.sanitizeFingerprints(fingerprints, minimum)
	if !ok {
		return MerklePrefixMatch{}, false
	}
	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	matcher.prepareLocked()
	return matcher.matchLocked(namespace, fingerprints, minimum, matcher.now())
}

func (matcher *MerklePrefixMatcher) Bind(namespace string, turns []CanonicalTurn, authID string) string {
	return matcher.BindWithResult(namespace, turns, authID).SessionID
}

func (matcher *MerklePrefixMatcher) BindWithResult(namespace string, turns []CanonicalTurn, authID string) MerklePrefixBindResult {
	fingerprints, minimum := matcher.Prepare(turns)
	return matcher.BindFingerprintsWithResult(namespace, fingerprints, minimum, authID)
}

func (matcher *MerklePrefixMatcher) BindFingerprints(namespace string, fingerprints []string, minimum int, authID string) string {
	return matcher.BindFingerprintsWithResult(namespace, fingerprints, minimum, authID).SessionID
}

func (matcher *MerklePrefixMatcher) BindFingerprintsWithResult(
	namespace string,
	fingerprints []string,
	minimum int,
	authID string,
) MerklePrefixBindResult {
	if matcher == nil || strings.TrimSpace(namespace) == "" || strings.TrimSpace(authID) == "" {
		return MerklePrefixBindResult{}
	}
	var ok bool
	fingerprints, minimum, ok = matcher.sanitizeFingerprints(fingerprints, minimum)
	if !ok {
		return MerklePrefixBindResult{}
	}
	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	matcher.prepareLocked()
	return matcher.bindLocked(namespace, fingerprints, minimum, strings.TrimSpace(authID), matcher.now())
}

func (matcher *MerklePrefixMatcher) Touch(namespace string, turns []CanonicalTurn, authID string) bool {
	fingerprints, minimum := matcher.Prepare(turns)
	return matcher.TouchFingerprints(namespace, fingerprints, minimum, authID)
}

// TouchFingerprints refreshes or extends a trajectory only for the expected auth.
func (matcher *MerklePrefixMatcher) TouchFingerprints(namespace string, fingerprints []string, minimum int, authID string) bool {
	if matcher == nil || strings.TrimSpace(namespace) == "" || strings.TrimSpace(authID) == "" {
		return false
	}
	var ok bool
	fingerprints, minimum, ok = matcher.sanitizeFingerprints(fingerprints, minimum)
	if !ok {
		return false
	}
	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	matcher.prepareLocked()
	return matcher.touchLocked(namespace, fingerprints, minimum, strings.TrimSpace(authID), matcher.now())
}

func (matcher *MerklePrefixMatcher) Remove(namespace string, turns []CanonicalTurn, authID string) bool {
	fingerprints, _ := matcher.Prepare(turns)
	return matcher.RemoveFingerprints(namespace, fingerprints, authID)
}

func (matcher *MerklePrefixMatcher) RemoveFingerprints(namespace string, fingerprints []string, authID string) bool {
	return matcher.RemoveFingerprintsBefore(namespace, fingerprints, authID, 0)
}

// RemoveFingerprintsBefore preserves bindings refreshed after maxGeneration.
func (matcher *MerklePrefixMatcher) RemoveFingerprintsBefore(
	namespace string,
	fingerprints []string,
	authID string,
	maxGeneration uint64,
) bool {
	if matcher == nil || namespace == "" || authID == "" || len(fingerprints) == 0 {
		return false
	}
	if len(fingerprints) > matcher.maxTurns {
		fingerprints = fingerprints[:matcher.maxTurns]
	}
	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	matcher.prepareLocked()
	namespaceGroups := matcher.namespaces[namespace]
	if namespaceGroups == nil {
		return false
	}
	keys := rollingPrefixKeys(fingerprints)
	group := namespaceGroups.groups[keys[len(keys)-1]]
	if group == nil || group.authID != authID || (maxGeneration > 0 && group.lastAccessNumber > maxGeneration) {
		return false
	}
	matcher.removeGroupLocked(group)
	return true
}

func (matcher *MerklePrefixMatcher) InvalidateAuth(authID string) {
	if matcher == nil || strings.TrimSpace(authID) == "" {
		return
	}
	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	for _, namespace := range matcher.namespaces {
		for _, group := range namespace.groups {
			if group.authID == authID {
				matcher.removeGroupLocked(group)
			}
		}
	}
}

func (matcher *MerklePrefixMatcher) Clear() {
	if matcher == nil {
		return
	}
	matcher.mu.Lock()
	matcher.namespaces = make(map[string]*lcpNamespace)
	matcher.lru = list.New()
	matcher.lruElements = make(map[*lcpGroup]*list.Element)
	matcher.groupCount = 0
	matcher.prefixCount = 0
	matcher.mu.Unlock()
}

// LookupSession reports distinct active auth bindings without refreshing TTL.
func (matcher *MerklePrefixMatcher) LookupSession(sessionID string) ([]string, string, bool) {
	if matcher == nil || sessionID == "" {
		return nil, "", false
	}
	matcher.mu.Lock()
	defer matcher.mu.Unlock()
	now := matcher.now()
	var active []*lcpGroup
	var expired []*lcpGroup
	for _, namespace := range matcher.namespaces {
		for _, group := range namespace.groups {
			if group == nil || group.sessionID != sessionID {
				continue
			}
			if now.Before(group.expiresAt) {
				active = append(active, group)
			} else {
				expired = append(expired, group)
			}
		}
	}
	for _, group := range expired {
		matcher.removeGroupLocked(group)
	}
	if len(active) == 0 {
		return nil, "", false
	}
	seen := make(map[string]struct{}, len(active))
	authIDs := make([]string, 0, len(active))
	for _, group := range active {
		if group.authID == "" {
			continue
		}
		if _, exists := seen[group.authID]; exists {
			continue
		}
		seen[group.authID] = struct{}{}
		authIDs = append(authIDs, group.authID)
	}
	sort.Strings(authIDs)
	return authIDs, active[0].namespace, len(authIDs) > 0
}

func (matcher *MerklePrefixMatcher) sanitizeFingerprints(fingerprints []string, minimum int) ([]string, int, bool) {
	if len(fingerprints) == 0 || minimum <= 0 || minimum > len(fingerprints) {
		return nil, 0, false
	}
	maxTurns := matcher.maxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMatcherMaxTurns
	}
	if len(fingerprints) > maxTurns {
		fingerprints = fingerprints[:maxTurns]
		if minimum > len(fingerprints) {
			return nil, 0, false
		}
	}
	return fingerprints, minimum, true
}

func (matcher *MerklePrefixMatcher) fingerprints(turns []CanonicalTurn) []string {
	if len(turns) == 0 {
		return nil
	}
	limit := min(len(turns), matcher.maxTurns)
	fingerprints := make([]string, 0, limit)
	for _, turn := range turns[:limit] {
		fingerprints = append(fingerprints, FastTurnFingerprint(turn))
	}
	return fingerprints
}

func (matcher *MerklePrefixMatcher) prepareLocked() {
	if matcher.namespaces == nil {
		matcher.namespaces = make(map[string]*lcpNamespace)
	}
	if matcher.lru == nil {
		matcher.lru = list.New()
	}
	if matcher.lruElements == nil {
		matcher.lruElements = make(map[*lcpGroup]*list.Element)
	}
	matcher.operations++
	if matcher.operations%128 == 0 {
		matcher.cleanupLocked(matcher.now())
	}
}

func (matcher *MerklePrefixMatcher) namespaceLocked(namespace string) *lcpNamespace {
	value := matcher.namespaces[namespace]
	if value == nil {
		value = &lcpNamespace{
			groups:   make(map[string]*lcpGroup),
			prefixes: make(map[string]map[string]*lcpGroup),
		}
		matcher.namespaces[namespace] = value
	}
	return value
}

func (matcher *MerklePrefixMatcher) touchLocked(
	namespace string,
	fingerprints []string,
	minimum int,
	authID string,
	now time.Time,
) bool {
	groups := matcher.namespaceLocked(namespace)
	key := sequenceKey(fingerprints)
	if existing := groups.groups[key]; existing != nil {
		if !now.Before(existing.expiresAt) {
			matcher.removeGroupLocked(existing)
			matcher.bindLocked(namespace, fingerprints, minimum, authID, now)
			return true
		}
		if existing.authID != authID {
			return false
		}
		existing.expiresAt = now.Add(matcher.ttl)
		existing.lastAccessNumber = matcher.nextAccessNumberLocked()
		if element := matcher.lruElements[existing]; element != nil {
			matcher.lru.MoveToBack(element)
		}
		return true
	}
	matcher.bindLocked(namespace, fingerprints, minimum, authID, now)
	return true
}

func (matcher *MerklePrefixMatcher) bindLocked(
	namespace string,
	fingerprints []string,
	minimum int,
	authID string,
	now time.Time,
) MerklePrefixBindResult {
	groups := matcher.namespaceLocked(namespace)
	key := sequenceKey(fingerprints)
	if existing := groups.groups[key]; existing != nil {
		if now.Before(existing.expiresAt) {
			result := MerklePrefixBindResult{
				SessionID:       existing.sessionID,
				ParentSessionID: existing.parentSessionID,
				IsFork:          existing.parentSessionID != "",
			}
			prefixKeys := append([]string(nil), existing.prefixKeys...)
			matcher.removeGroupLocked(existing)
			rebound := &lcpGroup{
				key:             key,
				namespace:       namespace,
				authID:          authID,
				sessionID:       result.SessionID,
				parentSessionID: result.ParentSessionID,
				minPrefixLength: minimum,
				fingerprints:    append([]string(nil), fingerprints...),
				prefixKeys:      prefixKeys,
				expiresAt:       now.Add(matcher.ttl),
			}
			matcher.addGroupLocked(rebound)
			result.AccessNumber = rebound.lastAccessNumber
			return result
		}
		matcher.removeGroupLocked(existing)
	}

	result := MerklePrefixBindResult{}
	if match, ok := matcher.matchLocked(namespace, fingerprints, minimum, now); ok {
		result.SessionID = match.SessionID
		result.ParentSessionID = match.ParentSessionID
		result.IsFork = match.IsFork
	}
	prefixKeys := rollingPrefixKeys(fingerprints)
	if result.SessionID == "" {
		index := 0
		if minimum > 0 && minimum <= len(prefixKeys) {
			index = minimum - 1
		}
		result.SessionID = newLCPSessionID(namespace, prefixKeys[index])
	}
	group := &lcpGroup{
		key:             key,
		namespace:       namespace,
		authID:          authID,
		sessionID:       result.SessionID,
		parentSessionID: result.ParentSessionID,
		minPrefixLength: minimum,
		fingerprints:    append([]string(nil), fingerprints...),
		prefixKeys:      prefixKeys,
		expiresAt:       now.Add(matcher.ttl),
	}
	matcher.addGroupLocked(group)
	result.AccessNumber = group.lastAccessNumber
	return result
}

func (matcher *MerklePrefixMatcher) addGroupLocked(group *lcpGroup) {
	namespace := matcher.namespaceLocked(group.namespace)
	if len(group.prefixKeys) == 0 {
		group.prefixKeys = rollingPrefixKeys(group.fingerprints)
	}
	namespace.groups[group.key] = group
	for _, prefix := range group.prefixKeys {
		bucket := namespace.prefixes[prefix]
		if bucket == nil {
			bucket = make(map[string]*lcpGroup)
			namespace.prefixes[prefix] = bucket
		}
		bucket[group.key] = group
	}
	group.lastAccessNumber = matcher.nextAccessNumberLocked()
	matcher.lruElements[group] = matcher.lru.PushBack(group)
	matcher.groupCount++
	matcher.prefixCount += len(group.prefixKeys)
	for matcher.groupCount > matcher.maxGroups || matcher.prefixCount > matcher.maxPrefixes {
		oldest := matcher.lru.Front()
		if oldest == nil {
			return
		}
		oldGroup, _ := oldest.Value.(*lcpGroup)
		if oldGroup == nil {
			matcher.lru.Remove(oldest)
			continue
		}
		matcher.removeGroupLocked(oldGroup)
	}
}

func (matcher *MerklePrefixMatcher) removeGroupLocked(group *lcpGroup) {
	if group == nil {
		return
	}
	namespace := matcher.namespaces[group.namespace]
	if namespace != nil {
		if namespace.groups[group.key] == group {
			delete(namespace.groups, group.key)
		}
		for _, prefix := range group.prefixKeys {
			bucket := namespace.prefixes[prefix]
			delete(bucket, group.key)
			if len(bucket) == 0 {
				delete(namespace.prefixes, prefix)
			}
		}
		if len(namespace.groups) == 0 {
			delete(matcher.namespaces, group.namespace)
		}
	}
	if element := matcher.lruElements[group]; element != nil {
		matcher.lru.Remove(element)
		delete(matcher.lruElements, group)
	}
	if matcher.groupCount > 0 {
		matcher.groupCount--
	}
	matcher.prefixCount = max(0, matcher.prefixCount-len(group.prefixKeys))
}

func (matcher *MerklePrefixMatcher) matchLocked(
	namespace string,
	fingerprints []string,
	minimum int,
	now time.Time,
) (MerklePrefixMatch, bool) {
	groups := matcher.namespaces[namespace]
	if groups == nil || len(fingerprints) == 0 || minimum <= 0 || minimum > len(fingerprints) {
		return MerklePrefixMatch{}, false
	}
	prefixKeys := rollingPrefixKeys(fingerprints)
	low, high := minimum, len(fingerprints)
	var best *lcpGroup
	bestLength := 0
	for low <= high {
		middle := low + (high-low)/2
		candidate := newestMatchingGroup(groups.prefixes[prefixKeys[middle-1]], fingerprints[:middle], now)
		if candidate == nil {
			high = middle - 1
			continue
		}
		best = candidate
		bestLength = middle
		low = middle + 1
	}
	if best == nil {
		return MerklePrefixMatch{}, false
	}
	best.expiresAt = now.Add(matcher.ttl)
	best.lastAccessNumber = matcher.nextAccessNumberLocked()
	if element := matcher.lruElements[best]; element != nil {
		matcher.lru.MoveToBack(element)
	}

	result := MerklePrefixMatch{
		AuthID:          best.authID,
		SessionID:       best.sessionID,
		ParentSessionID: best.parentSessionID,
		PrefixLength:    bestLength,
		AccessNumber:    best.lastAccessNumber,
	}
	if bestLength < len(best.fingerprints) && len(fingerprints) > bestLength {
		result.IsFork = true
		result.ParentSessionID = newLCPSessionID(namespace, prefixKeys[bestLength-1])
		result.SessionID = newLCPSessionID(namespace, prefixKeys[bestLength])
	}
	return result, true
}

func newestMatchingGroup(bucket map[string]*lcpGroup, fingerprints []string, now time.Time) *lcpGroup {
	var best *lcpGroup
	for _, group := range bucket {
		if group == nil || !now.Before(group.expiresAt) || group.minPrefixLength > len(fingerprints) ||
			len(group.fingerprints) < len(fingerprints) || !equalStrings(group.fingerprints[:len(fingerprints)], fingerprints) {
			continue
		}
		if best == nil || len(group.fingerprints) > len(best.fingerprints) ||
			(len(group.fingerprints) == len(best.fingerprints) &&
				(group.lastAccessNumber > best.lastAccessNumber ||
					(group.lastAccessNumber == best.lastAccessNumber && group.expiresAt.After(best.expiresAt)))) {
			best = group
		}
	}
	return best
}

func (matcher *MerklePrefixMatcher) nextAccessNumberLocked() uint64 {
	matcher.accessCounter++
	return matcher.accessCounter
}

func (matcher *MerklePrefixMatcher) cleanupLocked(now time.Time) {
	for _, namespace := range matcher.namespaces {
		for _, group := range namespace.groups {
			if !now.Before(group.expiresAt) {
				matcher.removeGroupLocked(group)
			}
		}
	}
}

func minimumAffinityPrefixLength(turns []CanonicalTurn) int {
	for index, turn := range turns {
		if canonicalRole(turn.Role) != "system" {
			return index + 1
		}
	}
	return 0
}

func rollingPrefixKeys(fingerprints []string) []string {
	keys := make([]string, 0, len(fingerprints))
	var previous [32]byte
	for index, fingerprint := range fingerprints {
		hash := sha256.New()
		_, _ = hash.Write(previous[:])
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(fingerprint))
		for offset, value := range hash.Sum(nil) {
			previous[offset] = value
		}
		keys = append(keys, strconv.Itoa(index+1)+":"+hex.EncodeToString(previous[:]))
	}
	return keys
}

func sequenceKey(fingerprints []string) string {
	keys := rollingPrefixKeys(fingerprints)
	if len(keys) == 0 {
		return ""
	}
	return keys[len(keys)-1]
}

func newLCPSessionID(namespace, prefix string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:lcp-session:v1\x00" + namespace + "\x00" + prefix))
	return "lcp:v1:" + hex.EncodeToString(sum[:])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

package helps

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	ConnectFlagData       byte = 0x00
	ConnectFlagCompressed byte = 0x01
	ConnectFlagEndStream  byte = 0x02

	DevinDefaultBaseURL       = "https://server.codeium.com"
	DevinChatPath             = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	DevinDefaultClientName    = "chisel"
	DevinDefaultClientVersion = "3000.10.21"
	DevinFingerprintHexLen    = 732
	DevinDefaultMaxTokens     = 128000

	maxConnectFrameSize      = 16 * 1024 * 1024
	maxDecompressedFrameSize = 64 * 1024 * 1024
	defaultDevinTurnCounters = 5000
)

// DevinTool represents a tool definition in a GetChatMessage request.
type DevinTool struct {
	Name        string
	Description string
	Parameters  []byte
}

// DevinToolCall represents one complete tool call in request history or a response log.
type DevinToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// DevinToolCallDelta represents one streamed tool call fragment.
type DevinToolCallDelta struct {
	ID        string
	Name      string
	Arguments string
	Index     int
}

// DevinImage represents an image attachment in request history.
type DevinImage struct {
	Base64Data string
	MimeType   string
}

// DevinPrompt represents a single history turn.
type DevinPrompt struct {
	MessageID     string
	Source        int
	Content       string
	Images        []DevinImage
	ToolCalls     []DevinToolCall
	ToolCallID    string
	Thinking      string
	Signature     []byte
	SignatureType string
}

// DevinFrameResult contains data decoded from one Connect-proto response frame.
type DevinFrameResult struct {
	OutputID                string
	Timestamp               uint64
	ContentText             string
	DeltaTokens             uint64
	StopReason              uint64
	ToolCallDeltas          []DevinToolCallDelta
	ThinkingText            string
	DeltaSignature          []byte
	DeltaSignatureType      string
	Latency                 float64
	MessageID               string
	Usage                   *DevinUsage
	ResponseDimensionGroups [][]byte
	UnknownFieldNumbers     []int
}

var devinSessionTurns = cache.NewBoundedLRU[string, *atomic.Uint64](defaultDevinTurnCounters, nil)

// GenerateDevinDeviceFingerprint returns the 732-character device fingerprint used by Devin CLI metadata.
func GenerateDevinDeviceFingerprint(seed string) string {
	if seed == "" {
		var random [DevinFingerprintHexLen / 2]byte
		if _, err := rand.Read(random[:]); err == nil {
			return hex.EncodeToString(random[:])
		}
		seed = uuid.NewString()
	}

	var result strings.Builder
	for counter := 0; result.Len() < DevinFingerprintHexLen; counter++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed, counter)))
		result.WriteString(hex.EncodeToString(digest[:]))
	}
	return result.String()[:DevinFingerprintHexLen]
}

// GenerateDevinSentryTrace returns a Sentry trace header compatible with the native CLI.
func GenerateDevinSentryTrace() string {
	var random [24]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:16]) + "-" + hex.EncodeToString(random[16:]) + "-1"
	}
	return strings.ReplaceAll(uuid.NewString(), "-", "") + "-" +
		strings.ReplaceAll(uuid.NewString(), "-", "")[:16] + "-1"
}

// NextDevinSessionTurnIndex returns the process-local zero-based request ordinal for a session.
func NextDevinSessionTurnIndex(sessionID string) int {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0
	}
	counter := devinSessionTurns.GetOrAdd(sessionID, func() *atomic.Uint64 {
		return &atomic.Uint64{}
	})
	return int(counter.Add(1) - 1)
}

// ResetDevinSessionTurnIndex clears the request ordinal for a session.
func ResetDevinSessionTurnIndex(sessionID string) {
	devinSessionTurns.Delete(strings.TrimSpace(sessionID))
}

// WrapConnectEnvelope wraps protobuf bytes in a Connect protocol data frame.
func WrapConnectEnvelope(protoBytes []byte) []byte {
	return WrapConnectEnvelopeWithFlag(ConnectFlagData, protoBytes)
}

// WrapConnectEnvelopeWithFlag wraps payload bytes in a Connect protocol frame.
func WrapConnectEnvelopeWithFlag(flag byte, protoBytes []byte) []byte {
	framed := make([]byte, 5, 5+len(protoBytes))
	framed[0] = flag
	binary.BigEndian.PutUint32(framed[1:], uint32(len(protoBytes)))
	return append(framed, protoBytes...)
}

// ReadConnectFrame reads and validates one bounded Connect protocol frame.
func ReadConnectFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}

	flag := header[0]
	if flag != ConnectFlagData &&
		flag != ConnectFlagCompressed &&
		flag != ConnectFlagEndStream &&
		flag != ConnectFlagCompressed|ConnectFlagEndStream {
		return flag, nil, fmt.Errorf("invalid connect frame flag: 0x%02x", flag)
	}

	length := binary.BigEndian.Uint32(header[1:])
	if length > maxConnectFrameSize {
		return flag, nil, fmt.Errorf(
			"connect frame length %d exceeds maximum limit %d",
			length,
			maxConnectFrameSize,
		)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return flag, nil, err
	}
	if flag&ConnectFlagCompressed == 0 {
		return flag, payload, nil
	}

	compressed, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return flag, nil, fmt.Errorf("decompress gzip connect frame: %w", err)
	}
	defer compressed.Close()

	initialCapacity := int(length) * 4
	if initialCapacity < 4096 {
		initialCapacity = 4096
	}
	if initialCapacity > 4*1024*1024 {
		initialCapacity = 4 * 1024 * 1024
	}
	decoded := bytes.NewBuffer(make([]byte, 0, initialCapacity))
	if _, err = decoded.ReadFrom(io.LimitReader(compressed, maxDecompressedFrameSize+1)); err != nil {
		return flag, nil, fmt.Errorf("read decompressed connect frame: %w", err)
	}
	if decoded.Len() > maxDecompressedFrameSize {
		return flag, nil, fmt.Errorf(
			"decompressed frame size exceeds maximum limit %d",
			maxDecompressedFrameSize,
		)
	}
	return flag, decoded.Bytes(), nil
}

func appendDevinStringField(dst []byte, number protowire.Number, value string) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendString(dst, value)
}

func appendDevinBytesField(dst []byte, number protowire.Number, value []byte) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}

func appendDevinVarintField(dst []byte, number protowire.Number, value uint64) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

// BuildDevinClientMetadataBytes builds the protobuf client metadata submessage.
func BuildDevinClientMetadataBytes(sessionToken, deviceSeed, osName string) []byte {
	if osName == "" {
		osName = runtime.GOOS
	}
	fingerprint := GenerateDevinDeviceFingerprint(deviceSeed)
	metadata := make([]byte, 0, 1024)
	metadata = appendDevinStringField(metadata, 1, "devin-cli")
	metadata = appendDevinStringField(metadata, 2, DevinDefaultClientVersion)
	metadata = appendDevinStringField(metadata, 3, sessionToken)
	metadata = appendDevinStringField(metadata, 4, "en")
	metadata = appendDevinStringField(metadata, 5, osName)
	metadata = appendDevinStringField(metadata, 7, DevinDefaultClientVersion)
	metadata = appendDevinStringField(metadata, 12, DevinDefaultClientName)
	metadata = appendDevinStringField(metadata, 28, DevinDefaultClientName)
	metadata = appendDevinStringField(metadata, 31, fingerprint)
	return metadata
}

// BuildDevinGetChatMessageRequest encodes a GetChatMessage protobuf request.
func BuildDevinGetChatMessageRequest(
	sessionToken string,
	deviceSeed string,
	chatModelUID string,
	systemPrompt string,
	prompts []DevinPrompt,
	tools []DevinTool,
	temperature *float64,
	maxTokens int,
	sessionID string,
	cascadeID string,
	matcher *SensitiveWordMatcher,
) []byte {
	if maxTokens <= 0 {
		maxTokens = DevinDefaultMaxTokens
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	if cascadeID == "" {
		cascadeID = sessionID
	}

	estimatedSize := 4096 + len(systemPrompt)
	for _, prompt := range prompts {
		estimatedSize += 256 + len(prompt.Content)
	}
	request := make([]byte, 0, estimatedSize)
	request = appendDevinBytesField(
		request,
		1,
		BuildDevinClientMetadataBytes(sessionToken, deviceSeed, runtime.GOOS),
	)

	if sanitized := SanitizeDevinSystemPrompt(systemPrompt, matcher); sanitized != "" {
		request = appendDevinStringField(request, 2, sanitized)
	}

	for _, prompt := range prompts {
		messageID := prompt.MessageID
		if messageID == "" {
			messageID = uuid.NewString()
		}
		source := prompt.Source
		if source <= 0 {
			source = 1
		}

		item := make([]byte, 0, 256+len(prompt.Content))
		item = appendDevinStringField(item, 1, messageID)
		item = appendDevinVarintField(item, 2, uint64(source))
		item = appendDevinStringField(item, 3, prompt.Content)
		for _, call := range prompt.ToolCalls {
			toolCall := make([]byte, 0, len(call.ID)+len(call.Name)+len(call.Arguments)+16)
			if call.ID != "" {
				toolCall = appendDevinStringField(toolCall, 1, call.ID)
			}
			if call.Name != "" {
				toolCall = appendDevinStringField(toolCall, 2, call.Name)
			}
			if call.Arguments != "" {
				toolCall = appendDevinStringField(toolCall, 3, call.Arguments)
			}
			item = appendDevinBytesField(item, 6, toolCall)
		}
		if prompt.ToolCallID != "" {
			item = appendDevinStringField(item, 7, prompt.ToolCallID)
		}
		for _, image := range prompt.Images {
			data := strings.TrimSpace(image.Base64Data)
			if data == "" {
				continue
			}
			mimeType := strings.TrimSpace(image.MimeType)
			if mimeType == "" {
				mimeType = "image/png"
			}
			encoded := appendDevinStringField(nil, 1, data)
			encoded = appendDevinStringField(encoded, 2, mimeType)
			item = appendDevinBytesField(item, 10, encoded)
		}
		if prompt.Thinking != "" {
			item = appendDevinStringField(item, 11, prompt.Thinking)
		}
		if len(prompt.Signature) > 0 {
			item = appendDevinBytesField(item, 12, prompt.Signature)
		}
		if prompt.SignatureType != "" {
			item = appendDevinStringField(item, 18, prompt.SignatureType)
		}
		request = appendDevinBytesField(request, 3, item)
	}

	request = appendDevinVarintField(request, 7, 5)
	completion := appendDevinVarintField(nil, 1, 1)
	completion = appendDevinVarintField(completion, 2, uint64(maxTokens))
	completion = appendDevinVarintField(completion, 3, 400)
	temperatureValue := 1.0
	if temperature != nil {
		temperatureValue = *temperature
	}
	completion = protowire.AppendTag(completion, 5, protowire.Fixed64Type)
	completion = protowire.AppendFixed64(completion, math.Float64bits(temperatureValue))
	completion = appendDevinVarintField(completion, 7, 40)
	completion = protowire.AppendTag(completion, 8, protowire.Fixed64Type)
	completion = protowire.AppendFixed64(completion, math.Float64bits(float64(float32(0.95))))
	request = appendDevinBytesField(request, 8, completion)

	for _, tool := range tools {
		encoded := make([]byte, 0, len(tool.Name)+len(tool.Description)+len(tool.Parameters)+16)
		if tool.Name != "" {
			encoded = appendDevinStringField(encoded, 1, tool.Name)
		}
		if tool.Description != "" {
			encoded = appendDevinStringField(encoded, 2, tool.Description)
		}
		if len(tool.Parameters) > 0 {
			encoded = appendDevinBytesField(encoded, 3, tool.Parameters)
		}
		request = appendDevinBytesField(request, 10, encoded)
	}

	turnIndex := NextDevinSessionTurnIndex(sessionID)
	session := appendDevinStringField(nil, 1, sessionID)
	if turnIndex > 0 {
		session = appendDevinVarintField(session, 2, uint64(turnIndex))
	}
	session = appendDevinVarintField(session, 3, 4)
	if len(prompts) > 0 && prompts[len(prompts)-1].Source == 1 &&
		(turnIndex == 0 || len(prompts) < 2 || prompts[len(prompts)-2].Source != 1) {
		session = appendDevinVarintField(session, 4, 14)
	}
	request = appendDevinBytesField(request, 15, session)
	request = appendDevinStringField(request, 16, cascadeID)
	request = appendDevinVarintField(request, 20, 1)
	request = appendDevinStringField(request, 21, chatModelUID)
	return request
}

func validateDevinFrameWireType(number protowire.Number, wireType protowire.Type) error {
	var valid bool
	switch number {
	case 1, 3, 6, 7, 9, 10, 17, 21, 28:
		valid = wireType == protowire.BytesType
	case 2:
		valid = wireType == protowire.VarintType || wireType == protowire.BytesType
	case 4, 5:
		valid = wireType == protowire.VarintType
	case 12:
		valid = wireType == protowire.Fixed64Type
	default:
		return nil
	}
	if !valid {
		return fmt.Errorf("unexpected wire type %d", wireType)
	}
	return nil
}

// ParseDevinFrame decodes one protobuf response frame.
func ParseDevinFrame(payload []byte) (DevinFrameResult, error) {
	var result DevinFrameResult
	var textParts []string
	var thinkingParts []string

	for position := 0; position < len(payload); {
		number, wireType, consumed := protowire.ConsumeTag(payload[position:])
		if consumed <= 0 {
			return result, fmt.Errorf("consume tag at offset %d: %w", position, protowire.ParseError(consumed))
		}
		position += consumed
		if err := validateDevinFrameWireType(number, wireType); err != nil {
			return result, fmt.Errorf("field %d at offset %d: %w", number, position-consumed, err)
		}

		switch wireType {
		case protowire.VarintType:
			value, size := protowire.ConsumeVarint(payload[position:])
			if size <= 0 {
				return result, fmt.Errorf("consume varint at offset %d: %w", position, protowire.ParseError(size))
			}
			position += size
			switch number {
			case 2:
				result.Timestamp = value
			case 4:
				result.DeltaTokens = value
			case 5:
				result.StopReason = value
			}
		case protowire.Fixed64Type:
			value, size := protowire.ConsumeFixed64(payload[position:])
			if size <= 0 {
				return result, fmt.Errorf("consume fixed64 at offset %d: %w", position, protowire.ParseError(size))
			}
			position += size
			if number == 12 {
				result.Latency = math.Float64frombits(value)
			}
		case protowire.Fixed32Type:
			_, size := protowire.ConsumeFixed32(payload[position:])
			if size <= 0 {
				return result, fmt.Errorf("consume fixed32 at offset %d: %w", position, protowire.ParseError(size))
			}
			position += size
		case protowire.BytesType:
			value, size := protowire.ConsumeBytes(payload[position:])
			if size <= 0 {
				return result, fmt.Errorf("consume bytes at offset %d: %w", position, protowire.ParseError(size))
			}
			position += size
			switch number {
			case 1:
				result.OutputID = string(value)
			case 2:
				timestamp, err := parseDevinTimestamp(value)
				if err != nil {
					return result, fmt.Errorf("parse timestamp: %w", err)
				}
				result.Timestamp = timestamp
			case 3:
				textParts = append(textParts, string(value))
			case 6:
				toolCall, err := parseDevinToolCallDelta(value)
				if err != nil {
					return result, fmt.Errorf("parse tool call delta: %w", err)
				}
				result.ToolCallDeltas = append(result.ToolCallDeltas, toolCall)
			case 7:
				usage, err := parseDevinUsageField(value)
				if err != nil {
					return result, fmt.Errorf("parse usage: %w", err)
				}
				if result.Usage == nil {
					result.Usage = usage
				} else {
					mergeDevinUsage(result.Usage, usage)
				}
			case 9:
				thinkingParts = append(thinkingParts, string(value))
			case 10:
				result.DeltaSignature = append(result.DeltaSignature, value...)
			case 17:
				result.MessageID = string(value)
			case 21:
				result.DeltaSignatureType = string(value)
			case 28:
				result.ResponseDimensionGroups = append(
					result.ResponseDimensionGroups,
					append([]byte(nil), value...),
				)
			default:
				result.UnknownFieldNumbers = append(result.UnknownFieldNumbers, int(number))
			}
		default:
			return result, fmt.Errorf("unsupported wire type %d at offset %d", wireType, position)
		}
	}

	result.ContentText = strings.Join(textParts, "")
	result.ThinkingText = strings.Join(thinkingParts, "")
	return result, nil
}

func parseDevinToolCallDelta(data []byte) (DevinToolCallDelta, error) {
	var result DevinToolCallDelta
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return result, protowire.ParseError(consumed)
		}
		position += consumed
		if (number >= 1 && number <= 3 && wireType != protowire.BytesType) ||
			(number == 4 && wireType != protowire.VarintType) {
			return result, fmt.Errorf("tool call field %d has unexpected wire type %d", number, wireType)
		}
		switch wireType {
		case protowire.VarintType:
			value, size := protowire.ConsumeVarint(data[position:])
			if size <= 0 {
				return result, protowire.ParseError(size)
			}
			position += size
			if number == 4 {
				if value > uint64(math.MaxInt) {
					return result, fmt.Errorf("tool call index overflows int: %d", value)
				}
				result.Index = int(value)
			}
		case protowire.BytesType:
			value, size := protowire.ConsumeBytes(data[position:])
			if size <= 0 {
				return result, protowire.ParseError(size)
			}
			position += size
			switch number {
			case 1:
				result.ID = string(value)
			case 2:
				result.Name = string(value)
			case 3:
				result.Arguments = string(value)
			}
		default:
			size := protowire.ConsumeFieldValue(number, wireType, data[position:])
			if size <= 0 {
				return result, protowire.ParseError(size)
			}
			position += size
		}
	}
	return result, nil
}

func parseDevinTimestamp(data []byte) (uint64, error) {
	var seconds uint64
	for position := 0; position < len(data); {
		number, wireType, consumed := protowire.ConsumeTag(data[position:])
		if consumed <= 0 {
			return 0, protowire.ParseError(consumed)
		}
		position += consumed
		if number == 1 && wireType != protowire.VarintType {
			return 0, fmt.Errorf("timestamp field 1 has unexpected wire type %d", wireType)
		}
		if wireType != protowire.VarintType {
			size := protowire.ConsumeFieldValue(number, wireType, data[position:])
			if size <= 0 {
				return 0, protowire.ParseError(size)
			}
			position += size
			continue
		}
		value, size := protowire.ConsumeVarint(data[position:])
		if size <= 0 {
			return 0, protowire.ParseError(size)
		}
		position += size
		if number == 1 {
			seconds = value
		}
	}
	return seconds, nil
}

// ParseDevinTrailerError validates a Connect end-stream trailer and maps its error status.
func ParseDevinTrailerError(payload []byte) (int, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
		return 0, nil
	}

	var trailer struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &trailer); err != nil {
		return http.StatusBadGateway, fmt.Errorf("invalid Devin Connect trailer: %w", err)
	}
	if trailer.Error == nil {
		return http.StatusBadGateway, fmt.Errorf("invalid Devin Connect trailer: missing error object")
	}
	if strings.TrimSpace(trailer.Error.Code) == "" {
		return http.StatusBadGateway, fmt.Errorf("invalid Devin Connect trailer: missing error code")
	}

	code := strings.ToLower(strings.TrimSpace(trailer.Error.Code))
	message := strings.TrimSpace(trailer.Error.Message)
	messageLower := strings.ToLower(message)
	statusCode := http.StatusBadGateway
	switch code {
	case "invalid_argument":
		if !strings.Contains(messageLower, "internal error") {
			statusCode = http.StatusBadRequest
		}
	case "internal":
		statusCode = http.StatusBadGateway
	case "unauthenticated":
		statusCode = http.StatusUnauthorized
	case "permission_denied":
		statusCode = http.StatusForbidden
	case "resource_exhausted":
		statusCode = http.StatusTooManyRequests
	case "unavailable":
		statusCode = http.StatusServiceUnavailable
	case "canceled":
		statusCode = 499
	case "deadline_exceeded":
		statusCode = http.StatusGatewayTimeout
	case "failed_precondition":
		if strings.Contains(messageLower, "quota") ||
			strings.Contains(messageLower, "credit") ||
			strings.Contains(messageLower, "acu") ||
			strings.Contains(messageLower, "exhausted") ||
			strings.Contains(messageLower, "limit") {
			statusCode = http.StatusTooManyRequests
		} else {
			statusCode = http.StatusBadRequest
		}
	}
	return statusCode, fmt.Errorf("devin upstream error (%s): %s", trailer.Error.Code, message)
}

// UTF8SplitBuffer retains an incomplete trailing rune between response frames.
type UTF8SplitBuffer struct {
	remainder []byte
	invalid   bool
}

// Feed returns all complete UTF-8 text in chunk and retains an incomplete trailing rune.
func (buffer *UTF8SplitBuffer) Feed(chunk []byte) string {
	combined := append(append([]byte(nil), buffer.remainder...), chunk...)
	buffer.remainder = nil
	if len(combined) == 0 {
		return ""
	}

	validUntil := 0
	for validUntil < len(combined) {
		runeValue, size := utf8.DecodeRune(combined[validUntil:])
		if runeValue == utf8.RuneError && size == 1 {
			if !utf8.FullRune(combined[validUntil:]) {
				break
			}
			buffer.invalid = true
			validUntil++
			continue
		}
		validUntil += size
	}
	buffer.remainder = append(buffer.remainder, combined[validUntil:]...)
	return strings.ToValidUTF8(string(combined[:validUntil]), "�")
}

// Finish validates that the stream did not end in the middle of a UTF-8 rune.
func (buffer *UTF8SplitBuffer) Finish() error {
	if len(buffer.remainder) > 0 {
		return fmt.Errorf("truncated UTF-8 sequence at end of Devin stream")
	}
	if buffer.invalid {
		return fmt.Errorf("invalid UTF-8 sequence in Devin stream")
	}
	return nil
}

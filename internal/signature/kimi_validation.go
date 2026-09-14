package signature

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	// kimiThinkingSignatureNonStreamingLen is the raw signature length in non-streaming responses.
	kimiThinkingSignatureNonStreamingLen = 12946
	// kimiThinkingSignatureStreamingLen is the raw signature length in signature_delta events.
	kimiThinkingSignatureStreamingLen = 4340
	// minKimiThinkingSignatureEntropyRatio rejects same-length repetitive input.
	minKimiThinkingSignatureEntropyRatio = 0.85
)

type kimiThinkingSignatureMode string

const (
	kimiThinkingSignatureModeNonStreaming kimiThinkingSignatureMode = "non_streaming"
	kimiThinkingSignatureModeStreaming    kimiThinkingSignatureMode = "streaming"
)

type kimiThinkingSignatureInfo struct {
	RawLen     int
	DecodedLen int
	Mode       kimiThinkingSignatureMode
}

// inspectKimiThinkingSignature checks the fixed transport shape emitted by Kimi.
// Kimi has no self-describing envelope, so callers must run this after the
// Claude, Gemini and GPT probes to avoid claiming a foreign signature by length.
func inspectKimiThinkingSignature(raw string) (*kimiThinkingSignatureInfo, error) {
	sig := strings.TrimSpace(raw)
	if sig == "" {
		return nil, fmt.Errorf("empty Kimi thinking signature")
	}
	if sig != raw {
		return nil, fmt.Errorf("kimi thinking signature has leading or trailing whitespace")
	}

	var mode kimiThinkingSignatureMode
	switch len(sig) {
	case kimiThinkingSignatureNonStreamingLen:
		mode = kimiThinkingSignatureModeNonStreaming
	case kimiThinkingSignatureStreamingLen:
		mode = kimiThinkingSignatureModeStreaming
	default:
		return nil, fmt.Errorf("invalid Kimi thinking signature: unexpected length %d", len(sig))
	}
	if strings.Contains(sig, "=") {
		return nil, fmt.Errorf("invalid Kimi thinking signature: expected unpadded standard base64")
	}
	if index, r, ok := firstInvalidGrokEncryptedContentChar(sig); ok {
		return nil, fmt.Errorf(
			"invalid Kimi thinking signature: contains non-base64 character U+%04X at byte %d",
			r,
			index,
		)
	}
	if strings.HasPrefix(sig, "gAAAA") {
		return nil, fmt.Errorf("kimi thinking signature looks like GPT/Codex reasoning signature")
	}
	if isValidClaudeCAISSignature(sig) || isValidClaudeSignature(sig, true) {
		return nil, fmt.Errorf("kimi thinking signature looks like Claude thinking signature")
	}
	if isRecognizedGeminiSignature(sig) {
		return nil, fmt.Errorf("kimi thinking signature looks like Gemini thoughtSignature")
	}

	decoded, err := base64.RawStdEncoding.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("invalid Kimi thinking signature: base64 decode failed: %w", err)
	}
	if entropyRatio := byteEntropyRatio(decoded); entropyRatio < minKimiThinkingSignatureEntropyRatio {
		return nil, fmt.Errorf(
			"invalid Kimi thinking signature: decoded payload entropy ratio %.3f below %.3f",
			entropyRatio,
			minKimiThinkingSignatureEntropyRatio,
		)
	}
	return &kimiThinkingSignatureInfo{
		RawLen:     len(sig),
		DecodedLen: len(decoded),
		Mode:       mode,
	}, nil
}

func isValidKimiThinkingSignature(raw string) bool {
	_, err := inspectKimiThinkingSignature(raw)
	return err == nil
}

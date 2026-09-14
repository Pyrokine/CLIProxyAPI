package signature

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"
)

const (
	maxGrokEncryptedContentLen = 8 * 1024 * 1024
	// minGrokEncryptedContentDecodedLen keeps headroom below the shortest observed 43-byte native payload.
	// Entropy remains the primary shape check.
	minGrokEncryptedContentDecodedLen   = 32
	minGrokEncryptedContentEntropyRatio = 0.85
)

type GrokEncryptedContentInfo struct {
	RawLen     int
	DecodedLen int
}

func InspectGrokEncryptedContent(raw string) (*GrokEncryptedContentInfo, error) {
	sig := strings.TrimSpace(raw)
	if sig == "" {
		return nil, fmt.Errorf("empty grok encrypted_content")
	}
	if len(sig) > maxGrokEncryptedContentLen {
		return nil, fmt.Errorf("grok encrypted_content exceeds maximum length (%d bytes)", maxGrokEncryptedContentLen)
	}
	if sig != raw {
		return nil, fmt.Errorf("grok encrypted_content has leading or trailing whitespace")
	}
	if strings.HasPrefix(sig, "gAAAA") {
		return nil, fmt.Errorf("grok encrypted_content looks like GPT/Codex reasoning signature")
	}
	if strings.Contains(sig, "=") {
		return nil, fmt.Errorf("invalid grok encrypted_content: expected unpadded standard base64")
	}
	if index, r, ok := firstInvalidGrokEncryptedContentChar(sig); ok {
		return nil, fmt.Errorf(
			"invalid grok encrypted_content: contains non-base64 character U+%04X at byte %d", r, index,
		)
	}
	if isValidClaudeCAISSignature(sig) || isValidClaudeSignature(sig, true) {
		return nil, fmt.Errorf("grok encrypted_content looks like Claude thinking signature")
	}
	if isRecognizedGeminiSignature(sig) {
		return nil, fmt.Errorf("grok encrypted_content looks like Gemini thoughtSignature")
	}
	if isValidKimiThinkingSignature(sig) {
		return nil, fmt.Errorf("grok encrypted_content has a Kimi thinking signature length")
	}

	decoded, err := decodeGrokEncryptedContent(sig)
	if err != nil {
		return nil, err
	}
	if len(decoded) < minGrokEncryptedContentDecodedLen {
		return nil, fmt.Errorf("invalid grok encrypted_content: decoded payload too short (%d bytes)", len(decoded))
	}
	if entropyRatio := byteEntropyRatio(decoded); entropyRatio < minGrokEncryptedContentEntropyRatio {
		return nil, fmt.Errorf(
			"invalid grok encrypted_content: decoded payload entropy ratio %.3f below %.3f", entropyRatio,
			minGrokEncryptedContentEntropyRatio,
		)
	}
	return &GrokEncryptedContentInfo{RawLen: len(sig), DecodedLen: len(decoded)}, nil
}

func isValidGrokEncryptedContent(raw string) bool {
	_, err := InspectGrokEncryptedContent(raw)
	return err == nil
}

func decodeGrokEncryptedContent(sig string) ([]byte, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("invalid grok encrypted_content: base64 decode failed: %w", err)
	}
	return decoded, nil
}

func firstInvalidGrokEncryptedContentChar(sig string) (int, rune, bool) {
	for index, r := range sig {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '+' || r == '/':
		default:
			return index, r, true
		}
	}
	return 0, 0, false
}

func byteEntropyRatio(buf []byte) float64 {
	if len(buf) == 0 {
		return 0
	}
	var counts [256]int
	for _, b := range buf {
		counts[b]++
	}
	n := float64(len(buf))
	entropy := 0.0
	for _, count := range counts {
		if count == 0 {
			continue
		}
		p := float64(count) / n
		entropy -= p * math.Log2(p)
	}
	maxSymbols := min(len(buf), 256)
	if maxSymbols <= 1 {
		return 0
	}
	return entropy / math.Log2(float64(maxSymbols))
}

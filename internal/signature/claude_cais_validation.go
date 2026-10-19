package signature

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	claudeCAISSignatureMarker = 0x08
	claudeCAISModelTextPrefix = "claude-"
)

type claudeCAISSignatureInfo struct {
	FirstByte            byte
	EnvelopeVersion      uint64
	Infrastructure       *uint64
	ChannelID            uint64
	ModelText            string
	BlockKind            string
	ContextID            string
	SignatureLen         int
	SignatureInContainer bool
}

func isValidClaudeCAISSignature(rawSignature string) bool {
	_, err := inspectClaudeCAISSignature(rawSignature)
	return err == nil
}

func inspectClaudeCAISSignature(rawSignature string) (*claudeCAISSignatureInfo, error) {
	sig := PayloadWithoutProviderPrefix(rawSignature)
	if sig == "" {
		return nil, fmt.Errorf("empty signature")
	}
	if len(sig) > maxSignatureLen {
		return nil, fmt.Errorf("signature exceeds maximum length (%d bytes)", maxSignatureLen)
	}
	if sig[0] != 'C' {
		return nil, fmt.Errorf("invalid Claude CAIS signature: expected 'C' prefix, got %q", string(sig[0]))
	}

	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("invalid Claude CAIS signature: base64 decode failed: %w", err)
	}
	return inspectClaudeCAISPayload(decoded, false)
}

func isValidAntigravityClaudeCAQSSignature(rawSignature string) bool {
	if !strings.HasPrefix(rawSignature, "Q") {
		return false
	}
	_, err := inspectAntigravityClaudeCAQSSignature(rawSignature)
	return err == nil
}

func inspectAntigravityClaudeCAQSSignature(rawSignature string) (*claudeCAISSignatureInfo, error) {
	sig := PayloadWithoutProviderPrefix(rawSignature)
	if len(sig) == 0 || len(sig) > maxSignatureLen || sig[0] != 'Q' || strings.ContainsAny(sig, "\r\n") {
		return nil, fmt.Errorf("invalid Antigravity CAQS wrapper")
	}
	inner, err := base64.StdEncoding.Strict().DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("invalid Antigravity CAQS encoding: %w", err)
	}
	innerText := string(inner)
	if len(innerText) == 0 || innerText[0] != 'C' || strings.ContainsAny(innerText, " \t\r\n#") {
		return nil, fmt.Errorf("invalid Antigravity CAQS inner encoding")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(innerText)
	if err != nil {
		return nil, fmt.Errorf("invalid Antigravity CAQS inner encoding: %w", err)
	}
	info, err := inspectClaudeCAISPayload(decoded, true)
	if err != nil {
		return nil, fmt.Errorf("invalid Antigravity CAQS payload: %w", err)
	}
	if info.EnvelopeVersion != 4 || info.ChannelID != 18 || info.Infrastructure == nil ||
		*info.Infrastructure != 2 || info.BlockKind != "thinking" || !info.SignatureInContainer {
		return nil, fmt.Errorf("unsupported Antigravity CAQS envelope or channel schema")
	}
	return info, nil
}

func inspectClaudeCAISPayload(decoded []byte, rejectDuplicateMessages bool) (*claudeCAISSignatureInfo, error) {
	if len(decoded) == 0 {
		return nil, fmt.Errorf("invalid Claude CAIS signature: empty after decode")
	}
	if decoded[0] != claudeCAISSignatureMarker {
		return nil, fmt.Errorf(
			"invalid Claude CAIS signature: expected first byte 0x%02x, got 0x%02x",
			claudeCAISSignatureMarker,
			decoded[0],
		)
	}

	info := &claudeCAISSignatureInfo{FirstByte: decoded[0]}
	var container []byte
	haveEnvelopeVersion := false
	haveContainer := false
	err := walkClaudeProtobufFields(
		decoded, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			switch num {
			case 1:
				value, errField := decodeClaudeCAISVarint(raw, typ, "CAIS top-level field 1 envelope version")
				if errField != nil {
					return errField
				}
				info.EnvelopeVersion = value
				haveEnvelopeVersion = true
			case 2:
				if rejectDuplicateMessages && haveContainer {
					return fmt.Errorf("invalid Antigravity CAQS signature: duplicate container")
				}
				haveContainer = true
				value, errField := decodeClaudeCAISBytes(raw, typ, "CAIS top-level field 2 container")
				if errField != nil {
					return errField
				}
				container = value
			case 3:
				if _, errField := decodeClaudeCAISVarint(raw, typ, "CAIS top-level field 3 trailer"); errField != nil {
					return errField
				}
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if !haveEnvelopeVersion {
		return nil, fmt.Errorf("invalid Claude CAIS signature: missing top-level field 1 envelope version")
	}
	if container == nil {
		return nil, fmt.Errorf("invalid Claude CAIS signature: missing top-level field 2 container")
	}

	var channelBlock, containerSignatureBytes []byte
	haveChannelBlock := false
	err = walkClaudeProtobufFields(
		container, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			switch num {
			case 1:
				if rejectDuplicateMessages && haveChannelBlock {
					return fmt.Errorf("invalid Antigravity CAQS signature: duplicate channel block")
				}
				haveChannelBlock = true
				value, errField := decodeClaudeCAISBytes(raw, typ, "CAIS container field 1 channel block")
				if errField != nil {
					return errField
				}
				channelBlock = value
			case 5:
				value, errField := decodeClaudeCAISBytes(raw, typ, "CAIS container field 5 signature bytes")
				if errField != nil {
					return errField
				}
				containerSignatureBytes = value
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if channelBlock == nil {
		return nil, fmt.Errorf("invalid Claude CAIS signature: missing container field 1 channel block")
	}

	var haveChannelID, haveSignatureBytes, haveModelText bool
	err = walkClaudeProtobufFields(
		channelBlock, func(num protowire.Number, typ protowire.Type, raw []byte) error {
			switch num {
			case 1:
				value, errField := decodeClaudeCAISVarint(raw, typ, "CAIS channel field 1 channel_id")
				if errField != nil {
					return errField
				}
				info.ChannelID = value
				haveChannelID = true
			case 2:
				value, errField := decodeClaudeCAISVarint(raw, typ, "CAIS channel field 2 infrastructure")
				if errField != nil {
					return errField
				}
				info.Infrastructure = &value
			case 3:
				if _, errField := decodeClaudeCAISVarint(raw, typ, "CAIS channel field 3 version"); errField != nil {
					return errField
				}
			case 5:
				value, errField := decodeClaudeCAISBytes(raw, typ, "CAIS channel field 5 signature bytes")
				if errField != nil {
					return errField
				}
				if len(value) == 0 {
					return fmt.Errorf("invalid Claude CAIS signature: channel field 5 signature bytes must not be empty")
				}
				info.SignatureLen = len(value)
				haveSignatureBytes = true
			case 6:
				value, errField := decodeClaudeCAISUTF8(raw, typ, "CAIS channel field 6 model_text")
				if errField != nil {
					return errField
				}
				if !strings.HasPrefix(value, claudeCAISModelTextPrefix) {
					return fmt.Errorf(
						"invalid Claude CAIS signature: channel field 6 model_text must start with %q, got %q",
						claudeCAISModelTextPrefix,
						value,
					)
				}
				info.ModelText = value
				haveModelText = true
			case 7:
				if _, errField := decodeClaudeCAISVarint(raw, typ, "CAIS channel field 7"); errField != nil {
					return errField
				}
			case 8:
				value, errField := decodeClaudeCAISUTF8(raw, typ, "CAIS channel field 8 block kind")
				if errField != nil {
					return errField
				}
				info.BlockKind = value
			case 11:
				value, errField := decodeClaudeCAISUTF8(raw, typ, "CAIS channel field 11 context id")
				if errField != nil {
					return errField
				}
				if !isASCIIUUIDBytes([]byte(value)) {
					return fmt.Errorf(
						"invalid Claude CAIS signature: channel field 11 context id must be a canonical UUID, got %q",
						value,
					)
				}
				info.ContextID = value
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	if !haveSignatureBytes && info.EnvelopeVersion >= 4 && len(containerSignatureBytes) > 0 {
		info.SignatureLen = len(containerSignatureBytes)
		info.SignatureInContainer = true
		haveSignatureBytes = true
	}
	switch {
	case !haveChannelID:
		return nil, fmt.Errorf("invalid Claude CAIS signature: missing channel field 1 channel_id")
	case !haveSignatureBytes:
		return nil, fmt.Errorf("invalid Claude CAIS signature: missing signature bytes")
	case !haveModelText && info.EnvelopeVersion < 4:
		return nil, fmt.Errorf("invalid Claude CAIS signature: missing channel field 6 model_text")
	case info.EnvelopeVersion >= 4 && info.BlockKind != "thinking" && info.BlockKind != "narration":
		return nil, fmt.Errorf(
			"invalid Claude CAQS signature: expected block kind %q or %q, got %q",
			"thinking",
			"narration",
			info.BlockKind,
		)
	}

	return info, nil
}

func decodeClaudeCAISVarint(raw []byte, typ protowire.Type, label string) (uint64, error) {
	if typ != protowire.VarintType {
		return 0, fmt.Errorf("invalid Claude CAIS signature: %s must be varint", label)
	}
	return decodeClaudeVarintField(raw, label)
}

func decodeClaudeCAISBytes(raw []byte, typ protowire.Type, label string) ([]byte, error) {
	if typ != protowire.BytesType {
		return nil, fmt.Errorf("invalid Claude CAIS signature: %s must be bytes", label)
	}
	return decodeClaudeBytesField(raw, label)
}

func decodeClaudeCAISUTF8(raw []byte, typ protowire.Type, label string) (string, error) {
	value, err := decodeClaudeCAISBytes(raw, typ, label)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(value) {
		return "", fmt.Errorf("invalid Claude CAIS signature: %s must be valid UTF-8", label)
	}
	return string(value), nil
}

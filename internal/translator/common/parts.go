package common

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// UnsupportedPartError rejects a user turn whose attachment cannot reach the provider.
type UnsupportedPartError struct {
	Type string
}

func (e *UnsupportedPartError) Error() string {
	if e == nil || e.Type == "" {
		return "unsupported content part"
	}
	return fmt.Sprintf("unsupported content part: %s", e.Type)
}

func (*UnsupportedPartError) StatusCode() int { return 400 }

func (*UnsupportedPartError) IsRequestScoped() bool { return true }

// UserTurnDrops remembers an attachment that leaves any individual user turn empty.
type UserTurnDrops struct {
	turn  string
	first string
}

func (d *UserTurnDrops) Drop(partType string) {
	if d.turn == "" {
		d.turn = partType
	}
}

func (d *UserTurnDrops) EndTurn(sendable int) {
	if d.turn != "" && sendable == 0 && d.first == "" {
		d.first = d.turn
	}
	d.turn = ""
}

func (d *UserTurnDrops) Err() error {
	if d.first == "" {
		return nil
	}
	return &UnsupportedPartError{Type: d.first}
}

// RejectDroppedInteractionsUserAttachments checks each consecutive user run against its target part converter.
// preferNativeParts matches targets that consume parts instead of content when both are present.
func RejectDroppedInteractionsUserAttachments(input gjson.Result, preferNativeParts bool, convertPart func(gjson.Result, bool) []byte) error {
	var drops UserTurnDrops
	sendable := 0
	endTurn := func() {
		drops.EndTurn(sendable)
		sendable = 0
	}
	var visit func(gjson.Result, string)
	visit = func(step gjson.Result, defaultRole string) {
		if step.IsArray() {
			for _, item := range step.Array() {
				visit(item, defaultRole)
			}
			return
		}
		if step.Type == gjson.String {
			if defaultRole == "user" && strings.TrimSpace(step.String()) != "" {
				sendable++
			} else if defaultRole != "user" {
				endTurn()
			}
			return
		}
		if !step.IsObject() {
			return
		}
		role := defaultRole
		explicitRole := strings.ToLower(strings.TrimSpace(step.Get("role").String()))
		if explicitRole != "" {
			role = explicitRole
		}
		stepType := strings.ToLower(strings.TrimSpace(step.Get("type").String()))
		if stepType == "developer" || stepType == "system" {
			role = "developer"
		}
		if role != "user" {
			endTurn()
		}
		if steps := step.Get("steps"); steps.IsArray() {
			for _, item := range steps.Array() {
				visit(item, role)
			}
			return
		}
		switch stepType {
		case "model_output", "assistant", "thought", "reasoning", "function_call":
			endTurn()
			return
		case "function_result", "function_call_output":
			if role == "user" {
				sendable++
			}
			return
		}
		if role != "user" {
			return
		}
		originalContent := step.Get("content")
		parts := step.Get("parts")
		content := originalContent
		native := parts.Exists() && (preferNativeParts || !originalContent.Exists())
		if native {
			content = parts
		}
		turnSendable := 0
		check := func(part gjson.Result) {
			partType := interactionsAttachmentType(part)
			if !native || parts.IsArray() {
				converted := convertPart(part, native)
				if len(converted) > 0 {
					if text := gjson.GetBytes(converted, "text"); !text.Exists() || strings.TrimSpace(text.String()) != "" {
						turnSendable++
						return
					}
				}
			}
			if partType != "" {
				drops.Drop(partType)
			}
		}
		switch {
		case content.IsArray():
			for _, part := range content.Array() {
				check(part)
			}
		case content.IsObject():
			check(content)
		case content.Type == gjson.String && strings.TrimSpace(content.String()) != "":
			turnSendable++
		}
		if !preferNativeParts && parts.Exists() && originalContent.Exists() && turnSendable == 0 {
			if parts.IsArray() {
				for _, part := range parts.Array() {
					if partType := interactionsAttachmentType(part); partType != "" {
						drops.Drop(partType)
					}
				}
			} else if partType := interactionsAttachmentType(parts); partType != "" {
				drops.Drop(partType)
			}
		}
		if !originalContent.Exists() && !preferNativeParts && strings.TrimSpace(step.Get("text").String()) != "" {
			turnSendable++
		}
		if !content.Exists() && interactionsAttachmentType(step) != "" {
			drops.Drop(interactionsAttachmentType(step))
		}
		sendable += turnSendable
	}
	visit(input, "user")
	endTurn()
	return drops.Err()
}

// GeminiPartIsSendable reports whether a Gemini part contains nonblank text or another sendable item.
func GeminiPartIsSendable(part []byte) bool {
	parsed := gjson.ParseBytes(part)
	text := parsed.Get("text")
	if !text.Exists() || strings.TrimSpace(text.String()) != "" {
		return true
	}
	for _, key := range []string{"functionCall", "functionResponse", "inlineData", "inline_data", "fileData", "file_data"} {
		if parsed.Get(key).Exists() {
			return true
		}
	}
	return false
}

// CountSendableGeminiParts counts items the provider can read in a user turn.
func CountSendableGeminiParts(parts [][]byte) int {
	count := 0
	for _, part := range parts {
		if GeminiPartIsSendable(part) {
			count++
		}
	}
	return count
}

func interactionsAttachmentType(part gjson.Result) string {
	partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
	if partType != "" && partType != "text" {
		return partType
	}
	switch {
	case part.Get("inline_data").Exists(), part.Get("inlineData").Exists():
		return "inline_data"
	case part.Get("file_data").Exists(), part.Get("fileData").Exists():
		return "file_data"
	}
	return ""
}

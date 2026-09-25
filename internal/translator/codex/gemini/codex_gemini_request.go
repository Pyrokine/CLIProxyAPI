// Package gemini provides request translation functionality for Codex to Gemini API compatibility.
// It handles parsing and transforming Codex API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Codex API format and Gemini API's expected format.
package gemini

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToCodex parses and transforms a Gemini API request into Codex API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Codex API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and generation configuration extraction
// 2. System instruction conversion to Codex format
// 3. Message content conversion with proper role mapping
// 4. Tool call and tool result handling with FIFO queue for ID matching
// 5. Tool declaration and tool choice configuration mapping
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Gemini API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Codex API format
func ConvertGeminiRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	// Base template
	out := []byte(`{"model":"","instructions":"","input":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Pre-compute tool name shortening map from declared functionDeclarations
	shortMap := map[string]string{}
	if tools := root.Get("tools"); tools.IsArray() {
		var names []string
		toolArray := tools.Array()
		for i := range toolArray {
			fns := toolArray[i].Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			for _, fn := range fns.Array() {
				if v := fn.Get("name"); v.Exists() {
					names = append(names, v.String())
				}
			}
		}
		if len(names) > 0 {
			shortMap = buildShortNameMap(names)
		}
	}

	var toolCalls translatorcommon.GeminiToolCallQueue
	toolCallCounter := 0
	newToolCallID := func() string {
		toolCallCounter++
		return fmt.Sprintf("call_gemini_%016d", toolCallCounter)
	}

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// System instruction -> as a user message with input_text parts
	sysParts := root.Get("system_instruction.parts")
	if !sysParts.IsArray() {
		sysParts = root.Get("systemInstruction.parts")
	}
	if sysParts.IsArray() {
		msg := []byte(`{"type":"message","role":"developer","content":[]}`)
		arr := sysParts.Array()
		for i := range arr {
			p := arr[i]
			if translatorcommon.IsGeminiThoughtPart(p) {
				continue
			}
			if t := p.Get("text"); t.Exists() {
				part := []byte(`{}`)
				part, _ = sjson.SetBytes(part, "type", "input_text")
				part, _ = sjson.SetBytes(part, "text", t.String())
				msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
			}
		}
		if len(gjson.GetBytes(msg, "content").Array()) > 0 {
			out, _ = sjson.SetRawBytes(out, "input.-1", msg)
		}
	}

	// Contents -> messages and function calls/results
	contents := root.Get("contents")
	if contents.IsArray() {
		items := contents.Array()
		for i := range items {
			item := items[i]
			role := item.Get("role").String()
			if role == "model" {
				role = "assistant"
			}

			parts := item.Get("parts")
			if !parts.IsArray() {
				continue
			}
			parr := parts.Array()
			for j := range parr {
				p := parr[j]
				if translatorcommon.IsGeminiThoughtPart(p) {
					continue
				}

				// text part
				if t := p.Get("text"); t.Exists() {
					msg := []byte(`{"type":"message","role":"","content":[]}`)
					msg, _ = sjson.SetBytes(msg, "role", role)
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := []byte(`{}`)
					part, _ = sjson.SetBytes(part, "type", partType)
					part, _ = sjson.SetBytes(part, "text", t.String())
					msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
					out, _ = sjson.SetRawBytes(out, "input.-1", msg)
					continue
				}

				// function call from model
				if fc := p.Get("functionCall"); fc.Exists() {
					fn := []byte(`{"type":"function_call"}`)
					functionName := fc.Get("name").String()
					providerName := functionName
					if short, ok := shortMap[providerName]; ok {
						providerName = short
					} else {
						providerName = shortenNameIfNeeded(providerName)
					}
					fn, _ = sjson.SetBytes(fn, "name", providerName)
					if args := fc.Get("args"); args.Exists() {
						fn, _ = sjson.SetBytes(fn, "arguments", args.Raw)
					}
					id := translatorcommon.GeminiToolCallID(fc)
					if id == "" {
						id = newToolCallID()
					}
					toolCalls.Add(functionName, id)
					fn, _ = sjson.SetBytes(fn, "call_id", id)
					out, _ = sjson.SetRawBytes(out, "input.-1", fn)
					continue
				}

				// function response from user
				if fr := p.Get("functionResponse"); fr.Exists() {
					fno := []byte(`{"type":"function_call_output"}`)
					// Prefer a string result if present; otherwise embed the raw response as a string
					if res := fr.Get("response.result"); res.Exists() {
						fno, _ = sjson.SetBytes(fno, "output", res.String())
					} else if resp := fr.Get("response"); resp.Exists() {
						fno, _ = sjson.SetBytes(fno, "output", resp.Raw)
					}
					functionName := fr.Get("name").String()
					id := toolCalls.Match(functionName, translatorcommon.GeminiToolCallID(fr))
					if id == "" {
						id = newToolCallID()
					}
					fno, _ = sjson.SetBytes(fno, "call_id", id)
					out, _ = sjson.SetRawBytes(out, "input.-1", fno)
					continue
				}
			}
		}
	}

	// Tools mapping: Gemini functionDeclarations -> Codex tools
	tools := root.Get("tools")
	if tools.IsArray() {
		out, _ = sjson.SetRawBytes(out, "tools", []byte(`[]`))
		out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		toolArray := tools.Array()
		for i := range toolArray {
			td := toolArray[i]
			fns := td.Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			farr := fns.Array()
			for j := range farr {
				fn := farr[j]
				tool := []byte(`{}`)
				tool, _ = sjson.SetBytes(tool, "type", "function")
				if v := fn.Get("name"); v.Exists() {
					name := v.String()
					if short, ok := shortMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
					tool, _ = sjson.SetBytes(tool, "name", name)
				}
				if v := fn.Get("description"); v.Exists() {
					tool, _ = sjson.SetBytes(tool, "description", v.String())
				}
				if prm := fn.Get("parameters"); prm.Exists() {
					cleaned := cleanGeminiCodexToolParameters(prm)
					tool, _ = sjson.SetRawBytes(tool, "parameters", cleaned)
				} else if prm = fn.Get("parametersJsonSchema"); prm.Exists() {
					cleaned := cleanGeminiCodexToolParameters(prm)
					tool, _ = sjson.SetRawBytes(tool, "parameters", cleaned)
				}
				tool, _ = sjson.SetBytes(tool, "strict", false)
				out, _ = sjson.SetRawBytes(out, "tools.-1", tool)
			}
		}
	}

	// Fixed flags aligning with Codex expectations
	out, _ = sjson.SetBytes(out, "parallel_tool_calls", true)
	out = setCodexToolChoiceFromGeminiToolConfig(out, root.Get("toolConfig.functionCallingConfig"))

	// Convert Gemini thinkingConfig to Codex reasoning.effort.
	// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
	effortSet := false
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
					effortSet = true
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToLevel(int(thinkingBudget.Int())); ok {
						out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
						effortSet = true
					}
				}
			}
		}
	}
	if !effortSet {
		// No thinking config, set default effort
		out, _ = sjson.SetBytes(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.SetBytes(out, "stream", true)
	out, _ = sjson.SetBytes(out, "store", false)
	out, _ = sjson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	var pathsToLower []string
	toolsResult := gjson.GetBytes(out, "tools")
	util.Walk(toolsResult, "", "type", &pathsToLower)
	for _, p := range pathsToLower {
		fullPath := fmt.Sprintf("tools.%s", p)
		typeValue := gjson.GetBytes(out, fullPath)
		if typeValue.Type != gjson.String {
			continue
		}
		normalizedType := strings.ToLower(typeValue.String())
		if normalizedType == typeValue.String() {
			continue
		}
		out, _ = sjson.SetBytes(out, fullPath, normalizedType)
	}

	return out
}

func setCodexToolChoiceFromGeminiToolConfig(out []byte, functionCallingConfig gjson.Result) []byte {
	if !functionCallingConfig.Exists() {
		return out
	}
	mode := functionCallingConfig.Get("mode").String()
	switch mode {
	case "NONE":
		out, _ = sjson.SetBytes(out, "tool_choice", "none")
	case "AUTO":
		current := gjson.GetBytes(out, "tool_choice")
		if current.Type != gjson.String || current.String() != "auto" {
			out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		}
	case "ANY":
		allowedNames := functionCallingConfig.Get("allowedFunctionNames")
		allowedNameItems := allowedNames.Array()
		if allowedNames.IsArray() && len(allowedNameItems) == 1 {
			choice := []byte(`{"type":"function","name":""}`)
			choice, _ = sjson.SetBytes(choice, "name", shortenNameIfNeeded(allowedNameItems[0].String()))
			out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
		} else {
			out, _ = sjson.SetBytes(out, "tool_choice", "required")
		}
	}
	return out
}

func cleanGeminiCodexToolParameters(parameters gjson.Result) []byte {
	cleaned := []byte(parameters.Raw)
	if parameters.Get("$schema").Exists() {
		cleaned, _ = sjson.DeleteBytes(cleaned, "$schema")
	}
	if parameters.Get("additionalProperties").Type != gjson.False {
		cleaned, _ = sjson.SetBytes(cleaned, "additionalProperties", false)
	}
	return cleaned
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				candidate := "mcp__" + n[idx+2:]
				if len(candidate) > limit {
					candidate = candidate[:limit]
				}
				return candidate
			}
		}
		return n[:limit]
	}

	makeUnique := func(candidate string) string {
		if _, ok := used[candidate]; !ok {
			return candidate
		}
		base := candidate
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := max(limit-len(suffix), 0)
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		candidate := baseCandidate(n)
		uniq := makeUnique(candidate)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

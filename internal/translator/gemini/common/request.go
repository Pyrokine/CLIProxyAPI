// Package common provides shared request translation logic for all Gemini API variants.
// It implements a configurable OpenAI-to-Gemini request converter that handles the common
// transformation logic while allowing per-variant behavioral differences via RequestConfig.
package common

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// functionThoughtSignature is the marker used to skip thought signature validation on function call parts.
const functionThoughtSignature = "skip_thought_signature_validator"

// RequestConfig controls the behavioral differences between Gemini API request format variants.
type RequestConfig struct {
	// BaseEnvelope is the initial JSON skeleton for the output.
	BaseEnvelope string
	// PathPrefix is prepended to all content paths (e.g., "request." for CLI variants, "" for direct Gemini).
	PathPrefix string
	// SystemInstructionKey is the JSON key name for system instructions ("systemInstruction" or "system_instruction").
	SystemInstructionKey string
	// MimeTypeField is the inline data MIME type field name ("mimeType" or "mime_type").
	MimeTypeField string
	// SupportMaxTokens enables mapping OpenAI max_tokens to generationConfig.maxOutputTokens.
	SupportMaxTokens bool
	// IncludeFunctionIDs includes functionCall.id and functionResponse.id in the output.
	IncludeFunctionIDs bool
	// ValidateFunctionArgs validates function arguments as JSON with a non-JSON fallback.
	ValidateFunctionArgs bool
	// ParseFunctionResponse parses function response content as JSON with non-JSON handling.
	ParseFunctionResponse bool
	// CheckTextEmptiness skips setting text parts when the text is empty.
	CheckTextEmptiness bool
}

// ConvertOpenAIRequest converts an OpenAI Chat Completions request (raw JSON) into a Gemini request JSON.
// The behavior adapts to the target API variant based on the provided RequestConfig.
func ConvertOpenAIRequest(modelName string, inputRawJSON []byte, cfg *RequestConfig) []byte {
	rawJSON := inputRawJSON
	out := []byte(cfg.BaseEnvelope)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	genConfigPath := cfg.PathPrefix + "generationConfig"

	// Let user-provided generationConfig pass through
	if genConfig := gjson.GetBytes(rawJSON, "generationConfig"); genConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, genConfigPath, []byte(genConfig.Raw))
	}

	// Apply thinking configuration: convert OpenAI reasoning_effort to Gemini thinkingConfig.
	// Inline translation-only mapping; capability checks happen later in ApplyThinking.
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
	if re.Exists() {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if effort != "" {
			thinkingPath := genConfigPath + ".thinkingConfig"
			if effort == "auto" {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingBudget", -1)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", true)
			} else {
				out, _ = sjson.SetBytes(out, thinkingPath+".thinkingLevel", effort)
				out, _ = sjson.SetBytes(out, thinkingPath+".includeThoughts", effort != "none")
			}
		}
	}

	// Temperature/top_p/top_k
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, genConfigPath+".temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, genConfigPath+".topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, genConfigPath+".topK", tkr.Num)
	}
	if cfg.SupportMaxTokens {
		if maxTok := gjson.GetBytes(rawJSON, "max_tokens"); maxTok.Exists() && maxTok.Type == gjson.Number {
			out, _ = sjson.SetBytes(out, genConfigPath+".maxOutputTokens", maxTok.Num)
		}
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == gjson.Number {
		if val := n.Int(); val > 1 {
			out, _ = sjson.SetBytes(out, genConfigPath+".candidateCount", val)
		}
	}

	// Map OpenAI modalities -> Gemini generationConfig.responseModalities
	if mods := gjson.GetBytes(rawJSON, "modalities"); mods.Exists() && mods.IsArray() {
		var responseMods []string
		for _, m := range mods.Array() {
			switch strings.ToLower(m.String()) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			}
		}
		if len(responseMods) > 0 {
			out, _ = sjson.SetBytes(out, genConfigPath+".responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, genConfigPath+".imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, genConfigPath+".imageConfig.imageSize", size.Str)
		}
	}

	contentsPath := cfg.PathPrefix + "contents"
	sysInstrPath := cfg.PathPrefix + cfg.SystemInstructionKey

	// messages -> systemInstruction + contents
	out = convertMessages(rawJSON, out, cfg, contentsPath, sysInstrPath)

	// tools -> tools[].functionDeclarations + tools[].googleSearch/codeExecution/urlContext passthrough
	out = convertTools(rawJSON, out, cfg.PathPrefix+"tools")

	return AttachDefaultSafetySettings(out, cfg.PathPrefix+"safetySettings")
}

// convertMessages transforms OpenAI messages into Gemini system instructions and contents.
func convertMessages(rawJSON, out []byte, cfg *RequestConfig, contentsPath, sysInstrPath string) []byte {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.IsArray() {
		return out
	}

	arr := messages.Array()

	// First pass: build assistant tool_calls id -> name map
	tcID2Name := map[string]string{}
	for _, m := range arr {
		if m.Get("role").String() == "assistant" {
			tcs := m.Get("tool_calls")
			if tcs.IsArray() {
				for _, tc := range tcs.Array() {
					if tc.Get("type").String() == "function" {
						id := tc.Get("id").String()
						name := tc.Get("function.name").String()
						if id != "" && name != "" {
							tcID2Name[id] = name
						}
					}
				}
			}
		}
	}

	// Second pass: build tool responses cache
	toolResponses := map[string]string{}
	for _, m := range arr {
		if m.Get("role").String() == "tool" {
			toolCallID := m.Get("tool_call_id").String()
			if toolCallID != "" {
				toolResponses[toolCallID] = m.Get("content").Raw
			}
		}
	}

	// Third pass: convert messages
	systemPartIndex := 0
	for _, m := range arr {
		role := m.Get("role").String()
		content := m.Get("content")

		switch {
		case (role == "system" || role == "developer") && len(arr) > 1:
			out, systemPartIndex = convertSystemMessage(out, content, sysInstrPath, systemPartIndex)

		case role == "user" || ((role == "system" || role == "developer") && len(arr) == 1):
			node := convertUserMessage(content, cfg)
			out, _ = sjson.SetRawBytes(out, contentsPath+".-1", node)

		case role == "assistant":
			out = convertAssistantMessage(out, m, content, cfg, contentsPath, tcID2Name, toolResponses)
		}
	}

	return out
}

// convertSystemMessage handles system/developer role messages as Gemini systemInstruction.
func convertSystemMessage(out []byte, content gjson.Result, sysInstrPath string, partIndex int) ([]byte, int) {
	if content.Type == gjson.String {
		out, _ = sjson.SetBytes(out, sysInstrPath+".role", "user")
		out, _ = sjson.SetBytes(
			out, fmt.Sprintf(sysInstrPath+".parts.%d.text", partIndex), content.String(),
		)
		partIndex++
	} else if content.IsObject() && content.Get("type").String() == "text" {
		out, _ = sjson.SetBytes(out, sysInstrPath+".role", "user")
		out, _ = sjson.SetBytes(
			out, fmt.Sprintf(sysInstrPath+".parts.%d.text", partIndex), content.Get("text").String(),
		)
		partIndex++
	} else if content.IsArray() {
		contents := content.Array()
		if len(contents) > 0 {
			out, _ = sjson.SetBytes(out, sysInstrPath+".role", "user")
			for _, c := range contents {
				out, _ = sjson.SetBytes(
					out, fmt.Sprintf(sysInstrPath+".parts.%d.text", partIndex), c.Get("text").String(),
				)
				partIndex++
			}
		}
	}
	return out, partIndex
}

// convertImageURLPart parses a data-URI image_url and appends an inlineData part to node.
// Returns the updated node and part index. If the URL is not a valid data-URI, node is unchanged.
func convertImageURLPart(node []byte, p int, imageURL string, cfg *RequestConfig) ([]byte, int) {
	if len(imageURL) <= 5 {
		return node, p
	}
	pieces := strings.SplitN(imageURL[5:], ";", 2)
	if len(pieces) != 2 || len(pieces[1]) <= 7 {
		return node, p
	}
	mime := pieces[0]
	data := pieces[1][7:]
	node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData."+cfg.MimeTypeField, mime)
	node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
	node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", functionThoughtSignature)
	return node, p + 1
}

// convertUserMessage builds a Gemini user content node from OpenAI user message content.
func convertUserMessage(content gjson.Result, cfg *RequestConfig) []byte {
	node := []byte(`{"role":"user","parts":[]}`)
	if content.Type == gjson.String {
		node, _ = sjson.SetBytes(node, "parts.0.text", content.String())
	} else if content.IsArray() {
		p := 0
		for _, item := range content.Array() {
			switch item.Get("type").String() {
			case "text":
				text := item.Get("text").String()
				if !cfg.CheckTextEmptiness || text != "" {
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", text)
				}
				p++
			case "image_url":
				node, p = convertImageURLPart(node, p, item.Get("image_url.url").String(), cfg)
			case "file":
				filename := item.Get("file.filename").String()
				fileData := item.Get("file.file_data").String()
				ext := ""
				if sp := strings.Split(filename, "."); len(sp) > 1 {
					ext = sp[len(sp)-1]
				}
				if mimeType, ok := misc.MimeTypes[ext]; ok {
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData."+cfg.MimeTypeField, mimeType)
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", fileData)
					p++
				} else {
					log.Warnf("Unknown file name extension '%s' in user message, skip", ext)
				}
			}
		}
	}
	return node
}

// convertAssistantMessage converts an OpenAI assistant message (text, multimodal, tool calls)
// into Gemini model content and tool response nodes.
func convertAssistantMessage(
	out []byte, m gjson.Result, content gjson.Result, cfg *RequestConfig,
	contentsPath string, tcID2Name, toolResponses map[string]string,
) []byte {
	node := []byte(`{"role":"model","parts":[]}`)
	p := 0
	if content.Type == gjson.String {
		text := content.String()
		if !cfg.CheckTextEmptiness || text != "" {
			node, _ = sjson.SetBytes(node, "parts.-1.text", text)
			p++
		}
	} else if content.IsArray() {
		for _, item := range content.Array() {
			switch item.Get("type").String() {
			case "text":
				text := item.Get("text").String()
				if !cfg.CheckTextEmptiness || text != "" {
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", text)
				}
				p++
			case "image_url":
				node, p = convertImageURLPart(node, p, item.Get("image_url.url").String(), cfg)
			}
		}
	}

	// Tool calls -> model content with functionCall parts + user content with functionResponse parts
	tcs := m.Get("tool_calls")
	if tcs.IsArray() {
		fIDs := make([]string, 0)
		for _, tc := range tcs.Array() {
			if tc.Get("type").String() != "function" {
				continue
			}
			fid := tc.Get("id").String()
			fname := tc.Get("function.name").String()
			fargs := tc.Get("function.arguments").String()
			if cfg.IncludeFunctionIDs {
				node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.id", fid)
			}
			node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.name", fname)
			if cfg.ValidateFunctionArgs {
				if gjson.Valid(fargs) {
					node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(fargs))
				} else {
					node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.args.params", []byte(fargs))
				}
			} else {
				node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(fargs))
			}
			node, _ = sjson.SetBytes(
				node, "parts."+itoa(p)+".thoughtSignature", functionThoughtSignature,
			)
			p++
			if fid != "" {
				fIDs = append(fIDs, fid)
			}
		}
		out, _ = sjson.SetRawBytes(out, contentsPath+".-1", node)

		// Append a single user content combining functionResponse per function
		toolNode := []byte(`{"role":"user","parts":[]}`)
		pp := 0
		for _, fid := range fIDs {
			if name, ok := tcID2Name[fid]; ok {
				if cfg.IncludeFunctionIDs {
					toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.id", fid)
				}
				toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.name", name)
				resp := toolResponses[fid]
				if resp == "" {
					resp = "{}"
				}
				if cfg.ParseFunctionResponse {
					if resp != "null" {
						parsed := gjson.Parse(resp)
						if parsed.Type == gjson.JSON {
							toolNode, _ = sjson.SetRawBytes(
								toolNode, "parts."+itoa(pp)+".functionResponse.response.result",
								[]byte(parsed.Raw),
							)
						} else {
							toolNode, _ = sjson.SetBytes(
								toolNode, "parts."+itoa(pp)+".functionResponse.response.result", resp,
							)
						}
					}
				} else {
					toolNode, _ = sjson.SetBytes(
						toolNode, "parts."+itoa(pp)+".functionResponse.response.result", []byte(resp),
					)
				}
				pp++
			}
		}
		if pp > 0 {
			out, _ = sjson.SetRawBytes(out, contentsPath+".-1", toolNode)
		}
	} else {
		out, _ = sjson.SetRawBytes(out, contentsPath+".-1", node)
	}

	return out
}

// convertTools transforms OpenAI tools definitions into Gemini format,
// including function declarations and native Gemini tool types (googleSearch, codeExecution, urlContext).
func convertTools(rawJSON, out []byte, toolsPath string) []byte {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return out
	}

	functionToolNode := []byte(`{}`)
	hasFunction := false
	googleSearchNodes := make([][]byte, 0)
	codeExecutionNodes := make([][]byte, 0)
	urlContextNodes := make([][]byte, 0)

	for _, t := range tools.Array() {
		if t.Get("type").String() == "function" {
			fn := t.Get("function")
			if fn.Exists() && fn.IsObject() {
				fnRaw := convertFunctionDeclaration(fn)
				if fnRaw == "" {
					continue
				}
				if !hasFunction {
					functionToolNode, _ = sjson.SetRawBytes(functionToolNode, "functionDeclarations", []byte("[]"))
				}
				tmp, errSet := sjson.SetRawBytes(functionToolNode, "functionDeclarations.-1", []byte(fnRaw))
				if errSet != nil {
					log.Warnf("Failed to append tool declaration for '%s': %v", fn.Get("name").String(), errSet)
					continue
				}
				functionToolNode = tmp
				hasFunction = true
			}
		}
		if gs := t.Get("google_search"); gs.Exists() {
			node := []byte(`{}`)
			var errSet error
			node, errSet = sjson.SetRawBytes(node, "googleSearch", []byte(gs.Raw))
			if errSet != nil {
				log.Warnf("Failed to set googleSearch tool: %v", errSet)
				continue
			}
			googleSearchNodes = append(googleSearchNodes, node)
		}
		if ce := t.Get("code_execution"); ce.Exists() {
			node := []byte(`{}`)
			var errSet error
			node, errSet = sjson.SetRawBytes(node, "codeExecution", []byte(ce.Raw))
			if errSet != nil {
				log.Warnf("Failed to set codeExecution tool: %v", errSet)
				continue
			}
			codeExecutionNodes = append(codeExecutionNodes, node)
		}
		if uc := t.Get("url_context"); uc.Exists() {
			node := []byte(`{}`)
			var errSet error
			node, errSet = sjson.SetRawBytes(node, "urlContext", []byte(uc.Raw))
			if errSet != nil {
				log.Warnf("Failed to set urlContext tool: %v", errSet)
				continue
			}
			urlContextNodes = append(urlContextNodes, node)
		}
	}

	if !hasFunction && len(googleSearchNodes) == 0 && len(codeExecutionNodes) == 0 && len(urlContextNodes) == 0 {
		return out
	}

	toolsNode := []byte("[]")
	if hasFunction {
		toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", functionToolNode)
	}
	for _, n := range googleSearchNodes {
		toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", n)
	}
	for _, n := range codeExecutionNodes {
		toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", n)
	}
	for _, n := range urlContextNodes {
		toolsNode, _ = sjson.SetRawBytes(toolsNode, "-1", n)
	}
	out, _ = sjson.SetRawBytes(out, toolsPath, toolsNode)
	return out
}

// convertFunctionDeclaration converts a single OpenAI function definition to Gemini format.
// Returns the raw JSON string, or empty string on failure.
func convertFunctionDeclaration(fn gjson.Result) string {
	fnRaw := fn.Raw
	fnName := fn.Get("name").String()

	if fn.Get("parameters").Exists() {
		renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
		if errRename != nil {
			log.Warnf("Failed to rename parameters for tool '%s': %v", fnName, errRename)
			var errSet error
			fnRaw, errSet = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
			if errSet != nil {
				log.Warnf("Failed to set default schema type for tool '%s': %v", fnName, errSet)
				return ""
			}
			fnRaw, errSet = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
			if errSet != nil {
				log.Warnf("Failed to set default schema properties for tool '%s': %v", fnName, errSet)
				return ""
			}
		} else {
			fnRaw = renamed
		}
	} else {
		var errSet error
		fnRaw, errSet = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
		if errSet != nil {
			log.Warnf("Failed to set default schema type for tool '%s': %v", fnName, errSet)
			return ""
		}
		fnRaw, errSet = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
		if errSet != nil {
			log.Warnf("Failed to set default schema properties for tool '%s': %v", fnName, errSet)
			return ""
		}
	}

	fnRaw, _ = sjson.Delete(fnRaw, "strict")
	return fnRaw
}

func itoa(i int) string { return strconv.Itoa(i) }

// functionCallGroup represents a group of function calls and their responses.
type functionCallGroup struct {
	ResponsesNeeded int
}

// parseFunctionResponseRaw attempts to normalize a function response part into a JSON object string.
// Falls back to a minimal "functionResponse" object when parsing fails.
func parseFunctionResponseRaw(response gjson.Result) string {
	if response.IsObject() && gjson.Valid(response.Raw) {
		return response.Raw
	}

	log.Debugf("parse function response failed, using fallback")
	funcResp := response.Get("functionResponse")
	if funcResp.Exists() {
		fr := `{"functionResponse":{"name":"","response":{"result":""}}}`
		fr, _ = sjson.Set(fr, "functionResponse.name", funcResp.Get("name").String())
		fr, _ = sjson.Set(fr, "functionResponse.response.result", funcResp.Get("response").String())
		if id := funcResp.Get("id").String(); id != "" {
			fr, _ = sjson.Set(fr, "functionResponse.id", id)
		}
		return fr
	}

	fr := `{"functionResponse":{"name":"unknown","response":{"result":""}}}`
	fr, _ = sjson.Set(fr, "functionResponse.response.result", response.String())
	return fr
}

// FixCLIToolResponse performs tool response format conversion and grouping.
// It transforms the CLI tool response format by grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
func FixCLIToolResponse(input string) (string, error) {
	parsed := gjson.Parse(input)
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		return input, fmt.Errorf("contents not found in input")
	}

	contentsWrapper := `{"contents":[]}`
	var pendingGroups []*functionCallGroup
	var collectedResponses []gjson.Result

	contents.ForEach(
		func(key, value gjson.Result) bool {
			role := value.Get("role").String()
			parts := value.Get("parts")

			// Check if this content has function responses
			var responsePartsInThisContent []gjson.Result
			parts.ForEach(
				func(_, part gjson.Result) bool {
					if part.Get("functionResponse").Exists() {
						responsePartsInThisContent = append(responsePartsInThisContent, part)
					}
					return true
				},
			)

			// If this content has function responses, collect them
			if len(responsePartsInThisContent) > 0 {
				collectedResponses = append(collectedResponses, responsePartsInThisContent...)

				// Check if any pending groups can be satisfied
				for i := len(pendingGroups) - 1; i >= 0; i-- {
					group := pendingGroups[i]
					if len(collectedResponses) >= group.ResponsesNeeded {
						groupResponses := collectedResponses[:group.ResponsesNeeded]
						collectedResponses = collectedResponses[group.ResponsesNeeded:]

						functionResponseContent := `{"parts":[],"role":"function"}`
						for _, response := range groupResponses {
							partRaw := parseFunctionResponseRaw(response)
							if partRaw != "" {
								functionResponseContent, _ = sjson.SetRaw(functionResponseContent, "parts.-1", partRaw)
							}
						}

						if gjson.Get(functionResponseContent, "parts.#").Int() > 0 {
							contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", functionResponseContent)
						}

						pendingGroups = append(pendingGroups[:i], pendingGroups[i+1:]...)
						break
					}
				}

				return true
			}

			// If this is a model with function calls, create a new group
			if role == "model" {
				functionCallsCount := 0
				parts.ForEach(
					func(_, part gjson.Result) bool {
						if part.Get("functionCall").Exists() {
							functionCallsCount++
						}
						return true
					},
				)

				if functionCallsCount > 0 {
					if !value.IsObject() {
						log.Warnf("failed to parse model content")
						return true
					}
					contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", value.Raw)

					group := &functionCallGroup{
						ResponsesNeeded: functionCallsCount,
					}
					pendingGroups = append(pendingGroups, group)
				} else {
					if !value.IsObject() {
						log.Warnf("failed to parse content")
						return true
					}
					contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", value.Raw)
				}
			} else {
				if !value.IsObject() {
					log.Warnf("failed to parse content")
					return true
				}
				contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", value.Raw)
			}

			return true
		},
	)

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]

			functionResponseContent := `{"parts":[],"role":"function"}`
			for _, response := range groupResponses {
				partRaw := parseFunctionResponseRaw(response)
				if partRaw != "" {
					functionResponseContent, _ = sjson.SetRaw(functionResponseContent, "parts.-1", partRaw)
				}
			}

			if gjson.Get(functionResponseContent, "parts.#").Int() > 0 {
				contentsWrapper, _ = sjson.SetRaw(contentsWrapper, "contents.-1", functionResponseContent)
			}
		}
	}

	result := input
	result, _ = sjson.SetRaw(result, "request.contents", gjson.Get(contentsWrapper, "contents").Raw)

	return result, nil
}

// RenameToolParameters renames parameters -> parametersJsonSchema in all
// tools[].function_declarations[].parameters entries of rawJSON.
func RenameToolParameters(rawJSON []byte) []byte {
	toolsResult := gjson.GetBytes(rawJSON, "tools")
	if !toolsResult.Exists() || !toolsResult.IsArray() {
		return rawJSON
	}
	for i := range toolsResult.Array() {
		fdPath := fmt.Sprintf("tools.%d.function_declarations", i)
		fdResult := gjson.GetBytes(rawJSON, fdPath)
		if !fdResult.Exists() || !fdResult.IsArray() {
			continue
		}
		for j := range fdResult.Array() {
			paramPath := fmt.Sprintf("%s.%d.parameters", fdPath, j)
			if gjson.GetBytes(rawJSON, paramPath).Exists() {
				renamed, _ := util.RenameKey(
					string(rawJSON), paramPath,
					fmt.Sprintf("%s.%d.parametersJsonSchema", fdPath, j),
				)
				rawJSON = []byte(renamed)
			}
		}
	}
	return rawJSON
}

// InjectThoughtSignatures walks contents and injects thoughtSignature on model-role parts
// that contain functionCall or an existing thoughtSignature. This is required by some
// Gemini API variants to bypass server-side thought signature validation.
func InjectThoughtSignatures(rawJSON []byte, contentsPath string) []byte {
	gjson.GetBytes(rawJSON, contentsPath).ForEach(
		func(key, content gjson.Result) bool {
			if content.Get("role").String() != "model" {
				return true
			}
			content.Get("parts").ForEach(
				func(partKey, part gjson.Result) bool {
					if part.Get("functionCall").Exists() || part.Get("thoughtSignature").Exists() {
						rawJSON, _ = sjson.SetBytes(
							rawJSON,
							fmt.Sprintf("%s.%d.parts.%d.thoughtSignature", contentsPath, key.Int(), partKey.Int()),
							functionThoughtSignature,
						)
					}
					return true
				},
			)
			return true
		},
	)
	return rawJSON
}


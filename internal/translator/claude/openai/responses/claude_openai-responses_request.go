package responses

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	user    = ""
	account = ""
	session = ""
)

// convertOpenAIResponsesRequestToClaudeDefault transforms an OpenAI Responses API request
// into a Claude Messages API request using only gjson/sjson for JSON handling.
// It supports:
// - instructions and system/developer inputs -> top-level system blocks
// - input[].type==message with input_text/output_text -> user/assistant messages
// - function_call -> assistant tool_use
// - function_call_output -> user tool_result
// - tools[].parameters -> tools[].input_schema
// - max_output_tokens -> max_tokens
// - stream passthrough via parameter
func convertOpenAIResponsesRequestToClaudeDefault(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIResponsesRequestToClaude(modelName, inputRawJSON, stream, false)
}

// ConvertOpenAIResponsesRequestToClaudeWithCompat preserves reasoning items
// with empty or provider-specific encrypted content for compatibility endpoints.
func ConvertOpenAIResponsesRequestToClaudeWithCompat(
	modelName string,
	inputRawJSON []byte,
	stream bool,
) []byte {
	return convertOpenAIResponsesRequestToClaude(modelName, inputRawJSON, stream, true)
}

func convertOpenAIResponsesRequestToClaude(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveThinkingBlocks bool,
) []byte {
	rawJSON := inputRawJSON

	if account == "" {
		u, _ := uuid.NewRandom()
		account = u.String()
	}
	if session == "" {
		u, _ := uuid.NewRandom()
		session = u.String()
	}
	if user == "" {
		sum := sha256.Sum256([]byte(account + session))
		user = hex.EncodeToString(sum[:])
	}
	userID := fmt.Sprintf("user_%s_account_%s_session_%s", user, account, session)

	// Base Claude message payload
	out := []byte(fmt.Sprintf(`{"model":"","max_tokens":32000,"messages":[],"metadata":{"user_id":"%s"}}`, userID))

	root := gjson.ParseBytes(rawJSON)
	toolIdentity := translatorcommon.NewResponsesToolIdentity(rawJSON)

	// Convert OpenAI Responses reasoning.effort to Claude thinking config.
	if v := root.Get("reasoning.effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			mi := registry.LookupModelInfo(modelName, "claude")
			supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
			supportsMax := supportsAdaptive && thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))

			// Claude 4.6 supports adaptive thinking with output_config.effort.
			// MapToClaudeEffort normalizes levels (e.g. minimal→low, xhigh→high) to avoid
			// validation errors since validate treats same-provider unsupported levels as errors.
			if supportsAdaptive {
				switch effort {
				case "none":
					out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.DeleteBytes(out, "output_config.effort")
				case "auto":
					out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.DeleteBytes(out, "output_config.effort")
				default:
					if mapped, ok := thinking.MapToClaudeEffort(effort, supportsMax); ok {
						effort = mapped
					}
					out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.SetBytes(out, "output_config.effort", effort)
				}
			} else {
				// Legacy/manual thinking (budget_tokens).
				budget, ok := thinking.ConvertLevelToBudget(effort)
				if ok {
					switch budget {
					case 0:
						out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
					case -1:
						out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
					default:
						if budget > 0 {
							out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
							out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
						}
					}
				}
			}
		}
	}

	// Helper for generating tool call IDs when missing
	genToolCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		for range 24 {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "toolu_" + b.String()
	}

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Max tokens
	if mot := root.Get("max_output_tokens"); mot.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", mot.Int())
	}

	// Stream
	out, _ = sjson.SetBytes(out, "stream", stream)

	systemBlocks := make([][]byte, 0, 4)
	appendSystemText := func(text string, cacheSource gjson.Result) {
		if text == "" {
			return
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		if cacheSource.Exists() {
			block = translatorcommon.AttachCacheControl(block, cacheSource)
		}
		systemBlocks = append(systemBlocks, block)
	}
	if instructions := root.Get("instructions"); instructions.Type == gjson.String {
		appendSystemText(instructions.String(), gjson.Result{})
	}
	if input := root.Get("input"); input.IsArray() {
		input.ForEach(
			func(_, item gjson.Result) bool {
				if !isResponsesSystemLevelRole(item.Get("role").String()) {
					return true
				}
				startIndex := len(systemBlocks)
				content := item.Get("content")
				if content.Type == gjson.String {
					appendSystemText(content.String(), gjson.Result{})
				} else if content.IsArray() {
					content.ForEach(
						func(_, part gjson.Result) bool {
							switch part.Get("type").String() {
							case "input_text", "output_text", "text":
								appendSystemText(part.Get("text").String(), part)
							default:
								if block := responsesSystemUnsupportedBlock(part); len(block) > 0 {
									systemBlocks = append(systemBlocks, block)
								}
							}
							return true
						},
					)
				}
				if item.Get("cache_control").Exists() && len(systemBlocks) > startIndex {
					lastIndex := len(systemBlocks) - 1
					if !gjson.GetBytes(systemBlocks[lastIndex], "cache_control").Exists() {
						systemBlocks[lastIndex] = translatorcommon.AttachCacheControl(
							systemBlocks[lastIndex],
							item,
						)
					}
				}
				return true
			},
		)
	}

	// input array processing
	var pendingRole string
	var pendingParts [][]byte
	var pendingToolUseParts [][]byte
	appendMessage := func(message []byte) {
		out, _ = sjson.SetRawBytes(out, "messages.-1", message)
	}
	flushPendingMessage := func() {
		if pendingRole == "" {
			return
		}

		parts := pendingParts
		if pendingRole == "assistant" && len(pendingToolUseParts) > 0 {
			combined := make([][]byte, 0, len(pendingParts)+len(pendingToolUseParts))
			combined = append(combined, pendingParts...)
			combined = append(combined, pendingToolUseParts...)
			parts = combined
		}
		if len(parts) > 0 {
			message := []byte(`{"role":"","content":[]}`)
			message, _ = sjson.SetBytes(message, "role", pendingRole)
			if len(parts) == 1 {
				part := gjson.ParseBytes(parts[0])
				if part.Get("type").String() == "text" && !part.Get("cache_control").Exists() {
					message, _ = sjson.SetBytes(message, "content", part.Get("text").String())
				} else {
					message, _ = sjson.SetRawBytes(message, "content", translatorcommon.JoinRawArray(parts))
				}
			} else {
				message, _ = sjson.SetRawBytes(message, "content", translatorcommon.JoinRawArray(parts))
			}
			appendMessage(message)
		}

		pendingRole = ""
		pendingParts = nil
		pendingToolUseParts = nil
	}
	appendParts := func(role string, parts ...[]byte) {
		if role == "" || len(parts) == 0 {
			return
		}
		if pendingRole != "" && pendingRole != role {
			flushPendingMessage()
		}
		pendingRole = role
		pendingParts = append(pendingParts, parts...)
	}
	appendToolUse := func(toolUse []byte) {
		if len(toolUse) == 0 {
			return
		}
		if pendingRole != "" && pendingRole != "assistant" {
			flushPendingMessage()
		}
		pendingRole = "assistant"
		pendingToolUseParts = append(pendingToolUseParts, toolUse)
	}

	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(
			func(_, item gjson.Result) bool {
				if isResponsesSystemLevelRole(item.Get("role").String()) {
					return true
				}
				typ := item.Get("type").String()
				if typ == "" && item.Get("role").String() != "" {
					typ = "message"
				}
				switch typ {
				case "message":
					// Determine role and construct Claude-compatible content parts.
					var role string
					var partsJSON [][]byte
					if parts := item.Get("content"); parts.Exists() && parts.IsArray() {
						parts.ForEach(
							func(_, part gjson.Result) bool {
								partType := part.Get("type").String()
								switch partType {
								case "input_text", "output_text":
									if text := part.Get("text"); text.Exists() {
										contentPart := []byte(`{"type":"text","text":""}`)
										contentPart, _ = sjson.SetBytes(contentPart, "text", text.String())
										contentPart = translatorcommon.AttachCacheControl(contentPart, part)
										partsJSON = append(partsJSON, contentPart)
									}
									if partType == "input_text" {
										role = "user"
									} else {
										role = "assistant"
									}
								case "input_image":
									url := part.Get("image_url").String()
									if url == "" {
										url = part.Get("url").String()
									}
									if url != "" {
										var contentPart []byte
										if after, ok := strings.CutPrefix(url, "data:"); ok {
											trimmed := after
											mediaAndData := strings.SplitN(trimmed, ";base64,", 2)
											mediaType := "application/octet-stream"
											data := ""
											if len(mediaAndData) == 2 {
												if mediaAndData[0] != "" {
													mediaType = mediaAndData[0]
												}
												data = mediaAndData[1]
											}
											if data != "" {
												contentPart = []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
												contentPart, _ = sjson.SetBytes(
													contentPart, "source.media_type", mediaType,
												)
												contentPart, _ = sjson.SetBytes(contentPart, "source.data", data)
											}
										} else {
											contentPart = []byte(`{"type":"image","source":{"type":"url","url":""}}`)
											contentPart, _ = sjson.SetBytes(contentPart, "source.url", url)
										}
										if len(contentPart) > 0 {
											contentPart = translatorcommon.AttachCacheControl(contentPart, part)
											partsJSON = append(partsJSON, contentPart)
											if role == "" {
												role = "user"
											}
										}
									}
								case "input_file":
									fileData := part.Get("file_data").String()
									if fileData != "" {
										mediaType := "application/octet-stream"
										data := fileData
										if after, ok := strings.CutPrefix(fileData, "data:"); ok {
											trimmed := after
											mediaAndData := strings.SplitN(trimmed, ";base64,", 2)
											if len(mediaAndData) == 2 {
												if mediaAndData[0] != "" {
													mediaType = mediaAndData[0]
												}
												data = mediaAndData[1]
											}
										}
										contentPart := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
										contentPart, _ = sjson.SetBytes(contentPart, "source.media_type", mediaType)
										contentPart, _ = sjson.SetBytes(contentPart, "source.data", data)
										contentPart = translatorcommon.AttachCacheControl(contentPart, part)
										partsJSON = append(partsJSON, contentPart)
										if role == "" {
											role = "user"
										}
									}
								}
								return true
							},
						)
					} else if parts.Type == gjson.String && parts.String() != "" {
						contentPart := []byte(`{"type":"text","text":""}`)
						contentPart, _ = sjson.SetBytes(contentPart, "text", parts.String())
						partsJSON = append(partsJSON, contentPart)
					}

					// Fallback to given role if content types not decisive
					if role == "" {
						switch item.Get("role").String() {
						case "user", "assistant":
							role = item.Get("role").String()
						default:
							role = "user"
						}
					}

					if len(partsJSON) > 0 {
						lastIndex := len(partsJSON) - 1
						if !gjson.GetBytes(partsJSON[lastIndex], "cache_control").Exists() {
							partsJSON[lastIndex] = translatorcommon.AttachCacheControl(partsJSON[lastIndex], item)
						}
						appendParts(role, partsJSON...)
					}

				case "reasoning":
					if thinkingPart := convertResponsesReasoningToClaudeThinking(
						item,
						preserveThinkingBlocks,
					); len(thinkingPart) > 0 {
						appendParts("assistant", thinkingPart)
					}

				case "function_call", "custom_tool_call":
					// Map function and freeform custom calls to Claude tool_use blocks.
					callID := item.Get("call_id").String()
					if callID == "" {
						callID = genToolCallID()
					} else {
						callID = util.SanitizeClaudeToolID(callID)
					}
					name := toolIdentity.ResolveName(item.Get("name").String(), item.Get("namespace").String())

					toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
					toolUse, _ = sjson.SetBytes(toolUse, "id", callID)
					toolUse, _ = sjson.SetBytes(toolUse, "name", name)
					if typ == "custom_tool_call" {
						toolUse, _ = sjson.SetRawBytes(
							toolUse, "input", []byte(translatorcommon.WrapResponsesCustomToolInput(item.Get("input"))),
						)
					} else {
						argsStr := item.Get("arguments").String()
						if argsStr != "" && gjson.Valid(argsStr) {
							argsJSON := gjson.Parse(argsStr)
							if argsJSON.IsObject() {
								toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
							}
						}
					}

					appendToolUse(toolUse)

				case "function_call_output", "custom_tool_call_output":
					// Map to user tool_result
					callID := util.SanitizeClaudeToolID(item.Get("call_id").String())
					outputStr := translatorcommon.ResponsesToolOutputText(item.Get("output"))
					toolResult := []byte(`{"type":"tool_result","tool_use_id":"","content":""}`)
					toolResult, _ = sjson.SetBytes(toolResult, "tool_use_id", callID)
					toolResult, _ = sjson.SetBytes(toolResult, "content", outputStr)

					appendParts("user", toolResult)
				}
				return true
			},
		)
	}
	flushPendingMessage()
	if gjson.GetBytes(out, "messages.#").Int() == 0 && len(systemBlocks) > 0 {
		appendMessage([]byte(`{"role":"user","content":[{"type":"text","text":""}]}`))
	}
	if len(systemBlocks) > 0 {
		out, _ = sjson.SetRawBytes(out, "system", translatorcommon.JoinRawArray(systemBlocks))
	}

	includedToolNames := map[string]struct{}{}
	toolsJSON := []byte("[]")
	for _, declaration := range toolIdentity.Declarations() {
		tJSON, ok := convertResponsesDeclarationToClaude(declaration)
		if !ok {
			continue
		}
		includedToolNames[declaration.QualifiedName] = struct{}{}
		toolsJSON, _ = sjson.SetRawBytes(toolsJSON, "-1", tJSON)
	}
	appendBuiltinTools := func(tools gjson.Result) {
		if !tools.IsArray() {
			return
		}
		for _, tool := range tools.Array() {
			toolType := strings.TrimSpace(tool.Get("type").String())
			if toolType == "" || toolType == "function" || toolType == "custom" || toolType == "namespace" {
				continue
			}
			for _, tJSON := range convertResponsesToolToClaudeTools(tool, nil) {
				toolName := gjson.GetBytes(tJSON, "name").String()
				if toolName != "" {
					if _, duplicate := includedToolNames[toolName]; duplicate {
						continue
					}
					includedToolNames[toolName] = struct{}{}
				}
				toolsJSON, _ = sjson.SetRawBytes(toolsJSON, "-1", tJSON)
			}
		}
	}
	appendBuiltinTools(root.Get("tools"))
	if input := root.Get("input"); input.IsArray() {
		for _, item := range input.Array() {
			if item.Get("type").String() == "additional_tools" {
				appendBuiltinTools(item.Get("tools"))
			}
		}
	}
	if parsedTools := gjson.ParseBytes(toolsJSON); parsedTools.IsArray() && len(parsedTools.Array()) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", toolsJSON)
	}

	// Map tool_choice similar to Chat Completions translator (optional in docs, safe to handle)
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		switch toolChoice.Type {
		case gjson.String:
			switch toolChoice.String() {
			case "auto":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "none":
				// Leave unset; implies no tools
			case "required":
				if len(includedToolNames) > 0 {
					out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
				}
			}
		case gjson.JSON:
			choiceType := toolChoice.Get("type").String()
			if choiceType == "function" || choiceType == "custom" {
				fn := toolChoice.Get("function.name").String()
				if fn == "" {
					fn = toolChoice.Get("custom.name").String()
				}
				if fn == "" {
					fn = toolChoice.Get("name").String()
				}
				namespace := toolChoice.Get("namespace").String()
				if namespace == "" {
					namespace = toolChoice.Get("function.namespace").String()
				}
				if namespace == "" {
					namespace = toolChoice.Get("custom.namespace").String()
				}
				fn = toolIdentity.ResolveName(fn, namespace)
				if _, ok := includedToolNames[fn]; ok {
					toolChoiceJSON := []byte(`{"name":"","type":"tool"}`)
					toolChoiceJSON, _ = sjson.SetBytes(toolChoiceJSON, "name", fn)
					out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
				}
			}
		default:

		}
	}

	return out
}

func isResponsesSystemLevelRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return true
	default:
		return false
	}
}

func responsesSystemUnsupportedBlock(part gjson.Result) []byte {
	partType := strings.TrimSpace(part.Get("type").String())
	if partType == "" {
		return nil
	}
	block := []byte(`{"type":""}`)
	block, _ = sjson.SetBytes(block, "type", partType)
	return block
}

func convertResponsesDeclarationToClaude(declaration translatorcommon.ResponsesToolDeclaration) ([]byte, bool) {
	tool := gjson.Parse(declaration.Raw)
	if declaration.Kind == translatorcommon.ResponsesCustomTool {
		tJSON := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}`)
		tJSON, _ = sjson.SetBytes(tJSON, "name", declaration.QualifiedName)
		if description := responsesToolDescription(tool); description != "" {
			tJSON, _ = sjson.SetBytes(tJSON, "description", description)
		}
		return tJSON, true
	}
	return convertResponsesFunctionToolToClaude(tool, declaration.QualifiedName)
}

func convertResponsesToolToClaudeTools(tool gjson.Result, toolNameMap map[string]string) [][]byte {
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "", "function":
		if tJSON, ok := convertResponsesFunctionToolToClaude(tool, ""); ok {
			return [][]byte{tJSON}
		}
	case "namespace":
		return convertResponsesNamespaceToolToClaude(tool, toolNameMap)
	case "web_search":
		if tJSON, ok := convertResponsesWebSearchToolToClaude(tool); ok {
			if name := gjson.GetBytes(tJSON, "name").String(); name != "" && toolNameMap != nil {
				toolNameMap[name] = name
			}
			return [][]byte{tJSON}
		}
	default:
		if isUnsupportedOpenAIBuiltinToolType(toolType) {
			return nil
		}
		if tool.Get("name").String() != "" {
			return [][]byte{[]byte(tool.Raw)}
		}
	}
	return nil
}

func convertResponsesNamespaceToolToClaude(tool gjson.Result, toolNameMap map[string]string) [][]byte {
	namespaceName := strings.TrimSpace(tool.Get("name").String())
	children := tool.Get("tools")
	if !children.Exists() || !children.IsArray() {
		return nil
	}

	var out [][]byte
	children.ForEach(
		func(_, child gjson.Result) bool {
			childName := responsesToolName(child)
			qualifiedName := qualifyResponsesNamespaceToolName(namespaceName, childName)
			if tJSON, ok := convertResponsesFunctionToolToClaude(child, qualifiedName); ok {
				out = append(out, tJSON)
				toolNameMap[qualifiedName] = qualifiedName
				if childName != "" {
					toolNameMap[childName] = qualifiedName
				}
			}
			return true
		},
	)
	return out
}

func convertResponsesFunctionToolToClaude(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolName(tool)
	}
	if name == "" {
		return nil, false
	}

	tJSON := []byte(`{"name":"","description":"","input_schema":{}}`)
	tJSON, _ = sjson.SetBytes(tJSON, "name", name)
	if d := responsesToolDescription(tool); d != "" {
		tJSON, _ = sjson.SetBytes(tJSON, "description", d)
	}
	tJSON, _ = sjson.SetRawBytes(
		tJSON,
		"input_schema",
		util.NormalizeClaudeToolInputSchema([]byte(responsesToolParameters(tool).Raw)),
	)
	return tJSON, true
}

func convertResponsesWebSearchToolToClaude(tool gjson.Result) ([]byte, bool) {
	if externalWebAccess := tool.Get("external_web_access"); externalWebAccess.Exists() && !externalWebAccess.Bool() {
		return nil, false
	}

	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		name = "web_search"
	}
	tJSON := []byte(`{"type":"web_search_20250305","name":""}`)
	tJSON, _ = sjson.SetBytes(tJSON, "name", name)
	if maxUses := tool.Get("max_uses"); maxUses.Exists() {
		tJSON, _ = sjson.SetBytes(tJSON, "max_uses", maxUses.Int())
	}
	if domains := nonEmptyStringArrayRaw(tool.Get("filters.allowed_domains")); len(domains) > 0 {
		tJSON, _ = sjson.SetRawBytes(tJSON, "allowed_domains", domains)
	}
	if domains := nonEmptyStringArrayRaw(tool.Get("filters.blocked_domains")); len(domains) > 0 {
		tJSON, _ = sjson.SetRawBytes(tJSON, "blocked_domains", domains)
	}
	if userLocation := tool.Get("user_location"); userLocation.Exists() && userLocation.IsObject() {
		tJSON, _ = sjson.SetRawBytes(tJSON, "user_location", []byte(userLocation.Raw))
	}
	return tJSON, true
}

func nonEmptyStringArrayRaw(value gjson.Result) []byte {
	if !value.Exists() || !value.IsArray() {
		return nil
	}
	out := []byte(`[]`)
	count := 0
	for _, item := range value.Array() {
		text := strings.TrimSpace(item.String())
		if text == "" {
			continue
		}
		updated, errSet := sjson.SetBytes(out, "-1", text)
		if errSet != nil {
			return nil
		}
		out = updated
		count++
	}
	if count == 0 {
		return nil
	}
	return out
}

func responsesToolName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

func responsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description").String(); description != "" {
		return description
	}
	return tool.Get("function.description").String()
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{
		"parameters",
		"parametersJsonSchema",
		"input_schema",
		"function.parameters",
		"function.parametersJsonSchema",
	} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}

func qualifyResponsesNamespaceToolName(namespaceName, childName string) string {
	childName = strings.TrimSpace(childName)
	if childName == "" || namespaceName == "" || strings.HasPrefix(childName, "mcp__") {
		return childName
	}
	if strings.HasPrefix(childName, namespaceName) {
		return childName
	}
	if strings.HasSuffix(namespaceName, "__") {
		return namespaceName + childName
	}
	return namespaceName + "__" + childName
}

func isUnsupportedOpenAIBuiltinToolType(toolType string) bool {
	switch toolType {
	case "image_generation", "file_search", "code_interpreter", "computer_use_preview":
		return true
	default:
		return false
	}
}

func convertResponsesReasoningToClaudeThinking(
	item gjson.Result,
	preserveThinkingBlocks ...bool,
) []byte {
	encryptedContent := item.Get("encrypted_content").String()
	normalizedSignature, ok := signature.CompatibleSignatureForProvider(
		signature.ProviderClaude,
		encryptedContent,
		signature.BlockKindClaudeThinking,
	)
	preserveThinking := len(preserveThinkingBlocks) > 0 && preserveThinkingBlocks[0]
	if !ok {
		if !preserveThinking {
			return nil
		}
		normalizedSignature = encryptedContent
	}

	thinkingPart := []byte(`{"type":"thinking","thinking":"","signature":""}`)
	thinkingPart, _ = sjson.SetBytes(thinkingPart, "thinking", responsesReasoningSummaryText(item))
	thinkingPart, _ = sjson.SetBytes(thinkingPart, "signature", normalizedSignature)
	return thinkingPart
}

func responsesReasoningSummaryText(item gjson.Result) string {
	var builder strings.Builder
	if summary := item.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(
			func(_, part gjson.Result) bool {
				if text := part.Get("text"); text.Exists() {
					builder.WriteString(text.String())
				} else if part.Type == gjson.String {
					builder.WriteString(part.String())
				}
				return true
			},
		)
	}
	return builder.String()
}

package interactions

import (
	"encoding/json"
	"fmt"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertInteractionsRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, err := convertInteractionsRequestToAntigravityChecked(modelName, inputRawJSON, stream)
	if err != nil {
		log.WithError(err).Warn("Interactions to Antigravity request translation rejected")
		return nil
	}
	return out
}

func convertInteractionsRequestToAntigravityChecked(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	root := gjson.ParseBytes(inputRawJSON)
	if err := translatorcommon.RejectDroppedInteractionsUserAttachments(root.Get("input"), true, func(part gjson.Result, native bool) []byte {
		if native {
			return interactionsNativeAntigravityPart(part)
		}
		return appendInteractionsContentToAntigravityPart(nil, part, false)
	}); err != nil {
		return nil, err
	}
	functionNameMap := util.SanitizedFunctionNameMap(inputRawJSON)
	out := []byte(`{"project":"","request":{"contents":[]},"model":""}`)
	out, _ = sjson.SetBytes(out, "model", modelName)
	if stream || root.Get("stream").Bool() {
		out, _ = sjson.SetBytes(out, "request.stream", true)
	}
	out = copyInteractionsSystemToAntigravity(out, root)
	out = copyInteractionsGenerationConfigToAntigravity(out, root)
	out = appendInteractionsInputToAntigravity(out, root.Get("input"))
	var err error
	out, err = copyInteractionsToolsToAntigravity(out, root, functionNameMap)
	if err != nil {
		return nil, err
	}
	out = rewriteInteractionsFunctionNames(out, functionNameMap)
	out = attachDefaultAntigravitySafetySettings(out)
	return out, nil
}

func rewriteInteractionsFunctionNames(out []byte, functionNameMap map[string]string) []byte {
	contents := gjson.GetBytes(out, "request.contents")
	contentsChanged := false
	contentItems := make([][]byte, 0, len(contents.Array()))
	for _, content := range contents.Array() {
		contentJSON := []byte(content.Raw)
		partsChanged := false
		partItems := make([][]byte, 0, len(content.Get("parts").Array()))
		for _, part := range content.Get("parts").Array() {
			partJSON := []byte(part.Raw)
			for _, field := range []string{"functionCall", "functionResponse"} {
				nameResult := part.Get(field + ".name")
				name := nameResult.String()
				if name == "" {
					continue
				}
				mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
				if nameResult.Type == gjson.String && mappedName == name {
					continue
				}
				partJSON, _ = sjson.SetBytes(partJSON, field+".name", mappedName)
				partsChanged = true
			}
			partItems = append(partItems, partJSON)
		}
		if partsChanged {
			contentJSON, _ = sjson.SetRawBytes(
				contentJSON,
				"parts",
				translatorcommon.JoinRawArray(partItems),
			)
			contentsChanged = true
		}
		contentItems = append(contentItems, contentJSON)
	}
	if contentsChanged {
		out = translatorcommon.SetRawArrayItems(out, "request.contents", contentItems)
	}

	allowedPath := "request.toolConfig.functionCallingConfig.allowedFunctionNames"
	allowedNames := gjson.GetBytes(out, allowedPath)
	if allowedNames.IsArray() {
		namesChanged := false
		nameItems := make([][]byte, 0, len(allowedNames.Array()))
		for _, name := range allowedNames.Array() {
			mappedName := util.MapSanitizedFunctionName(functionNameMap, name.String())
			namesChanged = namesChanged || name.Type != gjson.String || mappedName != name.String()
			mappedNameJSON, _ := json.Marshal(mappedName)
			nameItems = append(nameItems, mappedNameJSON)
		}
		if namesChanged {
			out, _ = sjson.SetRawBytes(out, allowedPath, translatorcommon.JoinRawArray(nameItems))
		}
	}
	return out
}

func copyInteractionsSystemToAntigravity(out []byte, root gjson.Result) []byte {
	sys := root.Get("system_instruction")
	if !sys.Exists() {
		return out
	}
	if sys.Type == gjson.String {
		instr := []byte(`{"parts":[{"text":""}]}`)
		instr, _ = sjson.SetBytes(instr, "parts.0.text", sys.String())
		out, _ = sjson.SetRawBytes(out, "request.systemInstruction", instr)
		return out
	}
	if text := sys.Get("text"); text.Exists() && !sys.Get("parts").Exists() {
		instr := []byte(`{"parts":[{"text":""}]}`)
		instr, _ = sjson.SetBytes(instr, "parts.0.text", text.String())
		out, _ = sjson.SetRawBytes(out, "request.systemInstruction", instr)
		return out
	}
	out, _ = sjson.SetRawBytes(out, "request.systemInstruction", []byte(sys.Raw))
	return out
}

func copyInteractionsGenerationConfigToAntigravity(out []byte, root gjson.Result) []byte {
	if cfg := root.Get("generation_config"); cfg.Exists() {
		out, _ = sjson.SetRawBytes(
			out, "request.generationConfig", convertSnakeCaseKeysToCamelCaseForAntigravity([]byte(cfg.Raw)),
		)
	} else if cfg := root.Get("generationConfig"); cfg.Exists() {
		out, _ = sjson.SetRawBytes(out, "request.generationConfig", []byte(cfg.Raw))
	}
	out = normalizeInteractionsGenerationConfigForAntigravity(out)
	out = copyInteractionsReasoningToAntigravity(out, root)
	out = copyInteractionsResponseModalitiesToAntigravity(out, root)
	out = copyInteractionsToolChoiceToAntigravity(out, root)
	return out
}

func normalizeInteractionsGenerationConfigForAntigravity(out []byte) []byte {
	if thinkingLevel := gjson.GetBytes(out, "request.generationConfig.thinkingLevel"); thinkingLevel.Exists() {
		out, _ = sjson.SetRawBytes(
			out, "request.generationConfig.thinkingConfig.thinkingLevel", []byte(thinkingLevel.Raw),
		)
		out, _ = sjson.DeleteBytes(out, "request.generationConfig.thinkingLevel")
	}
	if thinkingBudget := gjson.GetBytes(out, "request.generationConfig.thinkingBudget"); thinkingBudget.Exists() {
		out, _ = sjson.SetRawBytes(
			out, "request.generationConfig.thinkingConfig.thinkingBudget", []byte(thinkingBudget.Raw),
		)
		out, _ = sjson.DeleteBytes(out, "request.generationConfig.thinkingBudget")
	}
	canonicalPath := "request.generationConfig.thinkingConfig.includeThoughts"
	nestedInclude := false
	hasNestedInclude := false
	for _, path := range []string{canonicalPath, "request.generationConfig.thinkingConfig.include_thoughts"} {
		// Only JSON booleans have meaning for includeThoughts.
		// noinspection GoSwitchMissingCasesForIotaConsts
		switch includeThoughts := gjson.GetBytes(out, path); includeThoughts.Type {
		case gjson.True:
			if !hasNestedInclude {
				nestedInclude = true
				hasNestedInclude = true
			}
		case gjson.False:
			if !hasNestedInclude {
				hasNestedInclude = true
			}
		}
		out, _ = sjson.DeleteBytes(out, path)
	}
	if hasNestedInclude {
		out, _ = sjson.SetBytes(out, canonicalPath, nestedInclude)
	}
	if includeThoughts := gjson.GetBytes(out, "request.generationConfig.includeThoughts"); includeThoughts.Exists() {
		// Only JSON booleans have meaning for includeThoughts.
		// noinspection GoSwitchMissingCasesForIotaConsts
		switch includeThoughts.Type {
		case gjson.True:
			out, _ = sjson.SetBytes(out, canonicalPath, true)
		case gjson.False:
			out, _ = sjson.SetBytes(out, canonicalPath, false)
		}
		out, _ = sjson.DeleteBytes(out, "request.generationConfig.includeThoughts")
	}
	if summaries := gjson.GetBytes(out, "request.generationConfig.thinkingSummaries"); summaries.Exists() {
		if includeThoughts, ok := antigravityThinkingSummariesIncludeThoughts(summaries); ok {
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
		}
		out, _ = sjson.DeleteBytes(out, "request.generationConfig.thinkingSummaries")
	}
	if toolChoice := gjson.GetBytes(out, "request.generationConfig.toolChoice"); toolChoice.Exists() {
		out, _ = sjson.DeleteBytes(out, "request.generationConfig.toolChoice")
	}
	return out
}

func copyInteractionsReasoningToAntigravity(out []byte, root gjson.Result) []byte {
	reasoning := root.Get("reasoning")
	if !reasoning.Exists() {
		return out
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Get("effort").String()))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(reasoning.Get("thinking_level").String()))
	}
	if effort != "" {
		if effort == "auto" {
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget", -1)
		} else {
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel", effort)
		}
	}
	if summary := reasoning.Get("summary"); summary.Exists() {
		if includeThoughts, ok := antigravityThinkingSummariesIncludeThoughts(summary); ok {
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
		}
	}
	return out
}

func copyInteractionsResponseModalitiesToAntigravity(out []byte, root gjson.Result) []byte {
	mods := root.Get("response_modalities")
	if !mods.Exists() {
		mods = root.Get("responseModalities")
	}
	if !mods.Exists() || !mods.IsArray() {
		return out
	}
	var responseMods []string
	mods.ForEach(
		func(_, mod gjson.Result) bool {
			switch strings.ToLower(strings.TrimSpace(mod.String())) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			case "audio":
				responseMods = append(responseMods, "AUDIO")
			}
			return true
		},
	)
	if len(responseMods) > 0 {
		out, _ = sjson.SetBytes(out, "request.generationConfig.responseModalities", responseMods)
	}
	return out
}

func copyInteractionsToolChoiceToAntigravity(out []byte, root gjson.Result) []byte {
	toolChoice := root.Get("tool_choice")
	if !toolChoice.Exists() {
		toolChoice = root.Get("generation_config.tool_choice")
	}
	if !toolChoice.Exists() {
		toolChoice = root.Get("generationConfig.toolChoice")
	}
	if !toolChoice.Exists() {
		return out
	}
	mode := ""
	var allowedNames []string
	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "none":
			mode = "NONE"
		case "auto":
			mode = "AUTO"
		case "required", "any":
			mode = "ANY"
		}
	} else if toolChoice.IsObject() {
		switch strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String())) {
		case "none":
			mode = "NONE"
		case "auto":
			mode = "AUTO"
		case "required", "any":
			mode = "ANY"
		case "function":
			mode = "ANY"
			if name := toolChoice.Get("function.name").String(); strings.TrimSpace(name) != "" {
				allowedNames = append(allowedNames, name)
			}
		case "tool":
			mode = "ANY"
			if name := toolChoice.Get("name").String(); strings.TrimSpace(name) != "" {
				allowedNames = append(allowedNames, name)
			}
		}
	}
	if mode == "" {
		return out
	}
	out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", mode)
	if len(allowedNames) > 0 {
		out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.allowedFunctionNames", allowedNames)
	}
	return out
}

func appendInteractionsInputToAntigravity(out []byte, input gjson.Result) []byte {
	if !input.Exists() {
		return out
	}
	if input.Type == gjson.String {
		return appendAntigravityTextContent(out, "user", input.String())
	}
	if input.IsArray() {
		steps, _ := sjson.SetRawBytes([]byte(`{"steps":[]}`), "steps", []byte(input.Raw))
		return appendInteractionsStepToAntigravity(out, gjson.ParseBytes(steps), "user")
	}
	if steps := input.Get("steps"); steps.Exists() && steps.IsArray() {
		return appendInteractionsStepToAntigravity(out, input, "user")
	}
	return appendInteractionsStepToAntigravity(out, input, "user")
}

func appendInteractionsStepToAntigravity(out []byte, step gjson.Result, defaultRole string) []byte {
	if step.Type == gjson.String {
		return appendAntigravityTextContent(out, defaultRole, step.String())
	}
	defaultRole = antigravityContentRole(step.Get("role").String(), defaultRole)
	if steps := step.Get("steps"); steps.Exists() && steps.IsArray() {
		var contentItems [][]byte
		mergePrevious := false
		children := steps.Array()
		for index, child := range children {
			if child.Get("type").String() == "thought" {
				if signature := antigravityThoughtSignature(child); signature != "" {
					next := index + 1
					for next < len(children) && children[next].Get("type").String() == "thought" && antigravityThoughtSignature(children[next]) == "" {
						next++
					}
					if next < len(children) && children[next].Get("type").String() == "function_call" {
						ownSignature := antigravityThoughtSignature(children[next])
						if ownSignature == "" || ownSignature == signature {
							call, _ := sjson.SetBytes([]byte(children[next].Raw), "signature", signature)
							children[next] = gjson.ParseBytes(call)
							if !child.Get("content").Exists() && !child.Get("summary").Exists() && !child.Get("text").Exists() {
								continue
							}
						}
					}
				}
			}
			standaloneTurn := child.Get("steps").IsArray() || child.Get("parts").Exists() ||
				child.Get("type").String() == "user_input" || child.Get("type").String() == "" || child.Type == gjson.String
			if standaloneTurn {
				mergePrevious = false
			}
			converted := appendInteractionsStepToAntigravity([]byte(`{"request":{"contents":[]}}`), child, defaultRole)
			for _, content := range gjson.GetBytes(converted, "request.contents").Array() {
				last := len(contentItems) - 1
				if mergePrevious && !standaloneTurn && last >= 0 &&
					gjson.GetBytes(contentItems[last], "role").String() == content.Get("role").String() {
					var parts [][]byte
					for _, part := range gjson.GetBytes(contentItems[last], "parts").Array() {
						parts = append(parts, []byte(part.Raw))
					}
					for _, part := range content.Get("parts").Array() {
						parts = append(parts, []byte(part.Raw))
					}
					contentItems[last], _ = sjson.SetRawBytes(contentItems[last], "parts", translatorcommon.JoinRawArray(parts))
				} else {
					contentItems = append(contentItems, []byte(content.Raw))
				}
				mergePrevious = !standaloneTurn
			}
		}
		for _, content := range contentItems {
			out, _ = sjson.SetRawBytes(out, "request.contents.-1", content)
		}
		return out
	}
	switch step.Get("type").String() {
	case "model_output":
		return appendInteractionsStepContentToAntigravity(out, "model", step, false)
	case "thought":
		return appendInteractionsStepContentToAntigravity(out, "model", step, true)
	case "function_call":
		return appendInteractionsFunctionCallToAntigravity(out, step)
	case "function_result":
		return appendInteractionsFunctionResultToAntigravity(out, step)
	case "user_input", "":
		if step.Get("parts").Exists() {
			return appendInteractionsNativeContentToAntigravity(out, step, defaultRole)
		}
		return appendInteractionsContentListToAntigravity(out, defaultRole, step.Get("content"))
	default:
		if step.Get("parts").Exists() {
			return appendInteractionsNativeContentToAntigravity(out, step, defaultRole)
		}
		if step.Get("content").Exists() {
			return appendInteractionsContentListToAntigravity(out, defaultRole, step.Get("content"))
		}
		if text := step.Get("text"); text.Exists() {
			return appendAntigravityTextContent(out, defaultRole, text.String())
		}
	}
	return out
}

func appendInteractionsNativeContentToAntigravity(out []byte, step gjson.Result, defaultRole string) []byte {
	parts := step.Get("parts")
	if !parts.Exists() || !parts.IsArray() {
		return out
	}
	contentObj := []byte(`{"role":"","parts":[]}`)
	contentObj, _ = sjson.SetBytes(contentObj, "role", antigravityContentRole(step.Get("role").String(), defaultRole))
	parts.ForEach(
		func(_, part gjson.Result) bool {
			if partJSON := interactionsNativeAntigravityPart(part); len(partJSON) > 0 {
				contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", partJSON)
			}
			return true
		},
	)
	if gjson.GetBytes(contentObj, "parts.#").Int() == 0 {
		return out
	}
	out, _ = sjson.SetRawBytes(out, "request.contents.-1", contentObj)
	return out
}

func appendInteractionsStepContentToAntigravity(out []byte, role string, step gjson.Result, thought bool) []byte {
	content := step.Get("content")
	if !content.Exists() && thought {
		content = step.Get("summary")
	}
	if !content.Exists() {
		content = step.Get("text")
	}
	if !content.Exists() && (!thought || antigravityThoughtSignature(step) == "") {
		return out
	}
	contentObj := []byte(`{"role":"","parts":[]}`)
	contentObj, _ = sjson.SetBytes(contentObj, "role", role)
	if content.IsArray() {
		content.ForEach(
			func(_, part gjson.Result) bool {
				if partJSON := appendInteractionsContentToAntigravityPart(nil, part, thought); len(partJSON) > 0 {
					contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", partJSON)
				}
				return true
			},
		)
	} else if content.IsObject() {
		if partJSON := appendInteractionsContentToAntigravityPart(nil, content, thought); len(partJSON) > 0 {
			contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", partJSON)
		}
	} else if content.Type == gjson.String {
		contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", antigravityTextPartJSON(content.String(), thought))
	}
	stepJSON := []byte(step.Raw)
	if rawSignature := step.Get("signature"); rawSignature.Exists() {
		stepJSON, _ = sjson.SetBytes(stepJSON, "thoughtSignature", rawSignature.String())
	}
	if signature := antigravityThoughtSignature(gjson.ParseBytes(stepJSON)); signature != "" {
		partCount := gjson.GetBytes(contentObj, "parts.#").Int()
		if partCount == 0 && thought {
			part, _ := sjson.SetBytes([]byte(`{"thought":true}`), "thoughtSignature", signature)
			contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", part)
		} else if partCount > 0 && role == "model" {
			path := fmt.Sprintf("parts.%d.thoughtSignature", partCount-1)
			if !gjson.GetBytes(contentObj, path).Exists() {
				contentObj, _ = sjson.SetBytes(contentObj, path, signature)
			}
		}
	}
	if gjson.GetBytes(contentObj, "parts.#").Int() == 0 {
		return out
	}
	out, _ = sjson.SetRawBytes(out, "request.contents.-1", contentObj)
	return out
}

func appendInteractionsContentListToAntigravity(out []byte, role string, content gjson.Result) []byte {
	if !content.Exists() {
		return out
	}
	step, _ := sjson.SetRawBytes([]byte(`{}`), "content", []byte(content.Raw))
	return appendInteractionsStepContentToAntigravity(out, role, gjson.ParseBytes(step), false)
}

func appendInteractionsContentToAntigravityPart(_ []byte, content gjson.Result, thought bool) []byte {
	if text := content.Get("text"); text.Exists() {
		partType := strings.ToLower(strings.TrimSpace(content.Get("type").String()))
		part := antigravityTextPartJSON(text.String(), thought || content.Get("thought").Bool() || partType == "thought" || partType == "thinking")
		contentJSON := []byte(content.Raw)
		if rawSignature := content.Get("signature"); rawSignature.Exists() {
			contentJSON, _ = sjson.SetBytes(contentJSON, "thoughtSignature", rawSignature.String())
		}
		if signature := antigravityThoughtSignature(gjson.ParseBytes(contentJSON)); signature != "" {
			part, _ = sjson.SetBytes(part, "thoughtSignature", signature)
		}
		return part
	}
	if inline := content.Get("inline_data"); inline.Exists() {
		return antigravityInlineDataPartJSON(inline)
	}
	if inline := content.Get("inlineData"); inline.Exists() {
		return antigravityInlineDataPartJSON(inline)
	}
	switch strings.ToLower(strings.TrimSpace(content.Get("type").String())) {
	case "text":
		if text := content.Get("text"); text.Exists() {
			return antigravityTextPartJSON(text.String(), thought)
		}
	case "image", "audio", "video", "document":
		if mime := content.Get("mime_type"); mime.Exists() || content.Get("mimeType").Exists() {
			mimeType := mime.String()
			if mimeType == "" {
				mimeType = content.Get("mimeType").String()
			}
			if data := content.Get("data").String(); data != "" {
				return antigravityInlineDataPartJSON(
					gjson.Parse(
						fmt.Sprintf(
							`{"mime_type":%q,"data":%q}`, mimeType, data,
						),
					),
				)
			}
		}
		for _, key := range []string{"file_uri", "fileUri", "uri"} {
			uri := content.Get(key)
			if uri.Type != gjson.String || strings.TrimSpace(uri.String()) == "" {
				continue
			}
			mimeType := content.Get("mime_type").String()
			if strings.TrimSpace(mimeType) == "" {
				mimeType = content.Get("mimeType").String()
			}
			return antigravityFileDataPartJSON(
				gjson.Parse(fmt.Sprintf(`{"mimeType":%q,"fileUri":%q}`, mimeType, strings.TrimSpace(uri.String()))),
			)
		}
		if url := content.Get("url"); url.Exists() {
			return antigravityInlineDataPartFromDataURL(url.String())
		}
	case "image_url":
		return antigravityInlineDataPartFromDataURL(content.Get("image_url.url").String())
	case "input_audio":
		mimeType := antigravityInputAudioMimeType(content.Get("input_audio.format").String())
		return antigravityInlineDataPartJSON(
			gjson.Parse(
				fmt.Sprintf(
					`{"mime_type":%q,"data":%q}`, mimeType, content.Get("input_audio.data").String(),
				),
			),
		)
	case "file":
		filename := content.Get("file.filename").String()
		fileData := content.Get("file.file_data").String()
		if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData(filename, "", fileData); ok {
			return antigravityInlineDataPartJSON(gjson.Parse(fmt.Sprintf(`{"mime_type":%q,"data":%q}`, mimeType, data)))
		}
	}
	return nil
}

func appendInteractionsFunctionCallToAntigravity(out []byte, step gjson.Result) []byte {
	part := []byte(`{"functionCall":{"name":"","args":{}}}`)
	part, _ = sjson.SetBytes(part, "functionCall.name", step.Get("name").String())
	if callID := step.Get("call_id"); callID.Exists() {
		part, _ = sjson.SetBytes(part, "functionCall.id", callID.String())
	} else if id := step.Get("id"); id.Exists() {
		part, _ = sjson.SetBytes(part, "functionCall.id", id.String())
	}
	if args := step.Get("arguments"); args.Exists() {
		part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(args.Raw))
	}
	if signature := antigravityThoughtSignature(step); signature != "" {
		part, _ = sjson.SetBytes(part, "thoughtSignature", signature)
	}
	contentObj := []byte(`{"role":"model","parts":[]}`)
	contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", part)
	out, _ = sjson.SetRawBytes(out, "request.contents.-1", contentObj)
	return out
}

func appendInteractionsFunctionResultToAntigravity(out []byte, step gjson.Result) []byte {
	part := []byte(`{"functionResponse":{"name":"","response":{}}}`)
	part, _ = sjson.SetBytes(part, "functionResponse.name", step.Get("name").String())
	if callID := step.Get("call_id"); callID.Exists() {
		part, _ = sjson.SetBytes(part, "functionResponse.id", callID.String())
	} else if id := step.Get("id"); id.Exists() {
		part, _ = sjson.SetBytes(part, "functionResponse.id", id.String())
	}
	if result := step.Get("result"); result.Exists() {
		part = translatorcommon.SetGeminiFunctionResponseResult(part, "functionResponse.response", result)
	}
	contentObj := []byte(`{"role":"user","parts":[]}`)
	contentObj, _ = sjson.SetRawBytes(contentObj, "parts.-1", part)
	out, _ = sjson.SetRawBytes(out, "request.contents.-1", contentObj)
	return out
}

func copyInteractionsToolsToAntigravity(
	out []byte,
	root gjson.Result,
	functionNameMap map[string]string,
) ([]byte, error) {
	if gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String() == "NONE" {
		return sjson.DeleteBytes(out, "request.tools")
	}
	tools := root.Get("tools")
	if !tools.Exists() {
		return out, nil
	}
	if !tools.IsArray() {
		return sjson.SetRawBytes(out, "request.tools", []byte(tools.Raw))
	}
	var functionDeclarations [][]byte
	var otherTools [][]byte
	var errMarshal error
	tools.ForEach(
		func(_, tool gjson.Result) bool {
			if tool.Get("type").String() == "function" || (tool.Get("type").String() == "" && tool.Get("name").Exists()) {
				if converted := antigravityFunctionDeclarationJSON(tool, functionNameMap); len(converted) > 0 {
					functionDeclarations = append(functionDeclarations, converted)
				}
				return true
			}
			normalized := translatorcommon.GeminiToolFromInteractions(tool)
			if normalized == nil {
				otherTools = append(otherTools, []byte(tool.Raw))
				return true
			}
			declarations := tool.Get("functionDeclarations")
			if !declarations.IsArray() {
				declarations = tool.Get("function_declarations")
			}
			if declarations.IsArray() {
				declarations.ForEach(
					func(_, declaration gjson.Result) bool {
						if converted := antigravityFunctionDeclarationJSON(declaration, functionNameMap); len(converted) > 0 {
							functionDeclarations = append(functionDeclarations, converted)
						}
						return true
					},
				)
				delete(normalized, "functionDeclarations")
				delete(normalized, "function_declarations")
			}
			if len(normalized) > 0 {
				var raw []byte
				raw, errMarshal = json.Marshal(normalized)
				if errMarshal != nil {
					return false
				}
				otherTools = append(otherTools, raw)
			}
			return true
		},
	)
	if errMarshal != nil {
		return nil, fmt.Errorf("interactions: encode Antigravity tools: %w", errMarshal)
	}

	deduplicated := util.DeduplicateFunctionDeclarations(
		translatorcommon.JoinRawArray(functionDeclarations),
	)
	hasFunction := len(deduplicated) > 2
	if hasFunction || len(otherTools) > 0 {
		toolItems := make([][]byte, 0, 1+len(otherTools))
		if hasFunction {
			functionToolNode := []byte(`{"functionDeclarations":[]}`)
			functionToolNode, err := sjson.SetRawBytes(functionToolNode, "functionDeclarations", deduplicated)
			if err != nil {
				return nil, err
			}
			toolItems = append(toolItems, functionToolNode)
		}
		toolItems = append(toolItems, otherTools...)
		return sjson.SetRawBytes(out, "request.tools", translatorcommon.JoinRawArray(toolItems))
	}
	return out, nil
}

func antigravityFunctionDeclarationJSON(
	declaration gjson.Result,
	functionNameMap map[string]string,
) []byte {
	function := declaration
	if nested := declaration.Get("function"); nested.Exists() && nested.IsObject() {
		function = nested
	}
	name := function.Get("name").String()
	if strings.TrimSpace(name) == "" {
		return nil
	}
	out := []byte(`{"name":"","parametersJsonSchema":{"type":"object","properties":{}}}`)
	out, _ = sjson.SetBytes(out, "name", util.MapSanitizedFunctionName(functionNameMap, name))
	if description := function.Get("description"); description.Exists() {
		out, _ = sjson.SetBytes(out, "description", description.String())
	}
	if parameters := function.Get("parametersJsonSchema"); parameters.Exists() {
		out, _ = sjson.SetRawBytes(out, "parametersJsonSchema", []byte(parameters.Raw))
	} else if parameters := function.Get("parameters"); parameters.Exists() {
		out, _ = sjson.SetRawBytes(out, "parametersJsonSchema", []byte(parameters.Raw))
	}
	if response := function.Get("response"); response.Exists() {
		out, _ = sjson.SetRawBytes(out, "response", []byte(response.Raw))
	}
	if responseSchema := function.Get("responseJsonSchema"); responseSchema.Exists() {
		out, _ = sjson.SetRawBytes(out, "responseJsonSchema", []byte(responseSchema.Raw))
	}
	out, _ = sjson.DeleteBytes(out, "strict")
	return out
}

func interactionsNativeAntigravityPart(part gjson.Result) []byte {
	switch {
	case part.Get("text").Exists(), part.Get("functionCall").Exists(), part.Get("functionResponse").Exists():
		return []byte(part.Raw)
	case part.Get("inlineData").Exists():
		return antigravityInlineDataPartJSON(part.Get("inlineData"))
	case part.Get("fileData").Exists():
		return antigravityFileDataPartJSON(part.Get("fileData"))
	case part.Get("inline_data").Exists():
		return antigravityInlineDataPartJSON(part.Get("inline_data"))
	case part.Get("file_data").Exists():
		return antigravityFileDataPartJSON(part.Get("file_data"))
	}
	return nil
}

func antigravityTextPartJSON(text string, thought bool) []byte {
	partJSON := []byte(`{"text":""}`)
	partJSON, _ = sjson.SetBytes(partJSON, "text", text)
	if thought {
		partJSON, _ = sjson.SetBytes(partJSON, "thought", true)
	}
	return partJSON
}

func antigravityInlineDataPartJSON(inline gjson.Result) []byte {
	mimeType := inline.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inline.Get("mime_type").String()
	}
	data := inline.Get("data").String()
	if mimeType == "" || data == "" {
		return nil
	}
	partJSON := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	partJSON, _ = sjson.SetBytes(partJSON, "inlineData.mimeType", mimeType)
	partJSON, _ = sjson.SetBytes(partJSON, "inlineData.data", data)
	return partJSON
}

func antigravityFileDataPartJSON(fileData gjson.Result) []byte {
	mimeType := fileData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = fileData.Get("mime_type").String()
	}
	fileURI := fileData.Get("fileUri").String()
	if fileURI == "" {
		fileURI = fileData.Get("file_uri").String()
	}
	if mimeType == "" || fileURI == "" {
		return nil
	}
	partJSON := []byte(`{"fileData":{"mimeType":"","fileUri":""}}`)
	partJSON, _ = sjson.SetBytes(partJSON, "fileData.mimeType", mimeType)
	partJSON, _ = sjson.SetBytes(partJSON, "fileData.fileUri", fileURI)
	return partJSON
}

func antigravityInlineDataPartFromDataURL(dataURL string) []byte {
	if !strings.HasPrefix(dataURL, "data:") {
		return nil
	}
	payload := dataURL[5:]
	pieces := strings.SplitN(payload, ";", 2)
	if len(pieces) != 2 || !strings.HasPrefix(pieces[1], "base64,") {
		return nil
	}
	return antigravityInlineDataPartJSON(
		gjson.Parse(
			fmt.Sprintf(
				`{"mime_type":%q,"data":%q}`, pieces[0], pieces[1][7:],
			),
		),
	)
}

func appendAntigravityTextContent(out []byte, role, text string) []byte {
	contentObj := []byte(`{"role":"","parts":[{"text":""}]}`)
	contentObj, _ = sjson.SetBytes(contentObj, "role", antigravityContentRole(role, "user"))
	contentObj, _ = sjson.SetBytes(contentObj, "parts.0.text", text)
	out, _ = sjson.SetRawBytes(out, "request.contents.-1", contentObj)
	return out
}

func antigravityContentRole(role, defaultRole string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "model", "assistant":
		return "model"
	case "user":
		return "user"
	}
	if defaultRole == "model" {
		return "model"
	}
	return "user"
}

func antigravityInputAudioMimeType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "opus":
		return "audio/opus"
	case "pcm16":
		return "audio/pcm"
	default:
		return "audio/mpeg"
	}
}

func antigravityThinkingSummariesIncludeThoughts(summary gjson.Result) (bool, bool) {
	if summary.Type != gjson.String {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(summary.String())) {
	case "auto":
		return true, true
	case "none":
		return false, true
	default:
		return false, false
	}
}

func convertSnakeCaseKeysToCamelCaseForAntigravity(raw []byte) []byte {
	root := gjson.ParseBytes(raw)
	if !root.Exists() {
		return raw
	}
	out := []byte(`{}`)
	out = copySnakeCaseValueToCamelCaseForAntigravity(out, "", root)
	return out
}

func copySnakeCaseValueToCamelCaseForAntigravity(out []byte, path string, node gjson.Result) []byte {
	if node.IsObject() {
		node.ForEach(
			func(key, value gjson.Result) bool {
				childPath := joinAntigravityJSONPath(path, toAntigravityCamelCase(key.String()))
				out = copySnakeCaseValueToCamelCaseForAntigravity(out, childPath, value)
				return true
			},
		)
		return out
	}
	if node.IsArray() {
		node.ForEach(
			func(_, value gjson.Result) bool {
				out = copySnakeCaseValueToCamelCaseForAntigravity(out, path+".-1", value)
				return true
			},
		)
		return out
	}
	out, _ = sjson.SetRawBytes(out, path, []byte(node.Raw))
	return out
}

func joinAntigravityJSONPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func toAntigravityCamelCase(s string) string {
	parts := strings.Split(s, "_")
	if len(parts) == 0 {
		return s
	}
	var out strings.Builder
	out.WriteString(parts[0])
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		out.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return out.String()
}

func attachDefaultAntigravitySafetySettings(out []byte) []byte {
	if gjson.GetBytes(out, "request.safetySettings").Exists() {
		return out
	}
	settings := []map[string]string{
		{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"},
	}
	raw, errMarshal := json.Marshal(settings)
	if errMarshal != nil {
		return out
	}
	out, _ = sjson.SetRawBytes(out, "request.safetySettings", raw)
	return out
}

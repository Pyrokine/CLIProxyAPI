package executor

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeMCPAliasOptions struct {
	callerIdentity string
}

func resolveClaudeMCPAliasOptions(ctx context.Context) claudeMCPAliasOptions {
	// Alias identity belongs to the downstream caller, not to the selected
	// upstream credential. This keeps names stable across OAuth refresh and auth
	// failover while giving one caller a shared virtual MCP server component.
	callerIdentity := strings.TrimSpace(apiKeyFromContext(ctx))
	if callerIdentity == "" {
		callerIdentity = "cpa-claude-mcp-default-caller"
	}
	return claudeMCPAliasOptions{callerIdentity: callerIdentity}
}

// prepareClaudeOAuthToolNamesForUpstream applies one request-local MCP symbol
// table across every Claude OAuth request path.
func prepareClaudeOAuthToolNamesForUpstream(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string) {
	return remapOAuthToolNamesWithOptions(body, mcpAliases)
}

func restoreClaudeOAuthToolNamesFromResponse(body []byte, reverseMap map[string]string) ([]byte, error) {
	return reverseRemapOAuthToolNames(body, reverseMap)
}

func restoreClaudeOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) ([]byte, error) {
	return reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
}

type claudeRawJSONEdit struct {
	start       int
	end         int
	replacement string
}

func remapOAuthToolNamesWithOptions(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string) {
	remapped, reverseMap, ok := remapOAuthToolNamesWithBatchedEdits(body, mcpAliases)
	if ok {
		return remapped, reverseMap
	}
	return remapOAuthToolNamesWithOptionsLegacy(body, mcpAliases)
}

// remapOAuthToolNamesWithBatchedEdits records offsets from the original JSON
// and applies every rename in one copy. Repeated sjson.SetBytes calls copy most
// of the request for every historical tool reference, turning this path into
// O(body size * reference count) allocation growth.
func remapOAuthToolNamesWithBatchedEdits(body []byte, mcpAliases claudeMCPAliasOptions) (
	[]byte,
	map[string]string,
	bool,
) {
	if !gjson.ValidBytes(body) {
		return nil, nil, false
	}

	reverseMap := make(map[string]string)
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// Build one request-specific forward map from declarations. Every client
	// tool, including typed custom declarations and names resembling Claude
	// built-ins, gets an MCP alias. Historical references use this same map.
	tools := gjson.GetBytes(body, "tools")
	forwardMap := make(map[string]string)
	protectedNames := make(map[string]bool)
	reservedNames := helps.AugmentClaudeBuiltinToolRegistry(body, nil)
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				name := tool.Get("name").String()
				if name != "" {
					reservedNames[name] = true
				}
				if helps.IsClaudeServerToolType(tool.Get("type").String()) {
					protectedNames[name] = true
				}
				return true
			},
		)
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if helps.IsClaudeServerToolType(tool.Get("type").String()) {
					return true
				}
				name := tool.Get("name").String()
				if name == "" || helps.IsClaudeMCPToolName(name) {
					return true
				}
				if _, exists := forwardMap[name]; exists {
					return true
				}
				for attempt := uint32(0); ; attempt++ {
					alias := helps.ClaudeMCPToolAlias(mcpAliases.callerIdentity, name, attempt)
					if reservedNames[alias] {
						continue
					}
					forwardMap[name] = alias
					reservedNames[alias] = true
					break
				}
				return true
			},
		)
	}

	rewriteName := func(name string) (string, bool) {
		if name == "" || protectedNames[name] || helps.IsClaudeMCPToolName(name) {
			return name, false
		}
		if newName, ok := forwardMap[name]; ok && newName != name {
			return newName, true
		}
		return name, false
	}

	edits := make([]claudeRawJSONEdit, 0, len(forwardMap)+1)
	appendRawEdit := func(result gjson.Result, replacement string) bool {
		start := result.Index
		end := start + len(result.Raw)
		if result.Raw == "" || start < 0 || end < start || end > len(body) || !bytes.Equal(
			body[start:end], []byte(result.Raw),
		) {
			return false
		}
		edits = append(edits, claudeRawJSONEdit{start: start, end: end, replacement: replacement})
		return true
	}
	appendStringEdit := func(result gjson.Result, replacement string) bool {
		// ClaudeMCPToolAlias only emits [A-Za-z0-9_-], so adding quotes is
		// byte-identical to sjson's encoding without another allocation.
		return appendRawEdit(result, `"`+replacement+`"`)
	}

	// 1. Rebuild typed custom tools exactly as before, but replace the original
	// tools array only after all offsets have been collected.
	toolsNeedRewrite := false
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				toolType := tool.Get("type").String()
				if helps.IsClaudeServerToolType(toolType) {
					return true
				}
				if strings.TrimSpace(toolType) != "" {
					toolsNeedRewrite = true
					return false
				}
				name := tool.Get("name").String()
				_, toolsNeedRewrite = rewriteName(name)
				return !toolsNeedRewrite
			},
		)
	}
	if toolsNeedRewrite {
		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if helps.IsClaudeServerToolType(tool.Get("type").String()) {
					if toolCount > 0 {
						toolsJSON.WriteByte(',')
					}
					toolsJSON.WriteString(tool.Raw)
					toolCount++
					return true
				}

				name := tool.Get("name").String()
				toolJSON := tool.Raw
				if strings.TrimSpace(tool.Get("type").String()) != "" {
					if updatedTool, errDelete := sjson.Delete(toolJSON, "type"); errDelete == nil {
						toolJSON = updatedTool
					}
				}
				if newName, renamed := rewriteName(name); renamed {
					updatedTool, err := sjson.Set(toolJSON, "name", newName)
					if err == nil {
						toolJSON = updatedTool
						recordRename(name, newName)
					}
				}

				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(toolJSON)
				toolCount++
				return true
			},
		)
		toolsJSON.WriteByte(']')
		if !appendRawEdit(tools, toolsJSON.String()) {
			return nil, nil, false
		}
	}

	// 2. Rename tool_choice if it references a declared client tool.
	toolChoice := gjson.GetBytes(body, "tool_choice")
	if toolChoice.Get("type").String() == "tool" {
		nameResult := toolChoice.Get("name")
		tcName := nameResult.String()
		if newName, renamed := rewriteName(tcName); renamed {
			if !appendStringEdit(nameResult, newName) {
				return nil, nil, false
			}
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages while every Result.Index still
	// points into the original request bytes.
	messages := gjson.GetBytes(body, "messages")
	validOffsets := true
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(
			func(_, msg gjson.Result) bool {
				content := msg.Get("content")
				if !content.Exists() || !content.IsArray() {
					return true
				}
				content.ForEach(
					func(_, part gjson.Result) bool {
						switch part.Get("type").String() {
						case "tool_use":
							nameResult := part.Get("name")
							name := nameResult.String()
							if newName, renamed := rewriteName(name); renamed {
								if !appendStringEdit(nameResult, newName) {
									validOffsets = false
									return false
								}
								recordRename(name, newName)
							}
						case "tool_reference":
							nameResult := part.Get("tool_name")
							toolName := nameResult.String()
							if newName, renamed := rewriteName(toolName); renamed {
								if !appendStringEdit(nameResult, newName) {
									validOffsets = false
									return false
								}
								recordRename(toolName, newName)
							}
						case "tool_result":
							nestedContent := part.Get("content")
							if nestedContent.Exists() && nestedContent.IsArray() {
								nestedContent.ForEach(
									func(_, nestedPart gjson.Result) bool {
										if nestedPart.Get("type").String() != "tool_reference" {
											return true
										}
										nameResult := nestedPart.Get("tool_name")
										nestedToolName := nameResult.String()
										if newName, renamed := rewriteName(nestedToolName); renamed {
											if !appendStringEdit(nameResult, newName) {
												validOffsets = false
												return false
											}
											recordRename(nestedToolName, newName)
										}
										return true
									},
								)
							}
						}
						return validOffsets
					},
				)
				return validOffsets
			},
		)
	}
	if !validOffsets {
		return nil, nil, false
	}

	remapped, ok := applyClaudeRawJSONEdits(body, edits)
	if !ok {
		return nil, nil, false
	}
	return remapped, reverseMap, true
}

func applyClaudeRawJSONEdits(body []byte, edits []claudeRawJSONEdit) ([]byte, bool) {
	if len(edits) == 0 {
		return body, true
	}
	sort.Slice(
		edits, func(i, j int) bool {
			return edits[i].start < edits[j].start
		},
	)

	finalSize := len(body)
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor || edit.start < 0 || edit.end < edit.start || edit.end > len(body) {
			return nil, false
		}
		finalSize += len(edit.replacement) - (edit.end - edit.start)
		if finalSize < 0 {
			return nil, false
		}
		cursor = edit.end
	}

	out := make([]byte, 0, finalSize)
	cursor = 0
	for _, edit := range edits {
		out = append(out, body[cursor:edit.start]...)
		out = append(out, edit.replacement...)
		cursor = edit.end
	}
	out = append(out, body[cursor:]...)
	return out, true
}

// remapOAuthToolNamesWithOptionsLegacy is the byte-for-byte compatibility
// fallback for malformed JSON or an unexpected GJSON offset. Keep it available
// as a differential-test oracle for the batched implementation.
func remapOAuthToolNamesWithOptionsLegacy(body []byte, mcpAliases claudeMCPAliasOptions) ([]byte, map[string]string) {
	reverseMap := make(map[string]string)
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// Build one request-specific forward map from declarations. Every client
	// tool, including typed custom declarations and names resembling Claude
	// built-ins, gets an MCP alias. Historical references use this same map.
	tools := gjson.GetBytes(body, "tools")
	forwardMap := make(map[string]string)
	protectedNames := make(map[string]bool)
	reservedNames := helps.AugmentClaudeBuiltinToolRegistry(body, nil)
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				name := tool.Get("name").String()
				if name != "" {
					reservedNames[name] = true
				}
				if helps.IsClaudeServerToolType(tool.Get("type").String()) {
					protectedNames[name] = true
				}
				return true
			},
		)
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if helps.IsClaudeServerToolType(tool.Get("type").String()) {
					return true
				}
				name := tool.Get("name").String()
				if name == "" || helps.IsClaudeMCPToolName(name) {
					return true
				}
				if _, exists := forwardMap[name]; exists {
					return true
				}
				for attempt := uint32(0); ; attempt++ {
					alias := helps.ClaudeMCPToolAlias(mcpAliases.callerIdentity, name, attempt)
					if reservedNames[alias] {
						continue
					}
					forwardMap[name] = alias
					reservedNames[alias] = true
					break
				}
				return true
			},
		)
	}

	rewriteName := func(name string) (string, bool) {
		if name == "" || protectedNames[name] || helps.IsClaudeMCPToolName(name) {
			return name, false
		}
		if newName, ok := forwardMap[name]; ok && newName != name {
			return newName, true
		}
		return name, false
	}

	// 1. Rewrite the tools array without rebuilding from a stale gjson snapshot.
	toolsNeedRewrite := false
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				toolType := tool.Get("type").String()
				if helps.IsClaudeServerToolType(toolType) {
					return true
				}
				if strings.TrimSpace(toolType) != "" {
					toolsNeedRewrite = true
					return false
				}
				name := tool.Get("name").String()
				_, toolsNeedRewrite = rewriteName(name)
				return !toolsNeedRewrite
			},
		)
	}
	if toolsNeedRewrite {
		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if helps.IsClaudeServerToolType(tool.Get("type").String()) {
					if toolCount > 0 {
						toolsJSON.WriteByte(',')
					}
					toolsJSON.WriteString(tool.Raw)
					toolCount++
					return true
				}

				name := tool.Get("name").String()
				toolJSON := tool.Raw
				if strings.TrimSpace(tool.Get("type").String()) != "" {
					if updatedTool, errDelete := sjson.Delete(toolJSON, "type"); errDelete == nil {
						toolJSON = updatedTool
					}
				}
				if newName, renamed := rewriteName(name); renamed {
					updatedTool, err := sjson.Set(toolJSON, "name", newName)
					if err == nil {
						toolJSON = updatedTool
						recordRename(name, newName)
					}
				}

				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(toolJSON)
				toolCount++
				return true
			},
		)
		toolsJSON.WriteByte(']')
		body, _ = sjson.SetRawBytes(body, "tools", []byte(toolsJSON.String()))
	}

	// 2. Rename tool_choice if it references a declared client tool.
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	if toolChoiceType == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if newName, renamed := rewriteName(tcName); renamed {
			body, _ = sjson.SetBytes(body, "tool_choice.name", newName)
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(
			func(msgIndex, msg gjson.Result) bool {
				content := msg.Get("content")
				if !content.Exists() || !content.IsArray() {
					return true
				}
				content.ForEach(
					func(contentIndex, part gjson.Result) bool {
						partType := part.Get("type").String()
						switch partType {
						case "tool_use":
							name := part.Get("name").String()
							if newName, renamed := rewriteName(name); renamed {
								path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
								body, _ = sjson.SetBytes(body, path, newName)
								recordRename(name, newName)
							}
						case "tool_reference":
							toolName := part.Get("tool_name").String()
							if newName, renamed := rewriteName(toolName); renamed {
								path := fmt.Sprintf(
									"messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(),
								)
								body, _ = sjson.SetBytes(body, path, newName)
								recordRename(toolName, newName)
							}
						case "tool_result":
							// Handle nested tool_reference blocks inside tool_result.content[]
							toolID := part.Get("tool_use_id").String()
							_ = toolID // tool_use_id stays as-is
							nestedContent := part.Get("content")
							if nestedContent.Exists() && nestedContent.IsArray() {
								nestedContent.ForEach(
									func(nestedIndex, nestedPart gjson.Result) bool {
										if nestedPart.Get("type").String() == "tool_reference" {
											nestedToolName := nestedPart.Get("tool_name").String()
											if newName, renamed := rewriteName(nestedToolName); renamed {
												nestedPath := fmt.Sprintf(
													"messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(),
													contentIndex.Int(), nestedIndex.Int(),
												)
												body, _ = sjson.SetBytes(body, nestedPath, newName)
												recordRename(nestedToolName, newName)
											}
										}
										return true
									},
								)
							}
						}
						return true
					},
				)
				return true
			},
		)
	}

	return body, reverseMap
}

type claudeMCPAliasParts struct {
	server   string
	toolID   string
	semantic string
}

type claudeMCPAliasEntry struct {
	alias    string
	original string
	parts    claudeMCPAliasParts
}

type claudeMCPAliasResolver struct {
	exact   map[string]string
	aliases []claudeMCPAliasEntry
	servers map[string]struct{}
}

func newClaudeMCPAliasResolver(reverseMap map[string]string) claudeMCPAliasResolver {
	resolver := claudeMCPAliasResolver{
		exact:   reverseMap,
		aliases: make([]claudeMCPAliasEntry, 0, len(reverseMap)),
		servers: make(map[string]struct{}),
	}
	for alias, original := range reverseMap {
		parts, ok := parseClaudeMCPAlias(alias)
		if !ok {
			continue
		}
		resolver.aliases = append(
			resolver.aliases, claudeMCPAliasEntry{
				alias:    alias,
				original: original,
				parts:    parts,
			},
		)
		resolver.servers[parts.server] = struct{}{}
	}
	return resolver
}

func parseClaudeMCPAlias(name string) (claudeMCPAliasParts, bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return claudeMCPAliasParts{}, false
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || !isClaudeMCPAliasDigest(server) {
		return claudeMCPAliasParts{}, false
	}
	toolID, semantic, ok := strings.Cut(tool, "_")
	if !ok || !isClaudeMCPAliasDigest(toolID) || semantic == "" {
		return claudeMCPAliasParts{}, false
	}
	return claudeMCPAliasParts{server: server, toolID: toolID, semantic: semantic}, true
}

func isClaudeMCPAliasDigest(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func claudeMCPAliasServer(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

func (resolver claudeMCPAliasResolver) resolve(name string) (string, bool, error) {
	if original, ok := resolver.exact[name]; ok {
		return original, true, nil
	}

	server := claudeMCPAliasServer(name)
	if _, known := resolver.servers[server]; !known {
		return "", false, nil
	}

	matchedOriginal := ""
	matchCount := 0
	for _, entry := range resolver.aliases {
		if entry.parts.server == server && strings.HasSuffix(name, entry.alias) {
			matchedOriginal = entry.original
			matchCount++
		}
	}
	if matchCount == 1 {
		return matchedOriginal, true, nil
	}
	if matchCount > 1 {
		return "", false, fmt.Errorf(
			"cannot restore Claude OAuth MCP tool alias %q: matched multiple declared aliases", name,
		)
	}

	parts, ok := parseClaudeMCPAlias(name)
	if ok {
		for _, entry := range resolver.aliases {
			if entry.parts.server == parts.server && entry.parts.semantic == parts.semantic {
				matchedOriginal = entry.original
				matchCount++
			}
		}
		if matchCount == 1 {
			return matchedOriginal, true, nil
		}
		if matchCount > 1 {
			return "", false, fmt.Errorf(
				"cannot restore Claude OAuth MCP tool alias %q: semantic suffix matches multiple declared tools", name,
			)
		}
	}

	return "", false, fmt.Errorf("cannot restore Claude OAuth MCP tool alias %q: no unique request-local match", name)
}

// reverseRemapOAuthToolNames reverses the tool name mapping for non-stream responses
// using the per-request map produced by remapOAuthToolNames. Names outside the
// request-local generated MCP server are passed through unchanged.
func reverseRemapOAuthToolNames(body []byte, reverseMap map[string]string) ([]byte, error) {
	if len(reverseMap) == 0 {
		return body, nil
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body, nil
	}
	resolver := newClaudeMCPAliasResolver(reverseMap)
	var resolveErr error
	content.ForEach(
		func(index, part gjson.Result) bool {
			partType := part.Get("type").String()
			switch partType {
			case "tool_use":
				name := part.Get("name").String()
				origName, matched, errResolve := resolver.resolve(name)
				if errResolve != nil {
					resolveErr = errResolve
					return false
				}
				if matched {
					path := fmt.Sprintf("content.%d.name", index.Int())
					body, _ = sjson.SetBytes(body, path, origName)
				}
			case "tool_reference":
				toolName := part.Get("tool_name").String()
				origName, matched, errResolve := resolver.resolve(toolName)
				if errResolve != nil {
					resolveErr = errResolve
					return false
				}
				if matched {
					path := fmt.Sprintf("content.%d.tool_name", index.Int())
					body, _ = sjson.SetBytes(body, path, origName)
				}
			case "tool_result":
				nestedContent := part.Get("content")
				if nestedContent.Exists() && nestedContent.IsArray() {
					nestedContent.ForEach(
						func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() != "tool_reference" {
								return true
							}
							toolName := nestedPart.Get("tool_name").String()
							origName, matched, errResolve := resolver.resolve(toolName)
							if errResolve != nil {
								resolveErr = errResolve
								return false
							}
							if matched {
								path := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
								body, _ = sjson.SetBytes(body, path, origName)
							}
							return true
						},
					)
				}
			}
			return resolveErr == nil
		},
	)
	return body, resolveErr
}

// reverseRemapOAuthToolNamesFromStreamLine reverses the tool name mapping for SSE
// lines using the per-request reverseMap produced by remapOAuthToolNames.
func reverseRemapOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) ([]byte, error) {
	if len(reverseMap) == 0 {
		return line, nil
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line, nil
	}

	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line, nil
	}

	resolver := newClaudeMCPAliasResolver(reverseMap)
	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		origName, matched, errResolve := resolver.resolve(name)
		if errResolve != nil {
			return line, errResolve
		}
		if !matched {
			return line, nil
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", origName)
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		origName, matched, errResolve := resolver.resolve(toolName)
		if errResolve != nil {
			return line, errResolve
		}
		if !matched {
			return line, nil
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", origName)
	default:
		return line, nil
	}
	if err != nil {
		return line, fmt.Errorf("rewrite Claude OAuth MCP tool alias: %w", err)
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...), nil
	}
	return updated, nil
}

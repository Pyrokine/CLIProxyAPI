package amp

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responseRewriter wraps a gin.ResponseWriter to intercept and modify the response body
// It's used to rewrite model names in responses when model mapping is used
type responseRewriter struct {
	gin.ResponseWriter
	body             *bytes.Buffer
	originalModel    string
	isStreaming      bool
	requestToolNames map[string]string
}

// newResponseRewriter creates a new response rewriter for model name substitution
func newResponseRewriter(w gin.ResponseWriter, originalModel string) *responseRewriter {
	return &responseRewriter{
		ResponseWriter: w,
		body:           &bytes.Buffer{},
		originalModel:  originalModel,
	}
}

func newResponseRewriterForRequest(w gin.ResponseWriter, originalModel string, requestBody []byte) *responseRewriter {
	rw := newResponseRewriter(w, originalModel)
	rw.requestToolNames = collectRequestToolNames(requestBody)
	return rw
}

// Write intercepts response writes and buffers them for model name replacement
func (rw *responseRewriter) Write(data []byte) (int, error) {
	// Detect streaming on first write
	if rw.body.Len() == 0 && !rw.isStreaming {
		contentType := rw.Header().Get("Content-Type")
		rw.isStreaming = strings.Contains(contentType, "text/event-stream") ||
			strings.Contains(contentType, "stream")
	}

	if rw.isStreaming {
		n, err := rw.ResponseWriter.Write(rw.rewriteStreamChunk(data))
		if err == nil {
			if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		return n, err
	}
	return rw.body.Write(data)
}

// Flush writes the buffered response with model names rewritten
func (rw *responseRewriter) Flush() {
	if rw.isStreaming {
		if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	if rw.body.Len() > 0 {
		if _, err := rw.ResponseWriter.Write(rw.rewriteModelInResponse(rw.body.Bytes())); err != nil {
			log.Warnf("amp response rewriter: failed to write rewritten response: %v", err)
		}
	}
}

// modelFieldPaths lists all JSON paths where model name may appear
var modelFieldPaths = []string{"message.model", "model", "modelVersion", "response.model", "response.modelVersion"}

var ampCanonicalToolNames = map[string]string{
	"bash":  "Bash",
	"read":  "Read",
	"grep":  "Grep",
	"glob":  "glob",
	"task":  "Task",
	"check": "Check",
}

func collectRequestToolNames(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}
	parsed := gjson.ParseBytes(data)
	names := map[string]string{}
	conflicts := map[string]bool{}
	record := func(name string) {
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if conflicts[key] {
			return
		}
		if existing, exists := names[key]; exists {
			if existing != name {
				names[key] = ""
				conflicts[key] = true
			}
			return
		}
		names[key] = name
	}
	for _, tool := range parsed.Get("tools").Array() {
		record(tool.Get("name").String())
	}
	if parsed.Get("tool_choice.type").String() == "tool" {
		record(parsed.Get("tool_choice.name").String())
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

func canonicalAmpToolName(name string, requestToolNames map[string]string) (string, bool) {
	key := strings.ToLower(name)
	if canonical, ok := requestToolNames[key]; ok {
		if canonical == "" {
			return "", false
		}
		return canonical, true
	}
	canonical, ok := ampCanonicalToolNames[key]
	return canonical, ok
}

func normalizeAmpToolNamesForRequest(data []byte, requestToolNames map[string]string) []byte {
	for index, block := range gjson.GetBytes(data, "content").Array() {
		if block.Get("type").String() != "tool_use" {
			continue
		}
		name := block.Get("name").String()
		if canonical, ok := canonicalAmpToolName(name, requestToolNames); ok && name != canonical {
			path := fmt.Sprintf("content.%d.name", index)
			updated, err := sjson.SetBytes(data, path, canonical)
			if err != nil {
				log.Warnf("Amp ResponseRewriter: failed to normalize tool name %q to %q: %v", name, canonical, err)
				continue
			}
			data = updated
		}
	}
	if gjson.GetBytes(data, "content_block.type").String() == "tool_use" {
		name := gjson.GetBytes(data, "content_block.name").String()
		if canonical, ok := canonicalAmpToolName(name, requestToolNames); ok && name != canonical {
			updated, err := sjson.SetBytes(data, "content_block.name", canonical)
			if err != nil {
				log.Warnf(
					"Amp ResponseRewriter: failed to normalize streaming tool name %q to %q: %v", name, canonical, err,
				)
			} else {
				data = updated
			}
		}
	}
	return data
}

func (rw *responseRewriter) normalizeToolNames(data []byte) []byte {
	return normalizeAmpToolNamesForRequest(data, rw.requestToolNames)
}

func ensureAmpSignature(data []byte) []byte {
	for index, block := range gjson.GetBytes(data, "content").Array() {
		blockType := block.Get("type").String()
		if blockType != "tool_use" && blockType != "thinking" {
			continue
		}
		signaturePath := fmt.Sprintf("content.%d.signature", index)
		if gjson.GetBytes(data, signaturePath).Exists() {
			continue
		}
		updated, err := sjson.SetBytes(data, signaturePath, "")
		if err != nil {
			log.Warnf("Amp ResponseRewriter: failed to add empty signature to %s block: %v", blockType, err)
			continue
		}
		data = updated
	}

	contentBlockType := gjson.GetBytes(data, "content_block.type").String()
	if (contentBlockType == "tool_use" || contentBlockType == "thinking") && !gjson.GetBytes(
		data, "content_block.signature",
	).Exists() {
		updated, err := sjson.SetBytes(data, "content_block.signature", "")
		if err != nil {
			log.Warnf(
				"Amp ResponseRewriter: failed to add empty signature to streaming %s block: %v", contentBlockType, err,
			)
		} else {
			data = updated
		}
	}

	return data
}

// rewriteModelInResponse replaces all occurrences of the mapped model with the original model in JSON
// It also suppresses "thinking" blocks if "tool_use" is present to ensure Amp client compatibility
func (rw *responseRewriter) rewriteModelInResponse(data []byte) []byte {
	data = rw.normalizeToolNames(data)
	data = ensureAmpSignature(data)

	// 1. Amp Compatibility: Suppress thinking blocks if tool use is detected
	// The Amp client struggles when both thinking and tool_use blocks are present
	if gjson.GetBytes(data, `content.#(type=="tool_use")`).Exists() {
		filtered := gjson.GetBytes(data, `content.#(type!="thinking")#`)
		if filtered.Exists() {
			originalCount := gjson.GetBytes(data, "content.#").Int()
			filteredCount := filtered.Get("#").Int()

			if originalCount > filteredCount {
				var err error
				data, err = sjson.SetBytes(data, "content", filtered.Value())
				if err != nil {
					log.Warnf("Amp ResponseRewriter: failed to suppress thinking blocks: %v", err)
				} else {
					log.Debugf(
						"Amp ResponseRewriter: Suppressed %d thinking blocks due to tool usage",
						originalCount-filteredCount,
					)
					// Log the result for verification
					log.Debugf("Amp ResponseRewriter: Resulting content: %s", gjson.GetBytes(data, "content").String())
				}
			}
		}
	}

	if rw.originalModel == "" {
		return data
	}
	for _, path := range modelFieldPaths {
		if gjson.GetBytes(data, path).Exists() {
			data, _ = sjson.SetBytes(data, path, rw.originalModel)
		}
	}
	return data
}

func (rw *responseRewriter) rewriteStreamEvent(data []byte) []byte {
	data = rw.normalizeToolNames(data)
	data = ensureAmpSignature(data)
	if rw.originalModel != "" {
		for _, path := range modelFieldPaths {
			if gjson.GetBytes(data, path).Exists() {
				data, _ = sjson.SetBytes(data, path, rw.originalModel)
			}
		}
	}
	return data
}

// rewriteStreamChunk rewrites model names in SSE stream chunks
func (rw *responseRewriter) rewriteStreamChunk(chunk []byte) []byte {
	// SSE format: "data: {json}\n\n"
	lines := bytes.Split(chunk, []byte("\n"))
	for i, line := range lines {
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			jsonData := bytes.TrimSpace(after)
			if len(jsonData) > 0 && jsonData[0] == '{' {
				// Rewrite JSON in the data line
				rewritten := rw.rewriteStreamEvent(jsonData)
				lines[i] = append([]byte("data: "), rewritten...)
			}
		}
	}

	return bytes.Join(lines, []byte("\n"))
}

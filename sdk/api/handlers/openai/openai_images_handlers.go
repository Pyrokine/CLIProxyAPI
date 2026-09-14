package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultImagesMainModel            = "gpt-5.4-mini"
	defaultImagesToolModel            = "gpt-image-2"
	defaultXAIImagesModel             = "grok-imagine-image"
	xaiImagesQualityModel             = "grok-imagine-image-quality"
	xaiImagesHandlerType              = "openai-image"
	xaiImagesDefaultAspectRatio       = "1:1"
	xaiImagesDefaultResolution        = "1k"
	imagesGenerationsPath             = "/v1/images/generations"
	imagesEditsPath                   = "/v1/images/edits"
	maxImagesEditMultipartBodyBytes   = 50 << 20
	maxImagesEditMultipartMemoryBytes = 8 << 20
	maxResponsesSSEFrameBytes         = 50 << 20
)

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

type xaiImageResult struct {
	B64JSON       string
	URL           string
	RevisedPrompt string
	MimeType      string
}

type materializedImageArtifact struct {
	record     handlers.ImageArtifactRecord
	contentURL string
	b64JSON    string
}

type sseFrameAccumulator struct {
	pending              []byte
	maxPending           int
	validateDataJSON     bool
	sawResponsesTerminal bool
	err                  error
}

func newBoundedSSEFrameAccumulator(maxPending int) *sseFrameAccumulator {
	return &sseFrameAccumulator{maxPending: maxPending}
}

func newResponsesSSEFrameAccumulator() *sseFrameAccumulator {
	return &sseFrameAccumulator{
		maxPending:       maxResponsesSSEFrameBytes,
		validateDataJSON: true,
	}
}

func (a *sseFrameAccumulator) appendFrame(frames *[][]byte, frame []byte) bool {
	if a.validateDataJSON {
		if a.sawResponsesTerminal {
			return true
		}
		payload, hasData, valid := responsesSSEDataPayload(frame)
		if hasData && !valid {
			a.pending = nil
			a.err = fmt.Errorf("responses SSE stream ended with malformed data")
			return false
		}
		if hasData && !bytes.Equal(payload, []byte("[DONE]")) {
			switch strings.TrimSpace(gjson.GetBytes(payload, "type").String()) {
			case "response.completed", "response.done", "response.incomplete":
				a.sawResponsesTerminal = true
			}
		}
	}
	*frames = append(*frames, frame)
	return true
}

func (a *sseFrameAccumulator) frameExceedsLimit(size int) bool {
	if a == nil || a.maxPending <= 0 || size <= a.maxPending {
		return false
	}
	a.pending = nil
	streamName := "image"
	if a.validateDataJSON {
		streamName = "Responses"
	}
	a.err = fmt.Errorf("%s SSE frame exceeds maximum allowed size of %d bytes", streamName, a.maxPending)
	return true
}

func (a *sseFrameAccumulator) AddChunk(chunk []byte) [][]byte {
	if a == nil || a.err != nil || len(chunk) == 0 {
		return nil
	}

	var frames [][]byte
	if len(a.pending) > 0 {
		prefixLen := responsesSSEPrefixLenForPending(a.pending, chunk)
		if prefixLen == 0 {
			if a.frameExceedsLimit(len(a.pending) + len(chunk)) {
				return frames
			}
			a.pending = append(a.pending, chunk...)
			return frames
		}
		if a.frameExceedsLimit(len(a.pending) + prefixLen) {
			return frames
		}
		frame := make([]byte, 0, len(a.pending)+prefixLen)
		frame = append(frame, a.pending...)
		frame = append(frame, chunk[:prefixLen]...)
		a.pending = nil
		if !a.appendFrame(&frames, frame) {
			return frames
		}
		chunk = chunk[prefixLen:]
	}

	consumed := 0
	for consumed < len(chunk) {
		frameLen := responsesSSEFrameLen(chunk[consumed:])
		if frameLen == 0 {
			break
		}
		if a.frameExceedsLimit(frameLen) {
			return frames
		}
		frame := append([]byte(nil), chunk[consumed:consumed+frameLen]...)
		consumed += frameLen
		if !a.appendFrame(&frames, frame) {
			return frames
		}
	}

	remaining := chunk[consumed:]
	if len(remaining) == 0 {
		return frames
	}
	if a.frameExceedsLimit(len(remaining)) {
		return frames
	}
	if len(bytes.TrimSpace(remaining)) == 0 {
		a.pending = nil
		return frames
	}
	a.pending = append(a.pending[:0], remaining...)
	return frames
}

func (a *sseFrameAccumulator) Flush() [][]byte {
	if a == nil || a.err != nil || len(a.pending) == 0 {
		return nil
	}

	pending := a.pending
	a.pending = nil
	var frames [][]byte
	consumed := 0
	for consumed < len(pending) {
		frameLen := responsesSSEFrameLen(pending[consumed:])
		if frameLen == 0 {
			break
		}
		frame := append([]byte(nil), pending[consumed:consumed+frameLen]...)
		consumed += frameLen
		if !a.appendFrame(&frames, frame) {
			return frames
		}
	}

	remaining := pending[consumed:]
	if len(bytes.TrimSpace(remaining)) == 0 {
		return frames
	}
	if a.validateDataJSON && responsesSSEHasField(remaining, []byte("data:")) {
		a.err = fmt.Errorf("responses SSE stream ended with malformed data")
	} else if imageSSEHasData(remaining) {
		a.err = fmt.Errorf("image SSE stream ended with malformed data")
	}
	return frames
}

func (a *sseFrameAccumulator) Finalize() [][]byte {
	frames := a.Flush()
	if a != nil && a.validateDataJSON && a.err == nil && !a.sawResponsesTerminal {
		a.err = fmt.Errorf("responses SSE stream ended before response.completed")
	}
	return frames
}

func imageSSEHasData(frame []byte) bool {
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		if len(bytes.TrimSpace(trimmed[len("data:"):])) > 0 {
			return true
		}
	}
	return false
}

func (a *sseFrameAccumulator) Err() error {
	if a == nil {
		return nil
	}
	return a.err
}

func withDeferredImageUsage(ctx context.Context) (context.Context, *cliproxyusage.DeferredRecord) {
	deferred := cliproxyusage.NewDeferredRecord()
	return cliproxyusage.WithDeferredRecord(ctx, deferred), deferred
}

type imageHandlerStatusError struct {
	statusCode int
	err        error
}

func (e *imageHandlerStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("image request failed with status %d", e.statusCode)
}

func (e *imageHandlerStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *imageHandlerStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

func imageHandlerError(errMsg *handlers.ErrorMessage) error {
	if errMsg == nil {
		return nil
	}
	status := errMsg.StatusCode
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	return &imageHandlerStatusError{statusCode: status, err: errMsg.Error}
}

func imageErrorMessage(err error, fallbackStatus int) *handlers.ErrorMessage {
	if fallbackStatus <= 0 {
		fallbackStatus = http.StatusInternalServerError
	}
	return &handlers.ErrorMessage{
		StatusCode: clienterror.HTTPStatusFromErrorOr(err, fallbackStatus),
		Error:      err,
	}
}

type imageSSEFrameInfo struct {
	sawPayload bool
	terminal   bool
	err        error
}

func inspectImageSSEFrame(frame []byte) imageSSEFrameInfo {
	var info imageSSEFrameInfo
	payload, hasData, valid := responsesSSEDataPayload(frame)
	if !hasData {
		return info
	}
	if !valid {
		info.err = fmt.Errorf("invalid image SSE data JSON")
		return info
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		info.terminal = true
		return info
	}
	info.sawPayload = true
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	switch eventType {
	case "error", "response.failed":
		info.err = imageSSEEventError(payload)
	case "response.completed", "image_generation.completed", "image_edit.completed":
		info.terminal = true
	}
	return info
}

func imageSSEEventError(payload []byte) error {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "response.status_code").Int())
	}
	if status <= 0 {
		status = http.StatusBadGateway
	}
	message := ""
	for _, path := range []string{"error.message", "response.error.message", "message"} {
		if value := strings.TrimSpace(gjson.GetBytes(payload, path).String()); value != "" {
			message = value
			break
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &imageHandlerStatusError{statusCode: status, err: fmt.Errorf("%s", message)}
}

func imagesModelParts(model string) (prefix string, baseModel string) {
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return strings.TrimSpace(model[:idx]), strings.TrimSpace(model[idx+1:])
	}
	return "", model
}

func imagesModelBase(model string) string {
	_, baseModel := imagesModelParts(model)
	return strings.ToLower(strings.TrimSpace(baseModel))
}

func isXAIImagesModel(model string) bool {
	prefix, baseModel := imagesModelParts(model)
	baseModel = strings.ToLower(strings.TrimSpace(baseModel))
	if baseModel != defaultXAIImagesModel && baseModel != xaiImagesQualityModel {
		return false
	}

	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" || prefix == "xai" || prefix == "x-ai" || prefix == "grok" {
		return true
	}

	registeredModel := registry.GetGlobalRegistry().ResolveModelIDCaseInsensitive(
		strings.TrimSpace(thinking.ParseSuffix(model).ModelName),
	)
	if registeredModel == "" {
		return false
	}
	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(registeredModel) {
		if strings.EqualFold(strings.TrimSpace(provider), "xai") {
			return true
		}
	}
	return false
}

func isCodexImagesToolModel(model string) bool {
	return registry.IsCodexImageModel(imagesModelBase(model))
}

func isOpenAICompatImagesModel(model string) bool {
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if model == "" {
		return false
	}
	info := registry.LookupModelInfo(model)
	return info != nil && info.Type == registry.OpenAIImageModelType
}

func isSupportedImagesModel(model string) bool {
	if isCodexImagesToolModel(model) {
		return true
	}
	return isXAIImagesModel(model) || isOpenAICompatImagesModel(model)
}

func rejectUnsupportedImagesModel(c *gin.Context, model string) bool {
	if isSupportedImagesModel(model) {
		return false
	}

	c.JSON(
		http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf(
					"Model %s is not supported on %s or %s. Use one of %s, %s, %s, or a configured openai-compatibility image model.",
					model,
					imagesGenerationsPath,
					imagesEditsPath,
					strings.Join(registry.CodexImageModelIDs(), ", "),
					defaultXAIImagesModel,
					xaiImagesQualityModel,
				),
				Type: "invalid_request_error",
			},
		},
	)
	return true
}

func mimeTypeFromOutputFormat(outputFormat string) string {
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func buildOpenAICompatImagesJSONRequest(rawJSON []byte, imageModel string, stream bool) []byte {
	payload := rawJSON
	if model := strings.TrimSpace(imageModel); model != "" {
		payload, _ = sjson.SetBytes(payload, "model", model)
	}
	if stream {
		payload, _ = sjson.SetBytes(payload, "stream", true)
	} else {
		payload, _ = sjson.DeleteBytes(payload, "stream")
	}
	return payload
}

func cloneMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func buildOpenAICompatImagesMultipartRequest(form *multipart.Form, imageModel string, stream bool) (
	out []byte,
	contentType string,
	err error,
) {
	if form == nil {
		return nil, "", fmt.Errorf("multipart form is nil")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	defer func() {
		if err != nil {
			_ = writer.Close()
		}
	}()
	if errWrite := writer.WriteField("model", imageModel); errWrite != nil {
		return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}
	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			header := cloneMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			_, errCopy := io.Copy(part, src)
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai images: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func multipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	if fileHeader.Size > maxImagesEditMultipartBodyBytes {
		return "", fmt.Errorf("upload file exceeds %d bytes", maxImagesEditMultipartBodyBytes)
	}
	f, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open upload file failed: %w", err)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("openai images: close upload file error: %v", errClose)
		}
	}()

	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read upload file failed: %w", err)
	}

	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	return "data:" + mediaType + ";base64," + b64, nil
}

func parseIntField(raw string, fallback int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

func parseBoolField(raw string, fallback bool) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func normalizeImagesResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		return "url"
	}
	return "b64_json"
}

func (h *APIHandler) materializeImageData(
	ctx context.Context,
	c *gin.Context,
	b64JSON string,
	mimeType string,
) (materializedImageArtifact, error) {
	store, builder := h.imageArtifactDependencies()
	if store == nil || builder == nil {
		return materializedImageArtifact{}, fmt.Errorf("image artifact store is unavailable")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64JSON))
	if err != nil {
		return materializedImageArtifact{}, fmt.Errorf("decode image output failed: %w", err)
	}
	record, err := store.PutBytes(ctx, data, mimeTypeFromOutputFormat(mimeType))
	if err != nil {
		return materializedImageArtifact{}, err
	}
	contentURL, err := builder.ImageArtifactContentURL(c, record)
	if err != nil {
		return materializedImageArtifact{}, err
	}
	return materializedImageArtifact{record: record, contentURL: contentURL, b64JSON: strings.TrimSpace(b64JSON)}, nil
}

func (h *APIHandler) materializeImageURL(
	ctx context.Context,
	c *gin.Context,
	rawURL string,
) (materializedImageArtifact, error) {
	store, builder := h.imageArtifactDependencies()
	if store == nil || builder == nil {
		return materializedImageArtifact{}, fmt.Errorf("image artifact store is unavailable")
	}
	record, data, err := store.PutRemoteImage(ctx, strings.TrimSpace(rawURL))
	if err != nil {
		return materializedImageArtifact{}, err
	}
	contentURL, err := builder.ImageArtifactContentURL(c, record)
	if err != nil {
		return materializedImageArtifact{}, err
	}
	return materializedImageArtifact{
		record:     record,
		contentURL: contentURL,
		b64JSON:    base64.StdEncoding.EncodeToString(data),
	}, nil
}

func (h *APIHandler) materializeImageDataURL(
	ctx context.Context,
	c *gin.Context,
	raw string,
) (materializedImageArtifact, error) {
	store, builder := h.imageArtifactDependencies()
	if store == nil || builder == nil {
		return materializedImageArtifact{}, fmt.Errorf("image artifact store is unavailable")
	}
	trimmed := strings.TrimSpace(raw)
	record, err := store.PutDataURL(ctx, trimmed)
	if err != nil {
		return materializedImageArtifact{}, err
	}
	contentURL, err := builder.ImageArtifactContentURL(c, record)
	if err != nil {
		return materializedImageArtifact{}, err
	}
	_, encoded, _ := strings.Cut(trimmed, ",")
	return materializedImageArtifact{
		record:     record,
		contentURL: contentURL,
		b64JSON:    strings.TrimSpace(encoded),
	}, nil
}

func (h *APIHandler) materializeImageURLOrDataURL(
	ctx context.Context,
	c *gin.Context,
	raw string,
) (materializedImageArtifact, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(trimmed), "data:image/") {
		return h.materializeImageDataURL(ctx, c, trimmed)
	}
	return h.materializeImageURL(ctx, c, trimmed)
}

func (h *APIHandler) materializeImageSource(
	ctx context.Context,
	c *gin.Context,
	b64JSON string,
	mimeType string,
	rawURL string,
) (materializedImageArtifact, error) {
	if strings.TrimSpace(b64JSON) != "" {
		return h.materializeImageData(ctx, c, b64JSON, mimeType)
	}
	if strings.TrimSpace(rawURL) != "" {
		return h.materializeImageURLOrDataURL(ctx, c, rawURL)
	}
	return materializedImageArtifact{}, fmt.Errorf("image output is empty")
}

func (h *APIHandler) imageArtifactDependencies() (handlers.ImageArtifactStore, handlers.ImageArtifactURLBuilder) {
	if h == nil || h.BaseAPIHandler == nil {
		return nil, nil
	}
	return h.BaseAPIHandler.ImageArtifactStore, h.BaseAPIHandler.ImageArtifactURLBuilder
}

func setImageArtifactFields(item []byte, artifact materializedImageArtifact) []byte {
	if artifact.record.ID != "" {
		item, _ = sjson.SetBytes(item, "artifact_id", artifact.record.ID)
	}
	if artifact.record.SHA256 != "" {
		item, _ = sjson.SetBytes(item, "sha256", artifact.record.SHA256)
	}
	return item
}

func isMaterializableImageURL(raw string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(trimmed, "data:image/") || strings.HasPrefix(
		trimmed, "http://",
	) || strings.HasPrefix(trimmed, "https://")
}

func imageMIMEFromMap(item map[string]any) string {
	for _, key := range []string{"mime_type", "output_format"} {
		if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func imageContextForKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "data", "image", "images", "image_url", "image_urls", "image_generation_call", "output":
		return true
	default:
		return false
	}
}

func mapHasImageFields(item map[string]any) bool {
	if item == nil {
		return false
	}
	if _, ok := item["b64_json"].(string); ok {
		return true
	}
	if _, ok := item["partial_image_b64"].(string); ok {
		return true
	}
	if _, ok := item["mime_type"].(string); ok {
		return true
	}
	if _, ok := item["output_format"].(string); ok {
		return true
	}
	if itemType, ok := item["type"].(string); ok {
		itemType = strings.ToLower(strings.TrimSpace(itemType))
		if strings.Contains(itemType, "image") {
			return true
		}
	}
	return false
}

func addImageArtifactFieldsToMap(item map[string]any, artifact materializedImageArtifact, contentURLField bool) {
	if artifact.record.ID != "" {
		item["artifact_id"] = artifact.record.ID
	}
	if artifact.record.SHA256 != "" {
		item["sha256"] = artifact.record.SHA256
	}
	if contentURLField && artifact.contentURL != "" {
		item["content_url"] = artifact.contentURL
	}
}

func (h *APIHandler) rewriteOpenAICompatImagesPayload(
	ctx context.Context,
	c *gin.Context,
	payload []byte,
	requireImage bool,
) ([]byte, error) {
	if !json.Valid(payload) {
		return nil, fmt.Errorf("upstream returned invalid image response JSON")
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, fmt.Errorf("decode upstream image response failed: %w", err)
	}
	changed, err := h.rewriteOpenAICompatImageValue(ctx, c, value, false)
	if err != nil {
		return nil, err
	}
	if requireImage && !changed {
		return nil, fmt.Errorf("upstream image response did not contain materializable image output")
	}
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten image response failed: %w", err)
	}
	return out, nil
}

func (h *APIHandler) rewriteOpenAICompatImageValue(
	ctx context.Context,
	c *gin.Context,
	value any,
	imageContext bool,
) (bool, error) {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		localImageContext := imageContext || mapHasImageFields(typed)
		var artifact materializedImageArtifact
		if localImageContext {
			if rawB64, ok := typed["b64_json"].(string); ok && strings.TrimSpace(rawB64) != "" {
				var err error
				artifact, err = h.materializeImageData(ctx, c, rawB64, imageMIMEFromMap(typed))
				if err != nil {
					return false, err
				}
				addImageArtifactFieldsToMap(typed, artifact, true)
				changed = true
			}
			if rawURL, ok := typed["url"].(string); ok && isMaterializableImageURL(rawURL) {
				if artifact.contentURL == "" {
					var err error
					artifact, err = h.materializeImageURLOrDataURL(ctx, c, rawURL)
					if err != nil {
						return false, err
					}
				}
				typed["url"] = artifact.contentURL
				addImageArtifactFieldsToMap(typed, artifact, false)
				changed = true
			}
		}
		for key, child := range typed {
			if key == "url" || key == "b64_json" || key == "content_url" || key == "artifact_id" || key == "sha256" {
				continue
			}
			childImageContext := imageContextForKey(key)
			childChanged, err := h.rewriteOpenAICompatImageValue(ctx, c, child, childImageContext)
			if err != nil {
				return false, err
			}
			changed = changed || childChanged
		}
		return changed, nil
	case []any:
		changed := false
		for _, child := range typed {
			childChanged, err := h.rewriteOpenAICompatImageValue(ctx, c, child, imageContext)
			if err != nil {
				return false, err
			}
			changed = changed || childChanged
		}
		return changed, nil
	default:
		return false, nil
	}
}

func (h *APIHandler) rewriteOpenAICompatSSEFrame(ctx context.Context, c *gin.Context, frame []byte) ([]byte, error) {
	payload, hasData, valid := responsesSSEDataPayload(frame)
	if !hasData || bytes.Equal(payload, []byte("[DONE]")) {
		return frame, nil
	}
	if !valid {
		return nil, fmt.Errorf("invalid image SSE data JSON")
	}
	rewritten, err := h.rewriteOpenAICompatImagesPayload(ctx, c, payload, false)
	if err != nil {
		return nil, err
	}

	lines := bytes.Split(frame, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	wroteData := false
	for _, line := range lines {
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			out = append(out, line)
			continue
		}
		if !wroteData {
			out = append(out, []byte("data: "+string(rewritten)))
			wroteData = true
		}
	}
	return bytes.Join(out, []byte("\n")), nil
}

func canonicalXAIImagesModel(model string) string {
	baseModel := imagesModelBase(model)
	if baseModel == xaiImagesQualityModel {
		return xaiImagesQualityModel
	}
	return defaultXAIImagesModel
}

func xaiImagesAspectRatio(raw string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1:1", "square":
		return "1:1"
	case "16:9", "landscape":
		return "16:9"
	case "9:16", "portrait":
		return "9:16"
	case "4:3":
		return "4:3"
	case "3:4":
		return "3:4"
	case "3:2":
		return "3:2"
	case "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiImagesAspectRatioFromSize(size string, fallback string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	switch size {
	case "1024x1024", "2048x2048", "1:1":
		return "1:1"
	case "1792x1024", "16:9":
		return "16:9"
	case "1024x1792", "9:16":
		return "9:16"
	case "1536x1024", "3:2":
		return "3:2"
	case "1024x1536", "2:3":
		return "2:3"
	default:
		return fallback
	}
}

func xaiImagesResolution(raw string, size string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1k", "2k":
		return strings.ToLower(strings.TrimSpace(raw))
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(size)), "2048") {
		return "2k"
	}
	return fallback
}

func xaiImagesRef(imageURL string) []byte {
	ref := []byte(`{"type":"image_url","url":""}`)
	ref, _ = sjson.SetBytes(ref, "url", strings.TrimSpace(imageURL))
	return ref
}

func buildXAIImagesBaseRequest(
	model string,
	prompt string,
	responseFormat string,
	aspectRatio string,
	resolution string,
	n int64,
) []byte {
	req := []byte(`{}`)
	req, _ = sjson.SetBytes(req, "model", canonicalXAIImagesModel(model))
	req, _ = sjson.SetBytes(req, "prompt", strings.TrimSpace(prompt))
	req, _ = sjson.SetBytes(req, "response_format", normalizeImagesResponseFormat(responseFormat))
	if aspectRatio != "" {
		req, _ = sjson.SetBytes(req, "aspect_ratio", aspectRatio)
	}
	if resolution != "" {
		req, _ = sjson.SetBytes(req, "resolution", resolution)
	}
	if n > 0 {
		req, _ = sjson.SetBytes(req, "n", n)
	}
	return req
}

func buildXAIImagesGenerationsRequest(rawJSON []byte, model string, responseFormat string) []byte {
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	size := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String())
	aspectRatio := xaiImagesAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), "")
	aspectRatio = xaiImagesAspectRatioFromSize(size, aspectRatio)
	if aspectRatio == "" {
		aspectRatio = xaiImagesDefaultAspectRatio
	}
	resolution := xaiImagesResolution(gjson.GetBytes(rawJSON, "resolution").String(), size, xaiImagesDefaultResolution)
	n := int64(0)
	if v := gjson.GetBytes(rawJSON, "n"); v.Exists() && v.Type == gjson.Number {
		n = v.Int()
	}
	return buildXAIImagesBaseRequest(model, prompt, responseFormat, aspectRatio, resolution, n)
}

func buildXAIImagesEditRequest(
	model string,
	prompt string,
	images []string,
	responseFormat string,
	aspectRatio string,
	resolution string,
	n int64,
) []byte {
	req := buildXAIImagesBaseRequest(model, prompt, responseFormat, aspectRatio, resolution, n)
	trimmedImages := make([]string, 0, len(images))
	for _, img := range images {
		if strings.TrimSpace(img) != "" {
			trimmedImages = append(trimmedImages, strings.TrimSpace(img))
		}
	}
	if len(trimmedImages) == 1 {
		req, _ = sjson.SetRawBytes(req, "image", xaiImagesRef(trimmedImages[0]))
		return req
	}
	for _, img := range trimmedImages {
		req, _ = sjson.SetRawBytes(req, "images.-1", xaiImagesRef(img))
	}
	return req
}

func collectXAIImagesFromJSON(rawJSON []byte) []string {
	var images []string
	appendImage := func(url string) {
		url = strings.TrimSpace(url)
		if url != "" {
			images = append(images, url)
		}
	}

	if image := gjson.GetBytes(rawJSON, "image"); image.Exists() {
		if image.Type == gjson.String {
			appendImage(image.String())
		} else if image.Type == gjson.JSON {
			appendImage(image.Get("image_url.url").String())
			if imageURL := image.Get("image_url"); imageURL.Type == gjson.String {
				appendImage(imageURL.String())
			}
			appendImage(image.Get("url").String())
		}
	}
	if imagesResult := gjson.GetBytes(rawJSON, "images"); imagesResult.IsArray() {
		for _, img := range imagesResult.Array() {
			if img.Type == gjson.String {
				appendImage(img.String())
				continue
			}
			appendImage(img.Get("image_url.url").String())
			if imageURL := img.Get("image_url"); imageURL.Type == gjson.String {
				appendImage(imageURL.String())
			}
			appendImage(img.Get("url").String())
		}
	}
	return images
}

func xaiImagesEditOptionsFromJSON(rawJSON []byte) (aspectRatio string, resolution string, n int64) {
	size := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String())
	aspectRatio = xaiImagesAspectRatio(gjson.GetBytes(rawJSON, "aspect_ratio").String(), "")
	aspectRatio = xaiImagesAspectRatioFromSize(size, aspectRatio)
	resolution = xaiImagesResolution(gjson.GetBytes(rawJSON, "resolution").String(), size, "")
	if v := gjson.GetBytes(rawJSON, "n"); v.Exists() && v.Type == gjson.Number {
		n = v.Int()
	}
	return aspectRatio, resolution, n
}

func extractXAIImagesResponse(payload []byte) (results []xaiImageResult, createdAt int64, usageRaw []byte, err error) {
	if !json.Valid(payload) {
		return nil, 0, nil, fmt.Errorf("upstream returned invalid image response JSON")
	}

	createdAt = gjson.GetBytes(payload, "created").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	data := gjson.GetBytes(payload, "data")
	if data.IsArray() {
		for _, item := range data.Array() {
			result := xaiImageResult{
				B64JSON:       strings.TrimSpace(item.Get("b64_json").String()),
				URL:           strings.TrimSpace(item.Get("url").String()),
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				MimeType:      strings.TrimSpace(item.Get("mime_type").String()),
			}
			if result.MimeType == "" {
				result.MimeType = mimeTypeFromOutputFormat(strings.TrimSpace(item.Get("output_format").String()))
			}
			if result.MimeType == "" {
				result.MimeType = "image/png"
			}
			if result.B64JSON == "" && result.URL == "" {
				continue
			}
			results = append(results, result)
		}
	}
	if len(results) == 0 {
		return nil, 0, nil, fmt.Errorf("upstream did not return image output")
	}

	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}

	return results, createdAt, usageRaw, nil
}

func (h *APIHandler) buildImagesAPIResponseFromXAI(
	ctx context.Context,
	c *gin.Context,
	payload []byte,
	responseFormat string,
) ([]byte, error) {
	results, createdAt, usageRaw, err := extractXAIImagesResponse(payload)
	if err != nil {
		return nil, err
	}

	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)
	responseFormat = normalizeImagesResponseFormat(responseFormat)

	for _, img := range results {
		artifact, errMaterialize := h.materializeImageSource(ctx, c, img.B64JSON, img.MimeType, img.URL)
		if errMaterialize != nil {
			return nil, errMaterialize
		}
		item := []byte(`{}`)
		if responseFormat == "url" {
			item, _ = sjson.SetBytes(item, "url", artifact.contentURL)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", artifact.b64JSON)
			item, _ = sjson.SetBytes(item, "content_url", artifact.contentURL)
		}
		item = setImageArtifactFields(item, artifact)
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}

	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	return out, nil
}

func (h *APIHandler) handleXAIImages(
	c *gin.Context,
	xaiReq []byte,
	routeModel string,
	responseFormat string,
	streamPrefix string,
	stream bool,
) {
	if stream {
		h.streamXAIImages(c, xaiReq, routeModel, responseFormat, streamPrefix)
		return
	}
	h.collectXAIImages(c, xaiReq, routeModel, responseFormat)
}

func (h *APIHandler) handleOpenAICompatImages(c *gin.Context, compatReq []byte, imageModel string, stream bool) {
	if stream {
		h.streamOpenAICompatImages(c, compatReq, imageModel)
		return
	}
	h.collectOpenAICompatImages(c, compatReq, imageModel)
}

func withOpenAIImageServiceTier(ctx context.Context, c *gin.Context) context.Context {
	if c == nil {
		return ctx
	}
	serviceTier := strings.TrimSpace(c.PostForm("service_tier"))
	if serviceTier == "" {
		return ctx
	}
	return cliproxyusage.WithServiceTier(ctx, serviceTier)
}

func (h *APIHandler) collectOpenAICompatImages(c *gin.Context, compatReq []byte, imageModel string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withOpenAIImageServiceTier(cliCtx, c)
	cliCtx, deferredUsage := withDeferredImageUsage(cliCtx)
	var finalErr error
	defer func() { deferredUsage.Complete(cliCtx, finalErr) }()
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	model := strings.TrimSpace(imageModel)
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, xaiImagesHandlerType, model, compatReq, "")
	stopKeepAlive()
	if errMsg != nil {
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}
	if len(bytes.TrimSpace(resp)) == 0 {
		errMsg := imageErrorMessage(fmt.Errorf("upstream returned empty image response"), http.StatusBadGateway)
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}

	out, err := h.rewriteOpenAICompatImagesPayload(cliCtx, c, resp, true)
	if err != nil {
		errMsg := imageErrorMessage(err, http.StatusBadGateway)
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel(nil)
}

func (h *APIHandler) streamOpenAICompatImages(c *gin.Context, compatReq []byte, imageModel string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Streaming not supported",
					Type:    "server_error",
				},
			},
		)
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = withOpenAIImageServiceTier(cliCtx, c)
	cliCtx, deferredUsage := withDeferredImageUsage(cliCtx)
	var finalErr error
	defer func() { deferredUsage.Complete(cliCtx, finalErr) }()
	model := strings.TrimSpace(imageModel)
	dataChan, upstreamHeaders, errChan := h.ExecuteImageStreamChunksWithAuthManager(
		cliCtx, xaiImagesHandlerType, model, compatReq, "",
	)

	setSSEHeaders := func() {
		handlers.SetSSEHeaders(c)
		c.Header("Access-Control-Allow-Origin", "*")
	}

	for dataChan != nil || errChan != nil {
		select {
		case <-cliCtx.Done():
			finalErr = cliCtx.Err()
			cliCancel(finalErr)
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if errMsg == nil {
				continue
			}
			finalErr = imageHandlerError(errMsg)
			if errWrite := h.WriteAcknowledgedStreamError(c, flusher, errMsg); errWrite != nil {
				finalErr = errWrite
			}
			cliCancel(finalErr)
			return
		case chunk, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			finalErr = h.forwardRawImageStreamChunks(cliCtx, c, dataChan, errChan, chunk)
			cliCancel(finalErr)
			return
		}
	}

	errMsg := imageErrorMessage(fmt.Errorf("upstream returned empty image stream"), http.StatusBadGateway)
	finalErr = imageHandlerError(errMsg)
	if errWrite := handlers.DeliverStreamResponse(
		c.Writer, flusher, func() error {
			return h.WriteErrorResponseWithError(c, errMsg)
		},
	); errWrite != nil {
		finalErr = errWrite
	}
	cliCancel(finalErr)
}

func (h *APIHandler) forwardRawImageStream(
	ctx context.Context,
	c *gin.Context,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	firstChunk []byte,
) error {
	chunks := imageStreamChunks(ctx, data)
	return h.forwardRawImageStreamChunksWithMode(
		ctx, c, chunks, errs, true, coreexecutor.StreamChunk{Payload: firstChunk},
	)
}

func imageStreamChunks(ctx context.Context, data <-chan []byte) <-chan coreexecutor.StreamChunk {
	chunks := make(chan coreexecutor.StreamChunk)
	if data == nil {
		close(chunks)
		return chunks
	}
	go func() {
		defer close(chunks)
		for chunk := range data {
			if ctx == nil {
				chunks <- coreexecutor.StreamChunk{Payload: chunk}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case chunks <- coreexecutor.StreamChunk{Payload: chunk}:
			}
		}
	}()
	return chunks
}

func (h *APIHandler) forwardRawImageStreamChunks(
	ctx context.Context,
	c *gin.Context,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	initial ...coreexecutor.StreamChunk,
) error {
	return h.forwardRawImageStreamChunksWithMode(ctx, c, data, errs, false, initial...)
}

func (h *APIHandler) forwardRawImageStreamChunksWithMode(
	ctx context.Context,
	c *gin.Context,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	deferTerminal bool,
	initial ...coreexecutor.StreamChunk,
) error {
	flusher, _ := c.Writer.(http.Flusher)
	acc := newBoundedSSEFrameAccumulator(maxImagesEditMultipartBodyBytes)
	var terminalFrames [][]byte
	terminalBytes := 0
	sawPayload := false
	sawTerminal := false
	var resultErr error

	emitError := func(errMsg *handlers.ErrorMessage) error {
		if errMsg == nil {
			return nil
		}
		status := http.StatusInternalServerError
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
		}
		errText := http.StatusText(status)
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
		body := handlers.BuildErrorResponseBody(status, errText)
		return handlers.WriteStreamBytes(
			c.Writer, []byte("event: error\ndata: "), body, []byte("\n\n"),
		)
	}
	writeProcessingError := func(err error, flush bool) error {
		errMsg := imageErrorMessage(err, http.StatusBadGateway)
		if errWrite := emitError(errMsg); errWrite != nil {
			return errWrite
		}
		if flush {
			if errFlush := handlers.FlushStreamResponse(c.Writer, flusher); errFlush != nil {
				return errFlush
			}
		}
		return imageHandlerError(errMsg)
	}
	processFrames := func(frames [][]byte) error {
		for _, frame := range frames {
			info := inspectImageSSEFrame(frame)
			if info.err != nil {
				return info.err
			}
			sawPayload = sawPayload || info.sawPayload
			out, errRewrite := h.rewriteOpenAICompatSSEFrame(ctx, c, frame)
			if errRewrite != nil {
				return errRewrite
			}
			if !info.terminal || !deferTerminal {
				if errWrite := handlers.WriteStreamBytes(c.Writer, out); errWrite != nil {
					return errWrite
				}
				if info.terminal {
					sawTerminal = true
				}
				continue
			}
			sawTerminal = true
			if terminalBytes > maxImagesEditMultipartBodyBytes-len(out) {
				return fmt.Errorf("image stream terminal events exceed %d bytes", maxImagesEditMultipartBodyBytes)
			}
			terminalFrames = append(terminalFrames, out)
			terminalBytes += len(out)
		}
		return nil
	}
	writeChunk := func(chunk []byte) error {
		if errProcess := processFrames(acc.AddChunk(chunk)); errProcess != nil {
			return writeProcessingError(errProcess, true)
		}
		if errAcc := acc.Err(); errAcc != nil {
			return writeProcessingError(errAcc, true)
		}
		return nil
	}
	writeDone := func() error {
		if errProcess := processFrames(acc.Flush()); errProcess != nil {
			return writeProcessingError(errProcess, false)
		}
		if errAcc := acc.Err(); errAcc != nil {
			return writeProcessingError(errAcc, false)
		}
		if !sawPayload {
			return writeProcessingError(fmt.Errorf("upstream returned empty image stream"), false)
		}
		if !sawTerminal {
			return writeProcessingError(fmt.Errorf("image stream disconnected before completion"), false)
		}
		for _, frame := range terminalFrames {
			if errWrite := handlers.WriteStreamBytes(c.Writer, frame); errWrite != nil {
				return errWrite
			}
		}
		return nil
	}

	h.BaseAPIHandler.ForwardStreamChunks(
		c,
		flusher,
		func(err error) { resultErr = err },
		data,
		errs,
		handlers.StreamChunkForwardOptions{
			WriteChunk: writeChunk,
			WriteTerminalError: func(errMsg *handlers.ErrorMessage) error {
				terminalFrames = nil
				return emitError(errMsg)
			},
			WriteDone: writeDone,
		},
		initial...,
	)
	return resultErr
}

func (h *APIHandler) collectXAIImages(c *gin.Context, xaiReq []byte, routeModel string, responseFormat string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx, deferredUsage := withDeferredImageUsage(cliCtx)
	var finalErr error
	defer func() { deferredUsage.Complete(cliCtx, finalErr) }()
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	model := strings.TrimSpace(routeModel)
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, xaiImagesHandlerType, model, xaiReq, "")
	stopKeepAlive()
	if errMsg != nil {
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}

	out, err := h.buildImagesAPIResponseFromXAI(cliCtx, c, resp, responseFormat)
	if err != nil {
		errMsg := imageErrorMessage(err, http.StatusBadGateway)
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel(nil)
}

func (h *APIHandler) streamXAIImages(
	c *gin.Context,
	xaiReq []byte,
	routeModel string,
	responseFormat string,
	streamPrefix string,
) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Streaming not supported",
					Type:    "server_error",
				},
			},
		)
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx, deferredUsage := withDeferredImageUsage(cliCtx)
	var finalErr error
	defer func() { deferredUsage.Complete(cliCtx, finalErr) }()
	model := strings.TrimSpace(routeModel)
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(cliCtx, xaiImagesHandlerType, model, xaiReq, "")
	if errMsg != nil {
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}

	results, _, usageRaw, err := extractXAIImagesResponse(resp)
	if err != nil {
		errMsg := imageErrorMessage(err, http.StatusBadGateway)
		finalErr = imageHandlerError(errMsg)
		h.WriteErrorResponse(c, errMsg)
		cliCancel(finalErr)
		return
	}

	handlers.SetSSEHeaders(c)
	c.Header("Access-Control-Allow-Origin", "*")
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

	eventName := streamPrefix + ".completed"
	responseFormat = normalizeImagesResponseFormat(responseFormat)
	for _, img := range results {
		artifact, errMaterialize := h.materializeImageSource(cliCtx, c, img.B64JSON, img.MimeType, img.URL)
		if errMaterialize != nil {
			errMsg := imageErrorMessage(errMaterialize, http.StatusBadGateway)
			finalErr = imageHandlerError(errMsg)
			body := handlers.BuildErrorResponseBody(errMsg.StatusCode, errMaterialize.Error())
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", string(body))
			flusher.Flush()
			cliCancel(finalErr)
			return
		}
		data := []byte(`{"type":""}`)
		data, _ = sjson.SetBytes(data, "type", eventName)
		if responseFormat == "url" {
			data, _ = sjson.SetBytes(data, "url", artifact.contentURL)
		} else {
			data, _ = sjson.SetBytes(data, "b64_json", artifact.b64JSON)
			data, _ = sjson.SetBytes(data, "content_url", artifact.contentURL)
		}
		data = setImageArtifactFields(data, artifact)
		if len(usageRaw) > 0 && json.Valid(usageRaw) {
			data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
		}
		if strings.TrimSpace(eventName) != "" {
			_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(data))
		flusher.Flush()
	}
	cliCancel(nil)
}

func (h *APIHandler) ImagesGenerations(c *gin.Context) {
	if h == nil || h.BaseAPIHandler == nil {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Images handler is unavailable",
					Type:    "server_error",
				},
			},
		)
		return
	}
	if h.ConfigSnapshot() != nil && h.ConfigSnapshot().DisableImageGeneration == internalconfig.DisableImageGenerationAll {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}
	if !json.Valid(rawJSON) {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: body must be valid JSON",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if rejectUnsupportedImagesModel(c, imageModel) {
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: prompt is required",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawJSON, "stream").Bool()

	if isCodexImagesToolModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		xaiReq := buildXAIImagesGenerationsRequest(rawJSON, imageModel, responseFormat)
		h.handleXAIImages(c, xaiReq, imageModel, responseFormat, "image_generation", stream)
		return
	}
	if isOpenAICompatImagesModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, stream)
		return
	}

	tool := []byte(`{"type":"image_generation","action":"generate"}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)

	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "size", v)
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "quality").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "quality", v)
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "background").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "background", v)
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "output_format").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "output_format", v)
	}
	if v := gjson.GetBytes(rawJSON, "output_compression"); v.Exists() {
		if v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, "output_compression", v.Int())
		}
	}
	if v := gjson.GetBytes(rawJSON, "partial_images"); v.Exists() {
		if v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, "partial_images", v.Int())
		}
	}
	if v := strings.TrimSpace(gjson.GetBytes(rawJSON, "moderation").String()); v != "" {
		tool, _ = sjson.SetBytes(tool, "moderation", v)
	}

	responsesReq := buildImagesResponsesRequest(prompt, nil, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, "image_generation")
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func (h *APIHandler) ImagesEdits(c *gin.Context) {
	if h == nil || h.BaseAPIHandler == nil {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Images handler is unavailable",
					Type:    "server_error",
				},
			},
		)
		return
	}
	if h.ConfigSnapshot() != nil && h.ConfigSnapshot().DisableImageGeneration == internalconfig.DisableImageGenerationAll {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImagesEditMultipartBodyBytes)

	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "application/json") {
		h.imagesEditsFromJSON(c)
		return
	}
	if strings.HasPrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesEditsFromMultipart(c)
		return
	}

	c.JSON(
		http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType),
				Type:    "invalid_request_error",
			},
		},
	)
}

func (h *APIHandler) imagesEditsFromMultipart(c *gin.Context) {
	if err := c.Request.ParseMultipartForm(maxImagesEditMultipartMemoryBytes); err != nil {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}
	form := c.Request.MultipartForm
	if form == nil {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: multipart form is required",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}
	defer func() { _ = form.RemoveAll() }()

	imageModel := strings.TrimSpace(c.PostForm("model"))
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if rejectUnsupportedImagesModel(c, imageModel) {
		return
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: prompt is required",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	var imageFiles []*multipart.FileHeader
	if files := form.File["image[]"]; len(files) > 0 {
		imageFiles = files
	} else if files := form.File["image"]; len(files) > 0 {
		imageFiles = files
	}
	if len(imageFiles) == 0 {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: image is required",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	responseFormat := strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := parseBoolField(c.PostForm("stream"), false)

	if isCodexImagesToolModel(imageModel) || isOpenAICompatImagesModel(imageModel) {
		compatReq, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, imageModel, stream)
		if errBuild != nil {
			c.JSON(
				http.StatusBadRequest, handlers.ErrorResponse{
					Error: handlers.ErrorDetail{
						Message: fmt.Sprintf("Invalid request: %v", errBuild),
						Type:    "invalid_request_error",
					},
				},
			)
			return
		}
		c.Request.Header.Set("Content-Type", contentType)
		h.handleOpenAICompatImages(c, compatReq, imageModel, stream)
		return
	}

	images := make([]string, 0, len(imageFiles))
	for _, fh := range imageFiles {
		dataURL, err := multipartFileToDataURL(fh)
		if err != nil {
			c.JSON(
				http.StatusBadRequest, handlers.ErrorResponse{
					Error: handlers.ErrorDetail{
						Message: fmt.Sprintf("Invalid request: %v", err),
						Type:    "invalid_request_error",
					},
				},
			)
			return
		}
		images = append(images, dataURL)
	}

	var maskDataURL *string
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, err := multipartFileToDataURL(maskFiles[0])
		if err != nil {
			c.JSON(
				http.StatusBadRequest, handlers.ErrorResponse{
					Error: handlers.ErrorDetail{
						Message: fmt.Sprintf("Invalid request: %v", err),
						Type:    "invalid_request_error",
					},
				},
			)
			return
		}
		maskDataURL = &dataURL
	}

	if isXAIImagesModel(imageModel) {
		aspectRatio := xaiImagesAspectRatio(c.PostForm("aspect_ratio"), "")
		aspectRatio = xaiImagesAspectRatioFromSize(c.PostForm("size"), aspectRatio)
		resolution := xaiImagesResolution(c.PostForm("resolution"), c.PostForm("size"), "")
		n := parseIntField(c.PostForm("n"), 0)
		xaiReq := buildXAIImagesEditRequest(imageModel, prompt, images, responseFormat, aspectRatio, resolution, n)
		h.handleXAIImages(c, xaiReq, imageModel, responseFormat, "image_edit", stream)
		return
	}

	tool := []byte(`{"type":"image_generation","action":"edit"}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)

	if v := strings.TrimSpace(c.PostForm("size")); v != "" {
		tool, _ = sjson.SetBytes(tool, "size", v)
	}
	if v := strings.TrimSpace(c.PostForm("quality")); v != "" {
		tool, _ = sjson.SetBytes(tool, "quality", v)
	}
	if v := strings.TrimSpace(c.PostForm("background")); v != "" {
		tool, _ = sjson.SetBytes(tool, "background", v)
	}
	if v := strings.TrimSpace(c.PostForm("output_format")); v != "" {
		tool, _ = sjson.SetBytes(tool, "output_format", v)
	}
	if v := strings.TrimSpace(c.PostForm("input_fidelity")); v != "" {
		tool, _ = sjson.SetBytes(tool, "input_fidelity", v)
	}
	if v := strings.TrimSpace(c.PostForm("moderation")); v != "" {
		tool, _ = sjson.SetBytes(tool, "moderation", v)
	}

	if v := strings.TrimSpace(c.PostForm("output_compression")); v != "" {
		tool, _ = sjson.SetBytes(tool, "output_compression", parseIntField(v, 0))
	}
	if v := strings.TrimSpace(c.PostForm("partial_images")); v != "" {
		tool, _ = sjson.SetBytes(tool, "partial_images", parseIntField(v, 0))
	}

	if maskDataURL != nil && strings.TrimSpace(*maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(*maskDataURL))
	}

	responsesReq := buildImagesResponsesRequest(prompt, images, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, "image_edit")
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func (h *APIHandler) imagesEditsFromJSON(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: fmt.Sprintf("Invalid request: %v", err),
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}
	if !json.Valid(rawJSON) {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: body must be valid JSON",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	if rejectUnsupportedImagesModel(c, imageModel) {
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: prompt is required",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawJSON, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawJSON, "stream").Bool()

	if isCodexImagesToolModel(imageModel) || isOpenAICompatImagesModel(imageModel) {
		compatReq := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
		h.handleOpenAICompatImages(c, compatReq, imageModel, stream)
		return
	}
	if isXAIImagesModel(imageModel) {
		xaiImages := collectXAIImagesFromJSON(rawJSON)
		if len(xaiImages) == 0 {
			c.JSON(
				http.StatusBadRequest, handlers.ErrorResponse{
					Error: handlers.ErrorDetail{
						Message: "Invalid request: image is required",
						Type:    "invalid_request_error",
					},
				},
			)
			return
		}
		aspectRatio, resolution, n := xaiImagesEditOptionsFromJSON(rawJSON)
		xaiReq := buildXAIImagesEditRequest(imageModel, prompt, xaiImages, responseFormat, aspectRatio, resolution, n)
		h.handleXAIImages(c, xaiReq, imageModel, responseFormat, "image_edit", stream)
		return
	}

	var images []string
	imagesResult := gjson.GetBytes(rawJSON, "images")
	if imagesResult.IsArray() {
		for _, img := range imagesResult.Array() {
			url := strings.TrimSpace(img.Get("image_url").String())
			if url == "" {
				continue
			}
			images = append(images, url)
		}
	}
	if len(images) == 0 {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: images[].image_url is required (file_id is not supported)",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	var maskDataURL *string
	if mask := gjson.GetBytes(rawJSON, "mask.image_url"); mask.Exists() {
		url := strings.TrimSpace(mask.String())
		if url != "" {
			maskDataURL = &url
		}
	} else if mask := gjson.GetBytes(rawJSON, "mask.file_id"); mask.Exists() {
		c.JSON(
			http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: mask.file_id is not supported (use mask.image_url instead)",
					Type:    "invalid_request_error",
				},
			},
		)
		return
	}

	tool := []byte(`{"type":"image_generation","action":"edit"}`)
	tool, _ = sjson.SetBytes(tool, "model", imageModel)

	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"} {
		if v := strings.TrimSpace(gjson.GetBytes(rawJSON, field).String()); v != "" {
			tool, _ = sjson.SetBytes(tool, field, v)
		}
	}

	for _, field := range []string{"output_compression", "partial_images"} {
		if v := gjson.GetBytes(rawJSON, field); v.Exists() && v.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, v.Int())
		}
	}

	if maskDataURL != nil && strings.TrimSpace(*maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(*maskDataURL))
	}

	responsesReq := buildImagesResponsesRequest(prompt, images, tool)
	if stream {
		h.streamImagesFromResponses(c, responsesReq, responseFormat, "image_edit")
		return
	}
	h.collectImagesFromResponses(c, responsesReq, responseFormat)
}

func buildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	mainModel := defaultImagesMainModel
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		toolModel := strings.TrimSpace(gjson.GetBytes(toolJSON, "model").String())
		if idx := strings.LastIndex(toolModel, "/"); idx > 0 && idx < len(toolModel)-1 {
			prefix := strings.TrimSpace(toolModel[:idx])
			if prefix != "" {
				mainModel = prefix + "/" + defaultImagesMainModel
			}
		}
	}
	req, _ = sjson.SetBytes(req, "model", mainModel)

	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", prompt)
	contentIndex := 1
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", img)
		path := fmt.Sprintf("0.content.%d", contentIndex)
		input, _ = sjson.SetRawBytes(input, path, part)
		contentIndex++
	}
	req, _ = sjson.SetRawBytes(req, "input", input)

	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools.-1", toolJSON)
	}
	return req
}

func (h *APIHandler) collectImagesFromResponses(c *gin.Context, responsesReq []byte, responseFormat string) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	cliCtx, deferredUsage := withDeferredImageUsage(cliCtx)
	var finalErr error
	defer func() { deferredUsage.Complete(cliCtx, finalErr) }()
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	mainModel := strings.TrimSpace(gjson.GetBytes(responsesReq, "model").String())
	if mainModel == "" {
		mainModel = defaultImagesMainModel
	}
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamChunksWithAuthManager(
		cliCtx, "openai-response", mainModel, responsesReq, "",
	)

	out, terminalAcknowledge, errMsg := h.collectImagesFromResponsesChunks(
		cliCtx, c, dataChan, errChan, responseFormat,
	)
	stopKeepAlive()
	flusher, _ := c.Writer.(http.Flusher)
	if errMsg != nil {
		finalErr = imageHandlerError(errMsg)
		if errWrite := h.WriteAcknowledgedStreamError(c, flusher, errMsg); errWrite != nil {
			finalErr = errors.Join(finalErr, errWrite)
		}
		cliCancel(finalErr)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	errDelivery := deliverCollectedImagesResponse(c.Writer, flusher, out, terminalAcknowledge)
	if errDelivery != nil {
		finalErr = errDelivery
		cliCancel(finalErr)
		return
	}
	cliCancel(nil)
}

type imagesFromResponsesCollector struct {
	handler        *APIHandler
	ctx            context.Context
	ginContext     *gin.Context
	responseFormat string
	accumulator    *sseFrameAccumulator
	fallback       []imageCallResult
	fallbackMeta   imageCallResult
	completedOut   []byte
}

func newImagesFromResponsesCollector(
	handler *APIHandler,
	ctx context.Context,
	c *gin.Context,
	responseFormat string,
) *imagesFromResponsesCollector {
	return &imagesFromResponsesCollector{
		handler:        handler,
		ctx:            ctx,
		ginContext:     c,
		responseFormat: responseFormat,
		accumulator:    newBoundedSSEFrameAccumulator(maxImagesEditMultipartBodyBytes),
	}
}

func (collector *imagesFromResponsesCollector) addChunk(chunk []byte) *handlers.ErrorMessage {
	if errMsg := collector.processFrames(collector.accumulator.AddChunk(chunk)); errMsg != nil {
		return errMsg
	}
	if err := collector.accumulator.Err(); err != nil {
		return imageErrorMessage(err, http.StatusBadGateway)
	}
	return nil
}

func (collector *imagesFromResponsesCollector) finalize() ([]byte, *handlers.ErrorMessage) {
	if errMsg := collector.processFrames(collector.accumulator.Flush()); errMsg != nil {
		return nil, errMsg
	}
	if err := collector.accumulator.Err(); err != nil {
		return nil, imageErrorMessage(err, http.StatusBadGateway)
	}
	if collector.completedOut == nil {
		return nil, imageErrorMessage(fmt.Errorf("stream disconnected before completion"), http.StatusBadGateway)
	}
	return collector.completedOut, nil
}

func (collector *imagesFromResponsesCollector) processFrames(frames [][]byte) *handlers.ErrorMessage {
	for _, frame := range frames {
		out, done, errMsg := collector.processFrame(frame)
		if errMsg != nil {
			return errMsg
		}
		if !done {
			continue
		}
		if collector.completedOut != nil {
			return imageErrorMessage(fmt.Errorf("upstream returned multiple completion events"), http.StatusBadGateway)
		}
		collector.completedOut = out
	}
	return nil
}

func (collector *imagesFromResponsesCollector) processFrame(frame []byte) (
	[]byte,
	bool,
	*handlers.ErrorMessage,
) {
	if info := inspectImageSSEFrame(frame); info.err != nil {
		return nil, false, imageErrorMessage(info.err, http.StatusBadGateway)
	}
	payload, hasData, _ := responsesSSEDataPayload(frame)
	if !hasData || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false, nil
	}

	eventType := gjson.GetBytes(payload, "type").String()
	if eventType == "response.image_generation_call.partial_image" {
		res := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
		if res != "" {
			entry := imageCallResult{
				Result:       res,
				OutputFormat: strings.TrimSpace(gjson.GetBytes(payload, "output_format").String()),
				Size:         strings.TrimSpace(gjson.GetBytes(payload, "size").String()),
				Background:   strings.TrimSpace(gjson.GetBytes(payload, "background").String()),
				Quality:      strings.TrimSpace(gjson.GetBytes(payload, "quality").String()),
			}
			collector.fallback = []imageCallResult{entry}
			collector.fallbackMeta = entry
		}
		return nil, false, nil
	}
	if eventType != "response.completed" {
		return nil, false, nil
	}

	results, createdAt, usageRaw, firstMeta, err := extractImagesFromResponsesCompleted(payload)
	if err != nil {
		return nil, false, imageErrorMessage(err, http.StatusBadGateway)
	}
	if len(results) == 0 && len(collector.fallback) > 0 {
		results = collector.fallback
		firstMeta = collector.fallbackMeta
	}
	if len(results) == 0 {
		return nil, false, imageErrorMessage(
			fmt.Errorf("upstream did not return image output"), http.StatusBadGateway,
		)
	}
	out, err := collector.handler.buildImagesAPIResponse(
		collector.ctx,
		collector.ginContext,
		results,
		createdAt,
		usageRaw,
		firstMeta,
		collector.responseFormat,
	)
	if err != nil {
		return nil, false, imageErrorMessage(err, http.StatusBadGateway)
	}
	return out, true, nil
}

func bindImagesFromResponsesProcessingError(
	errMsg *handlers.ErrorMessage,
	acknowledge func(error),
) *handlers.ErrorMessage {
	if errMsg == nil || acknowledge == nil {
		return errMsg
	}
	processingErr := imageHandlerError(errMsg)
	errMsg.Acknowledge = func(deliveryErr error) {
		acknowledge(errors.Join(processingErr, deliveryErr))
	}
	return errMsg
}

func deliverCollectedImagesResponse(
	writer http.ResponseWriter,
	flusher http.Flusher,
	out []byte,
	acknowledge func(error),
) error {
	errDelivery := handlers.DeliverStreamResponse(
		writer, flusher, func() error {
			return handlers.WriteStreamBytes(writer, out)
		},
	)
	if acknowledge != nil {
		acknowledge(errDelivery)
	}
	return errDelivery
}

func (h *APIHandler) collectImagesFromResponsesChunks(
	ctx context.Context,
	c *gin.Context,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	responseFormat string,
) ([]byte, func(error), *handlers.ErrorMessage) {
	collector := newImagesFromResponsesCollector(h, ctx, c, responseFormat)
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return nil, nil, imageErrorMessage(ctx.Err(), http.StatusRequestTimeout)
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				return nil, nil, errMsg
			}
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			if chunk.Err != nil {
				errMsg := imageErrorMessage(chunk.Err, http.StatusBadGateway)
				errMsg.Acknowledge = chunk.Acknowledge
				return nil, nil, errMsg
			}
			if chunk.Terminal {
				out, errMsg := collector.finalize()
				if errMsg != nil {
					return nil, nil, bindImagesFromResponsesProcessingError(errMsg, chunk.Acknowledge)
				}
				return out, chunk.Acknowledge, nil
			}
			if len(chunk.Payload) == 0 {
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
				continue
			}
			if errMsg := collector.addChunk(chunk.Payload); errMsg != nil {
				return nil, nil, bindImagesFromResponsesProcessingError(errMsg, chunk.Acknowledge)
			}
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		}
	}

	if _, errMsg := collector.finalize(); errMsg != nil {
		return nil, nil, errMsg
	}
	return nil, nil, imageErrorMessage(
		fmt.Errorf("stream disconnected before transport completion"), http.StatusBadGateway,
	)
}

func (h *APIHandler) collectImagesFromResponsesStream(
	ctx context.Context,
	c *gin.Context,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	responseFormat string,
) ([]byte, *handlers.ErrorMessage) {
	collector := newImagesFromResponsesCollector(h, ctx, c, responseFormat)
	for data != nil || errs != nil {
		select {
		case <-ctx.Done():
			return nil, imageErrorMessage(ctx.Err(), http.StatusRequestTimeout)
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				return nil, errMsg
			}
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			if errMsg := collector.addChunk(chunk); errMsg != nil {
				return nil, errMsg
			}
		}
	}
	return collector.finalize()
}

func extractImagesFromResponsesCompleted(payload []byte) (
	results []imageCallResult,
	createdAt int64,
	usageRaw []byte,
	firstMeta imageCallResult,
	err error,
) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, nil, imageCallResult{}, fmt.Errorf("unexpected event type")
	}

	createdAt = gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			res := strings.TrimSpace(item.Get("result").String())
			if res == "" {
				continue
			}
			entry := imageCallResult{
				Result:        res,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
			}
			if len(results) == 0 {
				firstMeta = entry
			}
			results = append(results, entry)
		}
	}

	if usage := gjson.GetBytes(payload, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}

	return results, createdAt, usageRaw, firstMeta, nil
}

func (h *APIHandler) buildImagesAPIResponse(
	ctx context.Context,
	c *gin.Context,
	results []imageCallResult,
	createdAt int64,
	usageRaw []byte,
	firstMeta imageCallResult,
	responseFormat string,
) ([]byte, error) {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)

	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}

	for _, img := range results {
		artifact, errMaterialize := h.materializeImageData(ctx, c, img.Result, img.OutputFormat)
		if errMaterialize != nil {
			return nil, errMaterialize
		}
		item := []byte(`{}`)
		if responseFormat == "url" {
			item, _ = sjson.SetBytes(item, "url", artifact.contentURL)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", img.Result)
			item, _ = sjson.SetBytes(item, "content_url", artifact.contentURL)
		}
		item = setImageArtifactFields(item, artifact)
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}

	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}

	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}

	return out, nil
}

func (h *APIHandler) streamImagesFromResponses(
	c *gin.Context,
	responsesReq []byte,
	responseFormat string,
	streamPrefix string,
) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(
			http.StatusInternalServerError, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Streaming not supported",
					Type:    "server_error",
				},
			},
		)
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	cliCtx, deferredUsage := withDeferredImageUsage(cliCtx)
	var finalErr error
	defer func() { deferredUsage.Complete(cliCtx, finalErr) }()
	mainModel := strings.TrimSpace(gjson.GetBytes(responsesReq, "model").String())
	if mainModel == "" {
		mainModel = defaultImagesMainModel
	}
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamChunksWithAuthManager(
		cliCtx, "openai-response", mainModel, responsesReq, "",
	)

	setSSEHeaders := func() {
		handlers.SetSSEHeaders(c)
		c.Header("Access-Control-Allow-Origin", "*")
	}

	writeEvent := func(eventName string, dataJSON []byte) error {
		if strings.TrimSpace(eventName) != "" {
			if errWrite := handlers.WriteStreamBytes(
				c.Writer, []byte("event: "), []byte(eventName), []byte("\n"),
			); errWrite != nil {
				return errWrite
			}
		}
		return handlers.WriteStreamBytes(c.Writer, []byte("data: "), dataJSON, []byte("\n\n"))
	}

	for dataChan != nil || errChan != nil {
		select {
		case <-cliCtx.Done():
			finalErr = cliCtx.Err()
			cliCancel(finalErr)
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if errMsg == nil {
				continue
			}
			finalErr = imageHandlerError(errMsg)
			if errWrite := h.WriteAcknowledgedStreamError(c, flusher, errMsg); errWrite != nil {
				finalErr = errWrite
			}
			cliCancel(finalErr)
			return
		case chunk, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			finalErr = h.forwardImagesStreamChunks(
				cliCtx, c, dataChan, errChan, responseFormat, streamPrefix, writeEvent, chunk,
			)
			cliCancel(finalErr)
			return
		}
	}

	errMsg := imageErrorMessage(fmt.Errorf("upstream returned empty image stream"), http.StatusBadGateway)
	finalErr = imageHandlerError(errMsg)
	if errWrite := handlers.DeliverStreamResponse(
		c.Writer, flusher, func() error {
			return h.WriteErrorResponseWithError(c, errMsg)
		},
	); errWrite != nil {
		finalErr = errWrite
	}
	cliCancel(finalErr)
}

type imageStreamEvent struct {
	name string
	data []byte
}

func (h *APIHandler) forwardImagesStream(
	ctx context.Context,
	c *gin.Context,
	data <-chan []byte,
	errs <-chan *handlers.ErrorMessage,
	firstChunk []byte,
	responseFormat string,
	streamPrefix string,
	writeEvent func(string, []byte),
) error {
	chunks := imageStreamChunks(ctx, data)
	return h.forwardImagesStreamChunksWithMode(
		ctx,
		c,
		chunks,
		errs,
		responseFormat,
		streamPrefix,
		true,
		func(name string, payload []byte) error {
			writeEvent(name, payload)
			return nil
		},
		coreexecutor.StreamChunk{Payload: firstChunk},
	)
}

func (h *APIHandler) forwardImagesStreamChunks(
	ctx context.Context,
	c *gin.Context,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	responseFormat string,
	streamPrefix string,
	writeEvent func(string, []byte) error,
	initial ...coreexecutor.StreamChunk,
) error {
	return h.forwardImagesStreamChunksWithMode(
		ctx, c, data, errs, responseFormat, streamPrefix, false, writeEvent, initial...,
	)
}

func (h *APIHandler) forwardImagesStreamChunksWithMode(
	ctx context.Context,
	c *gin.Context,
	data <-chan coreexecutor.StreamChunk,
	errs <-chan *handlers.ErrorMessage,
	responseFormat string,
	streamPrefix string,
	deferCompleted bool,
	writeEvent func(string, []byte) error,
	initial ...coreexecutor.StreamChunk,
) error {
	flusher, _ := c.Writer.(http.Flusher)
	acc := newBoundedSSEFrameAccumulator(maxImagesEditMultipartBodyBytes)

	responseFormat = strings.ToLower(strings.TrimSpace(responseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	emittedPartial := false
	sawPayload := false
	completed := false
	pendingBytes := 0
	var pendingEvents []imageStreamEvent
	var resultErr error

	emitError := func(errMsg *handlers.ErrorMessage) error {
		if errMsg == nil {
			return nil
		}
		status := http.StatusInternalServerError
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
		}
		errText := http.StatusText(status)
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
		body := handlers.BuildErrorResponseBody(status, errText)
		return writeEvent("error", body)
	}
	writeProcessingError := func(err error, flush bool) error {
		errMsg := imageErrorMessage(err, http.StatusBadGateway)
		if errWrite := emitError(errMsg); errWrite != nil {
			return errWrite
		}
		if flush {
			if errFlush := handlers.FlushStreamResponse(c.Writer, flusher); errFlush != nil {
				return errFlush
			}
		}
		return imageHandlerError(errMsg)
	}
	appendPendingEvent := func(name string, eventData []byte) error {
		additional := len(name) + len(eventData)
		if pendingBytes > maxImagesEditMultipartBodyBytes-additional {
			return fmt.Errorf("image stream completion events exceed %d bytes", maxImagesEditMultipartBodyBytes)
		}
		pendingEvents = append(pendingEvents, imageStreamEvent{name: name, data: eventData})
		pendingBytes += additional
		return nil
	}

	processFrame := func(frame []byte) error {
		info := inspectImageSSEFrame(frame)
		if info.err != nil {
			return info.err
		}
		sawPayload = sawPayload || info.sawPayload
		payload, hasData, _ := responsesSSEDataPayload(frame)
		if !hasData || bytes.Equal(payload, []byte("[DONE]")) {
			return nil
		}

		switch gjson.GetBytes(payload, "type").String() {
		case "response.image_generation_call.partial_image":
			b64 := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
			if b64 == "" {
				return nil
			}
			index := gjson.GetBytes(payload, "partial_image_index").Int()
			eventName := streamPrefix + ".partial_image"
			evtData := []byte(`{"type":"","partial_image_index":0}`)
			evtData, _ = sjson.SetBytes(evtData, "type", eventName)
			evtData, _ = sjson.SetBytes(evtData, "partial_image_index", index)
			evtData, _ = sjson.SetBytes(evtData, "b64_json", b64)
			if errWrite := writeEvent(eventName, evtData); errWrite != nil {
				return errWrite
			}
			emittedPartial = true
		case "response.completed":
			if completed {
				return fmt.Errorf("upstream returned multiple completion events")
			}
			completed = true
			results, _, usageRaw, _, err := extractImagesFromResponsesCompleted(payload)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				if emittedPartial {
					return nil
				}
				return fmt.Errorf("upstream did not return image output")
			}
			eventName := streamPrefix + ".completed"
			for _, img := range results {
				artifact, errMaterialize := h.materializeImageData(ctx, c, img.Result, img.OutputFormat)
				if errMaterialize != nil {
					return errMaterialize
				}
				evtData := []byte(`{"type":""}`)
				evtData, _ = sjson.SetBytes(evtData, "type", eventName)
				if responseFormat == "url" {
					evtData, _ = sjson.SetBytes(evtData, "url", artifact.contentURL)
				} else {
					evtData, _ = sjson.SetBytes(evtData, "b64_json", img.Result)
					evtData, _ = sjson.SetBytes(evtData, "content_url", artifact.contentURL)
				}
				evtData = setImageArtifactFields(evtData, artifact)
				if len(usageRaw) > 0 && json.Valid(usageRaw) {
					evtData, _ = sjson.SetRawBytes(evtData, "usage", usageRaw)
				}
				if deferCompleted {
					if errAppend := appendPendingEvent(eventName, evtData); errAppend != nil {
						return errAppend
					}
					continue
				}
				if errWrite := writeEvent(eventName, evtData); errWrite != nil {
					return errWrite
				}
			}
		}
		return nil
	}
	processFrames := func(frames [][]byte) error {
		for _, frame := range frames {
			if errProcess := processFrame(frame); errProcess != nil {
				return errProcess
			}
		}
		return nil
	}
	writeChunk := func(chunk []byte) error {
		if errProcess := processFrames(acc.AddChunk(chunk)); errProcess != nil {
			return writeProcessingError(errProcess, true)
		}
		if errAcc := acc.Err(); errAcc != nil {
			return writeProcessingError(errAcc, true)
		}
		return nil
	}
	writeDone := func() error {
		if errProcess := processFrames(acc.Flush()); errProcess != nil {
			return writeProcessingError(errProcess, false)
		}
		if errAcc := acc.Err(); errAcc != nil {
			return writeProcessingError(errAcc, false)
		}
		if !sawPayload {
			return writeProcessingError(fmt.Errorf("upstream returned empty image stream"), false)
		}
		if !completed {
			return writeProcessingError(fmt.Errorf("image stream disconnected before completion"), false)
		}
		for _, event := range pendingEvents {
			if errWrite := writeEvent(event.name, event.data); errWrite != nil {
				return errWrite
			}
		}
		return nil
	}

	h.BaseAPIHandler.ForwardStreamChunks(
		c,
		flusher,
		func(err error) { resultErr = err },
		data,
		errs,
		handlers.StreamChunkForwardOptions{
			WriteChunk: writeChunk,
			WriteTerminalError: func(errMsg *handlers.ErrorMessage) error {
				pendingEvents = nil
				return emitError(errMsg)
			},
			WriteDone: writeDone,
		},
		initial...,
	)
	return resultErr
}

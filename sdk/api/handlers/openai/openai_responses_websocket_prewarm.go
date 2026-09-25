package openai

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func shouldHandleResponsesWebsocketPrewarmLocally(
	rawJSON []byte,
	lastRequest []byte,
	allowIncrementalInputWithPreviousResponseID bool,
) bool {
	if allowIncrementalInputWithPreviousResponseID || len(lastRequest) != 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	generate := gjson.GetBytes(rawJSON, "generate")
	return generate.Exists() && !generate.Bool()
}

func writeResponsesWebsocketSyntheticPrewarm(
	c *gin.Context,
	writer *responsesWebsocketWriter,
	requestJSON []byte,
	wsTimelineLog *websocketTimelineRecorder,
	sessionID string,
) (string, error) {
	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads(requestJSON)
	if errPayloads != nil {
		return "", errPayloads
	}
	for _, payload := range payloads {
		markAPIResponseTimestamp(c)
		if errWrite := writer.writeText(payload); errWrite != nil {
			log.Warnf(
				"responses websocket: downstream_out write failed id=%s event=%s error=%v",
				sessionID,
				websocketPayloadEventType(payload),
				errWrite,
			)
			return "", errWrite
		}
		appendWebsocketEvent(wsTimelineLog, "response", payload)
		log.Debugf(
			"responses websocket: downstream_out id=%s type=%d event=%s",
			sessionID,
			websocket.TextMessage,
			websocketPayloadEventType(payload),
		)
	}
	return gjson.GetBytes(payloads[0], "response.id").String(), nil
}

// A synthetic warm-up acknowledges input that never reached the upstream.
// Materialize that input before compacted-history detection can treat the
// remaining client delta as a complete replacement transcript.
func normalizeResponsesWebsocketPrewarmFollowup(
	rawJSON []byte,
	warmupRequest []byte,
) ([]byte, []byte, *handlers.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return nil, warmupRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
	input := gjson.GetBytes(rawJSON, "input")
	if !input.IsArray() {
		return nil, warmupRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}
	merged, errMerge := mergeResponsesWebsocketInput(warmupRequest, []byte("[]"), input.Raw)
	if errMerge != nil {
		return nil, warmupRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      errMerge,
		}
	}
	normalized := normalizeResponseTranscriptReplacement(rawJSON, warmupRequest)
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", merged)
	if errSet != nil {
		return nil, warmupRequest, &handlers.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      errSet,
		}
	}
	return normalized, normalized, nil
}

func syntheticResponsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, error) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())

	createdPayload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	var errSet error
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		createdPayload, errSet = sjson.SetBytes(createdPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	donePayload := []byte(`{"type":"response.done","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`)
	donePayload, errSet = sjson.SetBytes(donePayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	donePayload, errSet = sjson.SetBytes(donePayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		donePayload, errSet = sjson.SetBytes(donePayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	return [][]byte{createdPayload, donePayload}, nil
}

package wsrelay

// message represents the JSON payload exchanged with websocket clients.
type message struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

const (
	// messageTypeHTTPReq identifies an HTTP-style request envelope.
	messageTypeHTTPReq = "http_request"
	// MessageTypeHTTPResp identifies a non-streaming HTTP response envelope.
	MessageTypeHTTPResp = "http_response"
	// MessageTypeStreamStart marks the beginning of a streaming response.
	MessageTypeStreamStart = "stream_start"
	// MessageTypeStreamChunk carries a streaming response chunk.
	MessageTypeStreamChunk = "stream_chunk"
	// MessageTypeStreamEnd marks the completion of a streaming response.
	MessageTypeStreamEnd = "stream_end"
	// MessageTypeError carries an error response.
	MessageTypeError = "error"
	// messageTypePing represents ping messages from clients.
	messageTypePing = "ping"
	// messageTypePong represents pong responses back to clients.
	messageTypePong = "pong"
)

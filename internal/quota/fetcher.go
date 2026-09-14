package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// APICallRequest matches the management API's POST /api-call body format.
type APICallRequest struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Header    map[string]string `json:"header,omitempty"`
	AuthIndex string            `json:"auth_index,omitempty"`
	Data      string            `json:"data,omitempty"`
}

// APICallResponse exposes the upstream response body as its original bytes.
type APICallResponse struct {
	StatusCode int
	Header     map[string][]string
	Body       json.RawMessage
	BodyText   string
}

type localAPICallWireResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header,omitempty"`
	Body       json.RawMessage     `json:"body,omitempty"`
	BodyText   string              `json:"bodyText,omitempty"`
}

func restoreAPICallResponseBody(body json.RawMessage) (json.RawMessage, error) {
	if len(body) == 0 || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return nil, nil
	}
	var encoded string
	if err := json.Unmarshal(body, &encoded); err == nil {
		return json.RawMessage(encoded), nil
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("api-call response body is invalid JSON")
	}
	return append(json.RawMessage(nil), body...), nil
}

// InternalAPICallFunc wraps a call to the management /api-call endpoint on localhost.
type InternalAPICallFunc func(req APICallRequest) (*APICallResponse, error)

// InternalAPICallContextFunc supports cancellation while calling the local management API.
type InternalAPICallContextFunc func(ctx context.Context, req APICallRequest) (*APICallResponse, error)

// NewLocalAPICallContextFunc calls the management /api-call endpoint with request cancellation.
func NewLocalAPICallContextFunc(port int, managementKey string) InternalAPICallContextFunc {
	client := &http.Client{Timeout: 30 * time.Second}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/v0/management/api-call", port)

	return func(ctx context.Context, req APICallRequest) (*APICallResponse, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		body, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal api-call request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if managementKey != "" {
			httpReq.Header.Set("X-Management-Key", managementKey)
		}

		httpResp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("api-call request failed: %w", err)
		}
		defer func() { _ = httpResp.Body.Close() }()

		respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		var wire localAPICallWireResponse
		if err := json.Unmarshal(respBody, &wire); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		upstreamBody, err := restoreAPICallResponseBody(wire.Body)
		if err != nil {
			return nil, err
		}

		return &APICallResponse{
			StatusCode: wire.StatusCode,
			Header:     wire.Header,
			Body:       upstreamBody,
			BodyText:   wire.BodyText,
		}, nil
	}
}

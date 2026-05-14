package quota

import (
	"bytes"
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

// APICallResponse matches the management API's response format.
type APICallResponse struct {
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body,omitempty"`
	BodyText   string          `json:"bodyText,omitempty"`
}

// InternalAPICallFunc wraps a call to the management /api-call endpoint on localhost.
type InternalAPICallFunc func(req APICallRequest) (*APICallResponse, error)

// NewLocalAPICallFunc creates a function that calls the management API's /api-call
// endpoint on the local server. This is used by the quota scheduler to fetch
// quota data from provider APIs via the same proxy mechanism as the frontend.
func NewLocalAPICallFunc(port int, managementKey string) InternalAPICallFunc {
	client := &http.Client{Timeout: 30 * time.Second}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/v0/management/api-call", port)

	return func(req APICallRequest) (*APICallResponse, error) {
		body, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal api-call request: %w", err)
		}

		httpReq, err := http.NewRequest("POST", baseURL, bytes.NewReader(body))
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
		defer httpResp.Body.Close()

		respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20))
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		var resp APICallResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		return &resp, nil
	}
}

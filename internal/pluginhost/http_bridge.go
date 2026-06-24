package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	helps "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type hostHTTPClient struct {
	host     *Host
	auth     *coreauth.Auth
	provider string
}

func (h *Host) newHTTPClient(auth *coreauth.Auth, providers ...string) pluginapi.HostHTTPClient {
	provider := ""
	if len(providers) > 0 {
		provider = providers[0]
	}
	return &hostHTTPClient{host: h, auth: auth, provider: provider}
}

func (c *hostHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, cfg, errDo := c.doHTTP(ctx, req)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, cfg, errDo)
		return pluginapi.HTTPResponse{}, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warnf("pluginhost: response body close error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, cfg, resp.StatusCode, resp.Header)
	body, errReadAll := io.ReadAll(resp.Body)
	if errReadAll != nil {
		helps.RecordAPIResponseError(ctx, cfg, errReadAll)
		return pluginapi.HTTPResponse{}, fmt.Errorf("read host http response: %w", errReadAll)
	}
	helps.AppendAPIResponseChunk(ctx, cfg, body)
	return pluginapi.HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Header),
		Body:       body,
	}, nil
}

func (c *hostHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (
	pluginapi.HTTPStreamResponse,
	error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, cfg, errDo := c.doHTTP(ctx, req)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, cfg, errDo)
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, cfg, resp.StatusCode, resp.Header)
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Warnf("pluginhost: stream response body close error: %v", errClose)
			}
		}()
		buf := make([]byte, 32*1024)
		for {
			n, errRead := resp.Body.Read(buf)
			if n > 0 {
				payload := bytes.Clone(buf[:n])
				helps.AppendAPIResponseChunk(ctx, cfg, payload)
				select {
				case <-ctx.Done():
					return
				case chunks <- pluginapi.HTTPStreamChunk{Payload: payload}:
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					helps.RecordAPIResponseError(ctx, cfg, errRead)
					select {
					case <-ctx.Done():
					case chunks <- pluginapi.HTTPStreamChunk{Err: errRead}:
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Header),
		Chunks:     chunks,
	}, nil
}

func (c *hostHTTPClient) doHTTP(ctx context.Context, req pluginapi.HTTPRequest) (
	*http.Response,
	*config.Config,
	error,
) {
	if c == nil || c.host == nil {
		return nil, nil, fmt.Errorf("host http client is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := c.host.currentRuntimeConfig()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, errNewRequest := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(bytes.Clone(req.Body)))
	if errNewRequest != nil {
		return nil, cfg, fmt.Errorf("create host http request: %w", errNewRequest)
	}
	httpReq.Header = cloneHeader(req.Headers)
	helps.RecordAPIRequest(ctx, cfg, req.URL, method, httpReq.Header, req.Body, c.provider, c.auth)
	client := helps.NewProxyAwareHTTPClient(ctx, cfg, c.auth, 0)
	if client == nil {
		client = &http.Client{}
	}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, cfg, fmt.Errorf("execute host http request: %w", errDo)
	}
	return resp, cfg, nil
}

func (h *Host) currentRuntimeConfig() *config.Config {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runtimeConfig
}

package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	metaAudioFormat          = "meta-audio"
	MetaAudioMaxRequestBytes = 32 << 20
)

func metaImageExecutorAuth(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	prepared := auth.Clone()
	if prepared.Attributes == nil {
		prepared.Attributes = make(map[string]string)
	}
	prepared.Attributes["base_url"], prepared.Attributes["api_key"] = metaCredentials(auth)
	return prepared
}

// MetaAudioRequestModel reads the native multipart envelope without altering the WAV bytes.
func MetaAudioRequestModel(payload []byte, contentType string) (string, error) {
	if len(payload) > MetaAudioMaxRequestBytes {
		return "", fmt.Errorf("meta transcription request exceeds 32 MB")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" {
		return "", fmt.Errorf("meta transcription requires multipart/form-data with a boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(payload), parameters["boundary"])
	model := ""
	seenRequest, seenAudio := false, false
	for {
		part, errPart := reader.NextPart()
		if errPart == io.EOF {
			break
		}
		if errPart != nil {
			return "", fmt.Errorf("read transcription multipart: %w", errPart)
		}
		switch part.FormName() {
		case "request":
			if seenRequest {
				return "", fmt.Errorf("meta transcription requires one request part")
			}
			seenRequest = true
			settings, errRead := io.ReadAll(part)
			if errRead != nil || !gjson.ValidBytes(settings) || !gjson.ParseBytes(settings).IsObject() {
				return "", fmt.Errorf("meta transcription request part must be a JSON object")
			}
			model = strings.TrimSpace(gjson.GetBytes(settings, "model").String())
		case "audio":
			if seenAudio {
				return "", fmt.Errorf("meta transcription requires one audio part")
			}
			seenAudio = true
			var sample [1]byte
			if _, errRead := io.ReadFull(part, sample[:]); errRead != nil {
				return "", fmt.Errorf("meta transcription requires nonempty WAV audio")
			}
		default:
			return "", fmt.Errorf("meta transcription accepts request and audio parts")
		}
	}
	if !seenRequest || !seenAudio || model != "muse-voice-transcribe-1.0" {
		return "", fmt.Errorf("meta transcription requires request.model=muse-voice-transcribe-1.0 and audio")
	}
	return model, nil
}

func (e *MetaExecutor) audioRequest(
	ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options, reporter *usageReporter,
) (*http.Response, error) {
	model, err := MetaAudioRequestModel(req.Payload, opts.Headers.Get("Content-Type"))
	if err != nil {
		return nil, statusErr{
			code: http.StatusBadRequest, msg: err.Error(), requestScoped: true,
			dispatchPhase: cliproxyexecutor.DispatchPhaseNotSent,
		}
	}
	if err := bindFinalModelContext(&req, auth, opts, model); err != nil {
		return nil, err
	}
	baseURL, _ := metaCredentials(auth)
	endpoint := strings.TrimSuffix(baseURL, "/") + "/asr/transcribe"
	if sessionID := opts.Query.Get("sessionId"); sessionID != "" {
		endpoint += "?" + url.Values{"sessionId": {sessionID}}.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Payload))
	if err != nil {
		return nil, err
	}
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", opts.Headers.Get("Content-Type"))
	accept := opts.Headers.Get("Accept")
	if accept == "" {
		accept = "application/json"
	}
	httpReq.Header.Set("Accept", accept)
	recordUpstreamRequest(ctx, e.cfg, endpoint, httpReq.Header.Clone(), req.Payload, e.Identifier(), auth)
	response, err := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)).Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err, response)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, response.Header.Clone(), response)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		body, errRead := readXAIResponseBody(response.Body, maxXAIResponseBodyBytes)
		if errRead != nil {
			return nil, errRead
		}
		appendAPIResponseChunk(ctx, e.cfg, body)
		return nil, metaResponseError(response.StatusCode, body, response.Header)
	}
	return response, nil
}

func (e *MetaExecutor) executeAudio(
	ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (response cliproxyexecutor.Response, err error) {
	reporter := newUsageReporter(ctx, e, req.Model, auth)
	ctx = reporter.withContext(ctx)
	defer reporter.trackFailure(ctx, &err)
	upstream, err := e.audioRequest(ctx, auth, req, opts, reporter)
	if err != nil {
		return response, err
	}
	defer upstream.Body.Close()
	body, err := readXAIResponseBody(upstream.Body, maxXAIResponseBodyBytes)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return response, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	if strings.Contains(upstream.Header.Get("Content-Type"), "json") && !gjson.ValidBytes(body) {
		return response, markResponseStarted(
			statusErr{
				code: http.StatusBadGateway, msg: "Meta returned invalid transcription JSON",
			},
		)
	}
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: body, Headers: upstream.Header.Clone()}, nil
}

func (e *MetaExecutor) executeAudioStream(
	ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	reporter := newUsageReporter(ctx, e, req.Model, auth)
	ctx = reporter.withContext(ctx)
	reporter.markStream()
	defer reporter.trackFailure(ctx, &err)
	upstream, err := e.audioRequest(ctx, auth, req, opts, reporter)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer upstream.Body.Close()
		var streamErr error
		defer func() {
			if streamErr != nil {
				reporter.publishFailure(ctx, streamErr)
			} else {
				reporter.ensurePublished(ctx)
			}
		}()
		buffer := make([]byte, 32*1024)
		for {
			n, errRead := upstream.Body.Read(buffer)
			if n > 0 {
				data := bytes.Clone(buffer[:n])
				appendAPIResponseChunk(ctx, e.cfg, data)
				if !sendStreamChunk(ctx, chunks, cliproxyexecutor.StreamChunk{Payload: data}) {
					streamErr = ctx.Err()
					return
				}
			}
			if errRead == io.EOF {
				return
			}
			if errRead != nil {
				streamErr = markResponseStarted(errRead)
				recordAPIResponseError(ctx, e.cfg, streamErr)
				_ = sendStreamChunk(ctx, chunks, cliproxyexecutor.StreamChunk{Err: streamErr})
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: upstream.Header.Clone(), Chunks: chunks}, nil
}

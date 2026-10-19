package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type claudeThinkingReplayScope = kimiThinkingReplayScope

func claudeThinkingReplayEnabled(
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) bool {
	if auth == nil || !sourceFormatEqual(opts.SourceFormat, sdktranslator.FormatClaude) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") ||
		auth.AuthKind() != cliproxyauth.AuthKindAPIKey ||
		!helps.APIKeyModelIsCompat(req) {
		return false
	}
	apiKey, _ := claudeCreds(auth)
	return strings.TrimSpace(apiKey) != "" && !isClaudeOAuthToken(apiKey)
}

func claudeThinkingReplayScopeFromRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) claudeThinkingReplayScope {
	sessionKey := codexReasoningReplaySessionKey(
		ctx,
		sdktranslator.FormatClaude,
		req,
		opts,
		req.Payload,
	)
	sessionKey = xaiReasoningReplayIsolateSessionKey(ctx, sessionKey)
	return claudeThinkingReplayScope{
		modelFamily: claudeThinkingReplayModelFamily(auth, req.Model),
		sessionKey:  sessionKey,
	}
}

func claudeThinkingReplayModelFamily(auth *cliproxyauth.Auth, model string) string {
	baseModel := thinking.ParseSuffix(strings.TrimSpace(model)).ModelName
	if auth == nil || baseModel == "" {
		return ""
	}
	apiKey, baseURL := claudeCreds(auth)
	identity := strings.Join(
		[]string{
			strings.TrimSpace(auth.ID),
			strings.TrimSpace(baseURL),
			strings.TrimSpace(apiKey),
		},
		"\x00",
	)
	if strings.Trim(identity, "\x00") == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return "claude:" + hex.EncodeToString(sum[:8]) + ":" + baseModel
}

func prepareClaudeThinkingReplayRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Request, claudeThinkingReplayScope) {
	if !claudeThinkingReplayEnabled(auth, req, opts) {
		return req, claudeThinkingReplayScope{}
	}
	scope := claudeThinkingReplayScopeFromRequest(ctx, auth, req, opts)
	if !scope.valid() {
		return req, scope
	}
	contents, snapshot, found, errGet := internalcache.GetClaudeThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	scope.snapshot = snapshot
	scope.cacheReady = errGet == nil
	if errGet != nil {
		log.WithContext(ctx).Warnf("claude compatible thinking replay cache read failed: %v", errGet)
		return req, scope
	}
	if !found {
		return req, scope
	}
	updated, restored := restoreClaudeThinkingReplayContents(req.Payload, contents)
	if restored {
		req.Payload = updated
		scope.replayApplied = true
	}
	return req, scope
}

func restoreClaudeThinkingReplayContents(body []byte, cachedContents [][]byte) ([]byte, bool) {
	updated := bytes.Clone(body)
	restored := false
	for _, cachedContent := range cachedContents {
		var restoredTurn bool
		updated, restoredTurn = restoreKimiThinkingReplayContent(updated, cachedContent)
		restored = restored || restoredTurn
	}
	return updated, restored
}

func cacheClaudeThinkingReplayResponse(
	ctx context.Context,
	scope claudeThinkingReplayScope,
	response []byte,
) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	content := gjson.GetBytes(response, "content")
	if content.IsArray() {
		cacheClaudeThinkingReplayContent(ctx, scope, []byte(content.Raw))
		return
	}
	accumulator := newKimiThinkingReplayStreamAccumulator()
	accumulator.observe(response)
	if completedContent, completed := accumulator.content(); completed {
		cacheClaudeThinkingReplayContent(ctx, scope, completedContent)
	} else if accumulator.discardOldOnComplete {
		clearClaudeThinkingReplayContent(ctx, scope)
	}
}

func cacheClaudeThinkingReplayContent(
	ctx context.Context,
	scope claudeThinkingReplayScope,
	content []byte,
) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	if !kimiThinkingReplayContentIsReplayable(content) {
		clearClaudeThinkingReplayContent(ctx, scope)
		return
	}
	replaced, errReplace := internalcache.ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
		scope.snapshot,
		content,
	)
	if errReplace != nil {
		log.WithContext(ctx).Warnf("claude compatible thinking replay cache replace failed: %v", errReplace)
	} else if !replaced {
		log.WithContext(ctx).Debug("claude compatible thinking replay cache changed before replace")
	}
}

func clearClaudeThinkingReplayContent(ctx context.Context, scope claudeThinkingReplayScope) {
	if !scope.valid() || !scope.cacheReady {
		return
	}
	deleted, errDelete := internalcache.DeleteClaudeThinkingReplayIfUnchanged(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
		scope.snapshot,
	)
	if errDelete != nil {
		log.WithContext(ctx).Warnf("claude compatible thinking replay cache delete failed: %v", errDelete)
	} else if !deleted {
		log.WithContext(ctx).Debug("claude compatible thinking replay cache changed before delete")
	}
}

func clearClaudeThinkingReplayAfterError(
	ctx context.Context,
	scope claudeThinkingReplayScope,
	err error,
) {
	if scope.replayApplied && shouldClearKimiThinkingReplayAfterError(err) {
		clearClaudeThinkingReplayContent(ctx, scope)
	}
}

type claudeThinkingReplayStreamObserver struct {
	ctx         context.Context
	scope       claudeThinkingReplayScope
	accumulator *kimiThinkingReplayStreamAccumulator
}

func newClaudeThinkingReplayStreamObserver(
	ctx context.Context,
	scope claudeThinkingReplayScope,
) claudeStreamObserver {
	if !scope.valid() || !scope.cacheReady {
		return nil
	}
	return &claudeThinkingReplayStreamObserver{
		ctx:         ctx,
		scope:       scope,
		accumulator: newKimiThinkingReplayStreamAccumulator(),
	}
}

func (o *claudeThinkingReplayStreamObserver) ObserveClaudeSSELine(line []byte) {
	if o == nil || o.accumulator == nil {
		return
	}
	o.accumulator.observe(line)
}

func (o *claudeThinkingReplayStreamObserver) FinishClaudeStream(outcome claudeStreamOutcome) {
	if o == nil || o.accumulator == nil {
		return
	}
	// Incomplete, validation-failed, unreadable, and canceled streams preserve the existing replay state.
	// noinspection GoSwitchMissingCasesForIotaConsts
	switch outcome {
	case claudeStreamOutcomeComplete:
		finalizeCtx := context.WithoutCancel(o.ctx)
		if content, completed := o.accumulator.content(); completed {
			cacheClaudeThinkingReplayContent(finalizeCtx, o.scope, content)
		} else if o.accumulator.discardOldOnComplete {
			clearClaudeThinkingReplayContent(finalizeCtx, o.scope)
		}
	case claudeStreamOutcomeUpstreamError:
		if o.scope.replayApplied && o.accumulator.rejectsReplayState() {
			clearClaudeThinkingReplayContent(o.ctx, o.scope)
		}
	}
}

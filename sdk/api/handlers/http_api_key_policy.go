package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// ExecuteHTTPWithAPIKeyPolicy applies client policy to a fixed-provider HTTP operation.
// dispatch must be called immediately before the first upstream request is sent.
func (h *BaseAPIHandler) ExecuteHTTPWithAPIKeyPolicy(
	ctx context.Context,
	handlerType, provider, requestedModel, routedModel string,
	operation func(context.Context, string, func()) *ErrorMessage,
) *ErrorMessage {
	ctx, release, err := h.InferenceAdmission.Acquire(ctx, nil)
	if err != nil {
		return &ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: err}
	}
	defer release()
	ctx, _, model, reservation, policyError := h.prepareAPIKeyTarget(
		ctx, handlerType, requestedModel, newExecutionRequestID(),
		func(mapped string) ([]string, string, *ErrorMessage) {
			if routedModel != requestedModel {
				mapped = routedModel
				if cfg := h.ConfigSnapshot(); cfg != nil {
					var mappingError error
					mapped, _, mappingError = cfg.ResolveAPIKeyModel(apiKeyFromContext(ctx), routedModel)
					if mappingError != nil {
						return nil, "", apiKeyPolicyError(handlerType, http.StatusServiceUnavailable, time.Time{})
					}
				}
			}
			return []string{strings.TrimSpace(provider)}, strings.TrimSpace(mapped), nil
		},
	)
	if policyError != nil {
		return policyError
	}
	dispatched := false
	result := operation(ctx, model, func() { dispatched = true })
	if reservation == nil {
		return result
	}
	if !dispatched {
		reservation.release(ctx, "HTTP operation ended before dispatch")
	} else if result != nil || reservation.unknownUsage || ctx.Err() != nil {
		reservation.markUnknown(ctx)
	} else {
		transitionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiKeyReservationTransitionTimeout)
		defer cancel()
		if errCommit := reservation.store.CommitAPIKeyReservation(
			transitionCtx, reservation.id, "HTTP operation completed",
		); errCommit != nil {
			log.WithError(errCommit).Error("API key HTTP reservation commit failed")
		}
	}
	return result
}

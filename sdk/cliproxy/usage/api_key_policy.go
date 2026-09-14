package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const APIKeyIdentityPrefix = "sha256:"

// HashAPIKeyIdentity derives the stable irreversible identity persisted for a client API key.
func HashAPIKeyIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(raw))
	return APIKeyIdentityPrefix + hex.EncodeToString(digest[:])
}

// EnsureAPIKeyIdentity preserves a canonical identity and hashes every other non-empty value.
func EnsureAPIKeyIdentity(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == len(APIKeyIdentityPrefix)+sha256.Size*2 && strings.HasPrefix(value, APIKeyIdentityPrefix) {
		digest := value[len(APIKeyIdentityPrefix):]
		if _, err := hex.DecodeString(digest); err == nil {
			return APIKeyIdentityPrefix + strings.ToLower(digest)
		}
	}
	return HashAPIKeyIdentity(value)
}

type apiKeyIdentityContextKey struct{}
type apiKeyReservationContextKey struct{}

// APIKeyPolicyLimit is one persistent hard-limit window.
type APIKeyPolicyLimit struct {
	Window    time.Duration
	Requests  int64
	Tokens    int64
	CostMicro int64
}

// APIKeyReservationRequest describes one logical inbound request before dispatch.
type APIKeyReservationRequest struct {
	KeyIdentity      string
	LogicalRequestID string
	PolicyProfile    string
	RequestedModel   string
	CanonicalModel   string
	Providers        []string
	TTL              time.Duration
	Limits           []APIKeyPolicyLimit
	Now              time.Time
}

// APIKeyReservation identifies a durable logical-request reservation.
type APIKeyReservation struct {
	ID        string
	ExpiresAt time.Time
	Reused    bool
}

// APIKeyLimitExceededError reports the first instant when the rejected window no longer applies.
type APIKeyLimitExceededError struct {
	RetryAt time.Time
}

func (e *APIKeyLimitExceededError) Error() string {
	if e == nil || e.RetryAt.IsZero() {
		return "API key policy limit exceeded"
	}
	return fmt.Sprintf("API key policy limit exceeded until %s", e.RetryAt.UTC().Format(time.RFC3339))
}

// APIKeyReservationReplayError reports reuse of a reservation that already reached a terminal state.
type APIKeyReservationReplayError struct {
	ReservationID string
	State         string
}

func (e *APIKeyReservationReplayError) Error() string {
	if e == nil || e.State == "" {
		return "API key reservation is already terminal"
	}
	return fmt.Sprintf("API key reservation is already %s", e.State)
}

// APIKeyReservationStore owns durable reservation state.
type APIKeyReservationStore interface {
	ReserveAPIKey(context.Context, APIKeyReservationRequest) (APIKeyReservation, error)
	ReleaseAPIKeyReservation(context.Context, string, string) error
	MarkAPIKeyReservationUnknown(context.Context, string, string) error
	CommitAPIKeyReservation(context.Context, string, string) error
}

// WithAPIKeyIdentity stores a stable irreversible client API key identity.
func WithAPIKeyIdentity(ctx context.Context, identity string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ctx
	}
	return context.WithValue(ctx, apiKeyIdentityContextKey{}, identity)
}

// APIKeyIdentityFromContext returns the stable client API key identity.
func APIKeyIdentityFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	identity, _ := ctx.Value(apiKeyIdentityContextKey{}).(string)
	return strings.TrimSpace(identity)
}

// WithAPIKeyReservation stores the reservation shared by all attempts of one logical request.
func WithAPIKeyReservation(ctx context.Context, reservationID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return ctx
	}
	return context.WithValue(ctx, apiKeyReservationContextKey{}, reservationID)
}

// APIKeyReservationFromContext returns the logical-request reservation ID.
func APIKeyReservationFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	reservationID, _ := ctx.Value(apiKeyReservationContextKey{}).(string)
	return strings.TrimSpace(reservationID)
}

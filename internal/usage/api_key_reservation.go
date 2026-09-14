package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

const (
	defaultAPIKeyReservationTTL = 24 * time.Hour
	maxReservationReasonBytes   = 1024
	apiKeyReservationClockKey   = "api_key_reservation_clock_ns"
)

type apiKeyReservationWindow struct {
	windowNs          int64
	startNs           int64
	endNs             int64
	maxRequests       int64
	maxTokens         int64
	maxCostMicro      int64
	reservedRequests  int64
	reservedTokens    int64
	reservedCostMicro int64
}

type reservationSQLExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) ReserveAPIKey(
	ctx context.Context,
	request coreusage.APIKeyReservationRequest,
) (coreusage.APIKeyReservation, error) {
	if s == nil || s.db == nil {
		return coreusage.APIKeyReservation{}, errors.New("usage: API key reservation store is unavailable")
	}
	request, limits, err := normalizeAPIKeyReservationRequest(request)
	if err != nil {
		return coreusage.APIKeyReservation{}, err
	}
	reservationID := apiKeyReservationID(request.KeyIdentity, request.LogicalRequestID, request.PolicyProfile)
	providersJSON, err := json.Marshal(request.Providers)
	if err != nil {
		return coreusage.APIKeyReservation{}, fmt.Errorf("usage: encode reservation providers: %w", err)
	}
	requestedNs, err := unixNanoUTC(request.Now)
	if err != nil {
		return coreusage.APIKeyReservation{}, fmt.Errorf("usage: invalid reservation time: %w", err)
	}

	result := coreusage.APIKeyReservation{ID: reservationID}
	var policyError error
	err = s.withImmediateReservationTransaction(
		ctx, func(exec reservationSQLExecutor) error {
			createdNs, errNow := effectiveAPIKeyReservationNow(ctx, exec, requestedNs)
			if errNow != nil {
				return errNow
			}
			expiresNs, errExpiry := apiKeyReservationExpiry(createdNs, request.TTL)
			if errExpiry != nil {
				return errExpiry
			}
			result.ExpiresAt = time.Unix(0, expiresNs).UTC()
			if errExpire := expireAPIKeyReservations(ctx, exec, createdNs); errExpire != nil {
				return errExpire
			}
			var existingExpiresNs int64
			var existingKey, existingRequest, existingProfile, existingModel, existingState string
			errExisting := exec.QueryRowContext(
				ctx,
				`SELECT key_identity, logical_request_id, policy_profile, canonical_model, state, expires_at_ns
			   FROM api_key_reservation
			  WHERE id = ?`,
				reservationID,
			).Scan(
				&existingKey,
				&existingRequest,
				&existingProfile,
				&existingModel,
				&existingState,
				&existingExpiresNs,
			)
			if errExisting == nil {
				if existingKey != request.KeyIdentity || existingRequest != request.LogicalRequestID ||
					existingProfile != request.PolicyProfile || existingModel != request.CanonicalModel {
					return errors.New("usage: logical request reservation identity conflict")
				}
				result.ExpiresAt = time.Unix(0, existingExpiresNs).UTC()
				if existingState == "open" || existingState == "unknown" {
					result.Reused = true
					return nil
				}
				policyError = &coreusage.APIKeyReservationReplayError{
					ReservationID: reservationID,
					State:         existingState,
				}
				return nil
			}
			if !errors.Is(errExisting, sql.ErrNoRows) {
				return fmt.Errorf("usage: query existing API key reservation: %w", errExisting)
			}

			windows := make([]apiKeyReservationWindow, 0, len(limits))
			for _, limit := range limits {
				window, errWindow := buildAPIKeyReservationWindow(ctx, exec, request, limit, createdNs)
				if errWindow != nil {
					if _, ok := errors.AsType[*coreusage.APIKeyLimitExceededError](errWindow); ok {
						policyError = errWindow
						return nil
					}
					return errWindow
				}
				windows = append(windows, window)
			}
			if _, errInsert := exec.ExecContext(
				ctx,
				`INSERT INTO api_key_reservation (
			    id, key_identity, logical_request_id, policy_profile,
			    requested_model, canonical_model, providers_json, state,
			    created_at_ns, expires_at_ns, updated_at_ns, terminal_at_ns, terminal_reason
			) VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, NULL, '')`,
				reservationID,
				request.KeyIdentity,
				request.LogicalRequestID,
				request.PolicyProfile,
				request.RequestedModel,
				request.CanonicalModel,
				string(providersJSON),
				createdNs,
				expiresNs,
				createdNs,
			); errInsert != nil {
				return fmt.Errorf("usage: insert API key reservation: %w", errInsert)
			}
			for _, window := range windows {
				if _, errInsert := exec.ExecContext(
					ctx,
					`INSERT INTO api_key_reservation_window (
				    reservation_id, window_ns, window_start_ns, window_end_ns,
				    max_requests, max_tokens, max_cost_micro,
				    reserved_requests, reserved_tokens, reserved_cost_micro,
				    actual_requests, actual_tokens, actual_cost_micro
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0)`,
					reservationID,
					window.windowNs,
					window.startNs,
					window.endNs,
					window.maxRequests,
					window.maxTokens,
					window.maxCostMicro,
					window.reservedRequests,
					window.reservedTokens,
					window.reservedCostMicro,
				); errInsert != nil {
					return fmt.Errorf("usage: insert API key reservation window: %w", errInsert)
				}
			}
			return nil
		},
	)
	if err != nil {
		return coreusage.APIKeyReservation{}, err
	}
	if policyError != nil {
		return coreusage.APIKeyReservation{}, policyError
	}
	return result, nil
}

func normalizeAPIKeyReservationRequest(
	request coreusage.APIKeyReservationRequest,
) (coreusage.APIKeyReservationRequest, []coreusage.APIKeyPolicyLimit, error) {
	request.KeyIdentity = strings.TrimSpace(request.KeyIdentity)
	request.LogicalRequestID = strings.TrimSpace(request.LogicalRequestID)
	request.PolicyProfile = strings.TrimSpace(request.PolicyProfile)
	request.RequestedModel = strings.TrimSpace(request.RequestedModel)
	request.CanonicalModel = strings.TrimSpace(request.CanonicalModel)
	if request.PolicyProfile == "" {
		request.PolicyProfile = "default"
	}
	if request.TTL <= 0 {
		request.TTL = defaultAPIKeyReservationTTL
	}
	if !validAPIKeyIdentity(request.KeyIdentity) {
		return request, nil, errors.New("usage: invalid API key identity")
	}
	if request.LogicalRequestID == "" || len(request.LogicalRequestID) > 512 {
		return request, nil, errors.New("usage: invalid logical request identity")
	}
	if !validPolicyProfile(request.PolicyProfile) {
		return request, nil, errors.New("usage: invalid API key policy profile")
	}
	if request.CanonicalModel == "" {
		return request, nil, errors.New("usage: empty canonical model")
	}
	providers := make([]string, 0, len(request.Providers))
	seenProviders := make(map[string]struct{}, len(request.Providers))
	for _, provider := range request.Providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		if _, duplicate := seenProviders[provider]; duplicate {
			continue
		}
		seenProviders[provider] = struct{}{}
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	request.Providers = providers
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	} else {
		request.Now = request.Now.UTC()
	}

	limits := append([]coreusage.APIKeyPolicyLimit(nil), request.Limits...)
	sort.Slice(limits, func(i, j int) bool { return limits[i].Window < limits[j].Window })
	seenWindows := make(map[time.Duration]struct{}, len(limits))
	for _, limit := range limits {
		if limit.Window <= 0 {
			return request, nil, errors.New("usage: API key policy window must be positive")
		}
		if _, duplicate := seenWindows[limit.Window]; duplicate {
			return request, nil, errors.New("usage: API key policy contains duplicate windows")
		}
		seenWindows[limit.Window] = struct{}{}
		if limit.Requests < 0 || limit.Tokens < 0 || limit.CostMicro < 0 {
			return request, nil, errors.New("usage: API key policy limits must be non-negative")
		}
		if limit.Requests == 0 && limit.Tokens == 0 && limit.CostMicro == 0 {
			return request, nil, errors.New("usage: API key policy window has no enabled dimension")
		}
	}
	if len(limits) == 0 {
		return request, nil, errors.New("usage: API key reservation requires at least one policy window")
	}
	return request, limits, nil
}

func effectiveAPIKeyReservationNow(
	ctx context.Context,
	exec reservationSQLExecutor,
	requestedNs int64,
) (int64, error) {
	requested := strconv.FormatInt(requestedNs, 10)
	if _, err := exec.ExecContext(
		ctx,
		"INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)",
		apiKeyReservationClockKey,
		requested,
	); err != nil {
		return 0, fmt.Errorf("usage: initialize API key reservation clock: %w", err)
	}
	var persisted string
	if err := exec.QueryRowContext(
		ctx,
		"SELECT value FROM meta WHERE key = ?",
		apiKeyReservationClockKey,
	).Scan(&persisted); err != nil {
		return 0, fmt.Errorf("usage: read API key reservation clock: %w", err)
	}
	persistedNs, err := strconv.ParseInt(strings.TrimSpace(persisted), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("usage: parse API key reservation clock %q: %w", persisted, err)
	}
	if persistedNs >= requestedNs {
		return persistedNs, nil
	}
	if _, err = exec.ExecContext(
		ctx,
		"UPDATE meta SET value = ? WHERE key = ?",
		requested,
		apiKeyReservationClockKey,
	); err != nil {
		return 0, fmt.Errorf("usage: update API key reservation clock: %w", err)
	}
	return requestedNs, nil
}

func apiKeyReservationExpiry(createdNs int64, ttl time.Duration) (int64, error) {
	ttlNs := int64(ttl)
	if ttlNs <= 0 || createdNs > math.MaxInt64-ttlNs {
		return 0, errors.New("usage: API key reservation expiry is outside the supported time range")
	}
	expiresNs := createdNs + ttlNs
	if expiresNs <= createdNs {
		return 0, errors.New("usage: API key reservation expiry is outside the supported time range")
	}
	return expiresNs, nil
}

func buildAPIKeyReservationWindow(
	ctx context.Context,
	exec reservationSQLExecutor,
	request coreusage.APIKeyReservationRequest,
	limit coreusage.APIKeyPolicyLimit,
	nowNs int64,
) (apiKeyReservationWindow, error) {
	windowNs := int64(limit.Window)
	if windowNs <= 0 {
		return apiKeyReservationWindow{}, errors.New("usage: API key policy window exceeds int64")
	}
	remainder := floorModInt64(nowNs, windowNs)
	if nowNs < math.MinInt64+remainder {
		return apiKeyReservationWindow{}, errors.New("usage: API key policy window start overflows int64")
	}
	startNs := nowNs - remainder
	if startNs > math.MaxInt64-windowNs {
		return apiKeyReservationWindow{}, errors.New("usage: API key policy window end overflows int64")
	}
	endNs := startNs + windowNs
	var usedRequests, usedTokens, usedCost int64
	if err := exec.QueryRowContext(
		ctx,
		`SELECT
		    COALESCE(SUM(CASE
		        WHEN reservation.state IN ('open', 'unknown') THEN window.reserved_requests
		        WHEN reservation.state = 'released' THEN 0
		        ELSE window.actual_requests
		    END), 0),
		    COALESCE(SUM(CASE
		        WHEN reservation.state IN ('open', 'unknown') THEN window.reserved_tokens
		        WHEN reservation.state = 'released' THEN 0
		        ELSE window.actual_tokens
		    END), 0),
		    COALESCE(SUM(CASE
		        WHEN reservation.state IN ('open', 'unknown') THEN window.reserved_cost_micro
		        WHEN reservation.state = 'released' THEN 0
		        ELSE window.actual_cost_micro
		    END), 0)
		   FROM api_key_reservation_window AS window
		   JOIN api_key_reservation AS reservation ON reservation.id = window.reservation_id
		  WHERE reservation.key_identity = ?
		    AND reservation.policy_profile = ?
		    AND window.window_ns = ?
		    AND window.window_start_ns = ?`,
		request.KeyIdentity,
		request.PolicyProfile,
		windowNs,
		startNs,
	).Scan(&usedRequests, &usedTokens, &usedCost); err != nil {
		return apiKeyReservationWindow{}, fmt.Errorf("usage: query API key policy window: %w", err)
	}
	window := apiKeyReservationWindow{
		windowNs:     windowNs,
		startNs:      startNs,
		endNs:        endNs,
		maxRequests:  limit.Requests,
		maxTokens:    limit.Tokens,
		maxCostMicro: limit.CostMicro,
	}
	if limit.Requests > 0 {
		if usedRequests >= limit.Requests {
			return apiKeyReservationWindow{}, &coreusage.APIKeyLimitExceededError{RetryAt: time.Unix(0, endNs).UTC()}
		}
		window.reservedRequests = 1
	}
	if limit.Tokens > 0 {
		if usedTokens >= limit.Tokens {
			return apiKeyReservationWindow{}, &coreusage.APIKeyLimitExceededError{RetryAt: time.Unix(0, endNs).UTC()}
		}
		window.reservedTokens = limit.Tokens - usedTokens
	}
	if limit.CostMicro > 0 {
		if usedCost >= limit.CostMicro {
			return apiKeyReservationWindow{}, &coreusage.APIKeyLimitExceededError{RetryAt: time.Unix(0, endNs).UTC()}
		}
		window.reservedCostMicro = limit.CostMicro - usedCost
	}
	return window, nil
}

func expireAPIKeyReservations(ctx context.Context, exec reservationSQLExecutor, nowNs int64) error {
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE api_key_reservation_window
		    SET actual_requests = reserved_requests,
		        actual_tokens = reserved_tokens,
		        actual_cost_micro = reserved_cost_micro
		  WHERE reservation_id IN (
		      SELECT id FROM api_key_reservation
		       WHERE state IN ('open', 'unknown') AND expires_at_ns <= ?
		  )`,
		nowNs,
	); err != nil {
		return fmt.Errorf("usage: commit expired API key reservation windows: %w", err)
	}
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE api_key_reservation
		    SET state = 'expired_committed', updated_at_ns = ?, terminal_at_ns = ?,
		        terminal_reason = 'reservation lease expired'
		  WHERE state IN ('open', 'unknown') AND expires_at_ns <= ?`,
		nowNs,
		nowNs,
		nowNs,
	); err != nil {
		return fmt.Errorf("usage: expire API key reservations: %w", err)
	}
	return nil
}

func (s *Store) ReleaseAPIKeyReservation(ctx context.Context, reservationID, reason string) error {
	return s.transitionAPIKeyReservation(ctx, reservationID, reason, "released", false)
}

func (s *Store) MarkAPIKeyReservationUnknown(ctx context.Context, reservationID, reason string) error {
	return s.transitionAPIKeyReservation(ctx, reservationID, reason, "unknown", false)
}

func (s *Store) CommitAPIKeyReservation(ctx context.Context, reservationID, reason string) error {
	return s.transitionAPIKeyReservation(ctx, reservationID, reason, "committed", true)
}

func (s *Store) transitionAPIKeyReservation(
	ctx context.Context,
	reservationID,
	reason,
	target string,
	commitReserved bool,
) error {
	if s == nil || s.db == nil {
		return errors.New("usage: API key reservation store is unavailable")
	}
	reservationID = strings.TrimSpace(reservationID)
	if !validReservationID(reservationID) {
		return errors.New("usage: invalid API key reservation ID")
	}
	reason = sanitizeReservationReason(reason)
	requestedNs := time.Now().UTC().UnixNano()
	return s.withImmediateReservationTransaction(
		ctx, func(exec reservationSQLExecutor) error {
			nowNs, errNow := effectiveAPIKeyReservationNow(ctx, exec, requestedNs)
			if errNow != nil {
				return errNow
			}
			var state string
			var createdNs, updatedNs int64
			if err := exec.QueryRowContext(
				ctx,
				"SELECT state, created_at_ns, updated_at_ns FROM api_key_reservation WHERE id = ?",
				reservationID,
			).Scan(&state, &createdNs, &updatedNs); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errors.New("usage: API key reservation does not exist")
				}
				return fmt.Errorf("usage: query API key reservation state: %w", err)
			}
			if nowNs < createdNs {
				nowNs = createdNs
			}
			if nowNs < updatedNs {
				nowNs = updatedNs
			}
			if target == "committed" {
				if state == "committed" || state == "expired_committed" {
					return nil
				}
				if state != "open" && state != "unknown" && state != "settled" {
					return nil
				}
			} else {
				if state != "open" && state != "unknown" {
					return nil
				}
				if target == "unknown" && state == "unknown" {
					return nil
				}
			}
			if commitReserved {
				if _, err := exec.ExecContext(
					ctx,
					`UPDATE api_key_reservation_window
				    SET actual_requests = MAX(actual_requests, reserved_requests),
				        actual_tokens = MAX(actual_tokens, reserved_tokens),
				        actual_cost_micro = MAX(actual_cost_micro, reserved_cost_micro)
				  WHERE reservation_id = ?`,
					reservationID,
				); err != nil {
					return fmt.Errorf("usage: commit API key reservation windows: %w", err)
				}
			}
			var result sql.Result
			var err error
			switch target {
			case "unknown":
				result, err = exec.ExecContext(
					ctx,
					`UPDATE api_key_reservation
				    SET state = 'unknown', updated_at_ns = ?, terminal_reason = ?
				  WHERE id = ? AND state = 'open'`,
					nowNs,
					reason,
					reservationID,
				)
			case "released":
				result, err = exec.ExecContext(
					ctx,
					`UPDATE api_key_reservation
				    SET state = 'released', updated_at_ns = ?, terminal_at_ns = ?, terminal_reason = ?
				  WHERE id = ? AND state IN ('open', 'unknown')`,
					nowNs,
					nowNs,
					reason,
					reservationID,
				)
			case "committed":
				result, err = exec.ExecContext(
					ctx,
					`UPDATE api_key_reservation
				    SET state = 'committed', updated_at_ns = ?, terminal_at_ns = ?, terminal_reason = ?
				  WHERE id = ? AND state IN ('open', 'unknown', 'settled')`,
					nowNs,
					nowNs,
					reason,
					reservationID,
				)
			default:
				return errors.New("usage: invalid API key reservation transition")
			}
			if err != nil {
				return fmt.Errorf("usage: transition API key reservation to %s: %w", target, err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("usage: inspect API key reservation transition: %w", err)
			}
			if affected != 1 {
				return errors.New("usage: API key reservation state changed concurrently")
			}
			return nil
		},
	)
}

func settleAPIKeyReservationEvent(
	ctx context.Context,
	tx *sql.Tx,
	reservationID string,
	fingerprint []byte,
	tokens int64,
	cost costResult,
	failed bool,
) error {
	reservationID = strings.TrimSpace(reservationID)
	if reservationID == "" {
		return nil
	}
	if tx == nil || !validReservationID(reservationID) || len(fingerprint) != eventFingerprintBytes {
		return errors.New("usage: invalid API key reservation settlement")
	}
	if failed && tokens == 0 && cost.Micro == 0 {
		return nil
	}
	nowNs, err := effectiveAPIKeyReservationNow(ctx, tx, time.Now().UTC().UnixNano())
	if err != nil {
		return err
	}
	if err = expireAPIKeyReservations(ctx, tx, nowNs); err != nil {
		return err
	}
	var state string
	var createdNs, updatedNs int64
	if err = tx.QueryRowContext(
		ctx,
		"SELECT state, created_at_ns, updated_at_ns FROM api_key_reservation WHERE id = ?",
		reservationID,
	).Scan(&state, &createdNs, &updatedNs); err != nil {
		return fmt.Errorf("usage: query API key reservation for settlement: %w", err)
	}
	result, err := tx.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO api_key_reservation_event (
		    reservation_id, event_fingerprint, total_tokens, cost_micro, cost_status, created_at_ns
		) VALUES (?, ?, ?, ?, ?, ?)`,
		reservationID,
		fingerprint,
		tokens,
		cost.Micro,
		cost.Status,
		nowNs,
	)
	if err != nil {
		return fmt.Errorf("usage: insert API key reservation event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("usage: inspect API key reservation event insert: %w", err)
	}
	if inserted == 0 {
		return nil
	}
	var totalTokens, totalCost, incompleteCost int64
	if err = tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(SUM(total_tokens), 0),
		        COALESCE(SUM(cost_micro), 0),
		        COALESCE(SUM(CASE WHEN cost_status = 'complete' THEN 0 ELSE 1 END), 0)
		   FROM api_key_reservation_event
		  WHERE reservation_id = ?`,
		reservationID,
	).Scan(&totalTokens, &totalCost, &incompleteCost); err != nil {
		return fmt.Errorf("usage: aggregate API key reservation events: %w", err)
	}
	terminalCommitted := state == "committed" || state == "expired_committed"
	preserveReservedCost := terminalCommitted || incompleteCost > 0
	if _, err = tx.ExecContext(
		ctx,
		`UPDATE api_key_reservation_window
		    SET actual_requests = MAX(actual_requests, 1, CASE WHEN ? THEN reserved_requests ELSE 0 END),
		        actual_tokens = MAX(actual_tokens, ?, CASE WHEN ? THEN reserved_tokens ELSE 0 END),
		        actual_cost_micro = MAX(actual_cost_micro, ?, CASE WHEN ? THEN reserved_cost_micro ELSE 0 END)
		  WHERE reservation_id = ?`,
		terminalCommitted,
		totalTokens,
		terminalCommitted,
		totalCost,
		preserveReservedCost,
		reservationID,
	); err != nil {
		return fmt.Errorf("usage: settle API key reservation windows: %w", err)
	}
	if terminalCommitted {
		return nil
	}
	if nowNs < createdNs {
		nowNs = createdNs
	}
	if nowNs < updatedNs {
		nowNs = updatedNs
	}
	if _, err = tx.ExecContext(
		ctx,
		`UPDATE api_key_reservation
		    SET state = 'settled', updated_at_ns = ?, terminal_at_ns = ?, terminal_reason = 'usage settled'
		  WHERE id = ? AND state IN ('open', 'unknown', 'settled', 'released')`,
		nowNs,
		nowNs,
		reservationID,
	); err != nil {
		return fmt.Errorf("usage: settle API key reservation: %w", err)
	}
	return nil
}

func (s *Store) withImmediateReservationTransaction(
	ctx context.Context,
	fn func(reservationSQLExecutor) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("usage: acquire API key reservation connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("usage: begin immediate API key reservation transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("usage: commit API key reservation transaction: %w", err)
	}
	committed = true
	return nil
}

func apiKeyReservationID(keyIdentity, logicalRequestID, profile string) string {
	digest := sha256.Sum256([]byte(keyIdentity + "\x00" + logicalRequestID + "\x00" + profile))
	return "rsv_" + hex.EncodeToString(digest[:])
}

func validReservationID(value string) bool {
	if len(value) != 68 || !strings.HasPrefix(value, "rsv_") || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[4:])
	return err == nil
}

func validAPIKeyIdentity(value string) bool {
	if len(value) != len(coreusage.APIKeyIdentityPrefix)+64 ||
		!strings.HasPrefix(value, coreusage.APIKeyIdentityPrefix) || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[len(coreusage.APIKeyIdentityPrefix):])
	return err == nil
}

func validPolicyProfile(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := range len(value) {
		ch := value[index]
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') &&
			(ch < '0' || ch > '9') && ch != '.' && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

func sanitizeReservationReason(reason string) string {
	reason = strings.TrimSpace(strings.ToValidUTF8(reason, "�"))
	if len(reason) <= maxReservationReasonBytes {
		return reason
	}
	limit := maxReservationReasonBytes
	for limit > 0 && !utf8.RuneStart(reason[limit]) {
		limit--
	}
	return reason[:limit]
}

func floorModInt64(value, divisor int64) int64 {
	remainder := value % divisor
	if remainder < 0 {
		remainder += divisor
	}
	return remainder
}

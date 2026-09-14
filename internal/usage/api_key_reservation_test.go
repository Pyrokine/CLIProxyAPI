package usage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func apiKeyReservationRequest(
	now time.Time,
	logicalRequestID string,
	limits ...coreusage.APIKeyPolicyLimit,
) coreusage.APIKeyReservationRequest {
	return coreusage.APIKeyReservationRequest{
		KeyIdentity:      coreusage.HashAPIKeyIdentity("client-key"),
		LogicalRequestID: logicalRequestID,
		PolicyProfile:    "default",
		RequestedModel:   "alias-model",
		CanonicalModel:   "canonical-model",
		Providers:        []string{"OpenAI", "anthropic", "openai", ""},
		TTL:              time.Hour,
		Limits:           limits,
		Now:              now,
	}
}

func readAPIKeyReservationState(t *testing.T, store *Store, reservationID string) string {
	t.Helper()
	var state string
	if err := store.db.QueryRow(
		"SELECT state FROM api_key_reservation WHERE id = ?",
		reservationID,
	).Scan(&state); err != nil {
		t.Fatalf("query reservation state: %v", err)
	}
	return state
}

func readAPIKeyReservationWindow(
	t *testing.T,
	store *Store,
	reservationID string,
) (reservedRequests, reservedTokens, reservedCost, actualRequests, actualTokens, actualCost int64) {
	t.Helper()
	if err := store.db.QueryRow(
		`SELECT reserved_requests, reserved_tokens, reserved_cost_micro,
		        actual_requests, actual_tokens, actual_cost_micro
		   FROM api_key_reservation_window
		  WHERE reservation_id = ?`,
		reservationID,
	).Scan(
		&reservedRequests,
		&reservedTokens,
		&reservedCost,
		&actualRequests,
		&actualTokens,
		&actualCost,
	); err != nil {
		t.Fatalf("query reservation window: %v", err)
	}
	return
}

func TestReserveAPIKeyPersistsCanonicalRequestSnapshot(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 8, 1, 2, 3, 4, time.UTC)
	request := apiKeyReservationRequest(
		now,
		"request-1",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
	)

	reservation, err := store.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatalf("ReserveAPIKey: %v", err)
	}
	if !validReservationID(reservation.ID) || reservation.Reused {
		t.Fatalf("reservation = %+v, want new canonical ID", reservation)
	}
	if !reservation.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expiresAt = %s, want %s", reservation.ExpiresAt, now.Add(time.Hour))
	}
	var keyIdentity, providersJSON, state string
	if err = store.db.QueryRow(
		`SELECT key_identity, providers_json, state
		   FROM api_key_reservation WHERE id = ?`,
		reservation.ID,
	).Scan(&keyIdentity, &providersJSON, &state); err != nil {
		t.Fatalf("query reservation: %v", err)
	}
	if keyIdentity != request.KeyIdentity || providersJSON != `["anthropic","openai"]` || state != "open" {
		t.Fatalf("persisted reservation = %q/%s/%s", keyIdentity, providersJSON, state)
	}
	reservedRequests, reservedTokens, reservedCost, actualRequests, actualTokens, actualCost :=
		readAPIKeyReservationWindow(t, store, reservation.ID)
	if reservedRequests != 1 || reservedTokens != 100 || reservedCost != 1_000 ||
		actualRequests != 0 || actualTokens != 0 || actualCost != 0 {
		t.Fatalf(
			"window = reserved %d/%d/%d actual %d/%d/%d",
			reservedRequests,
			reservedTokens,
			reservedCost,
			actualRequests,
			actualTokens,
			actualCost,
		)
	}
}

func TestReserveAPIKeyReusesLogicalRequestAndRejectsExhaustedWindow(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 8, 1, 15, 0, 0, time.UTC)
	limit := coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1}
	request := apiKeyReservationRequest(now, "request-1", limit)

	first, err := store.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatalf("first ReserveAPIKey: %v", err)
	}
	second, err := store.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatalf("second ReserveAPIKey: %v", err)
	}
	if second.ID != first.ID || !second.Reused {
		t.Fatalf("reused reservation = %+v, want ID %q", second, first.ID)
	}
	request.LogicalRequestID = "request-2"
	_, err = store.ReserveAPIKey(context.Background(), request)
	var limitError *coreusage.APIKeyLimitExceededError
	if !errors.As(err, &limitError) {
		t.Fatalf("exhausted window error = %v, want APIKeyLimitExceededError", err)
	}
	wantRetryAt := now.Truncate(time.Hour).Add(time.Hour)
	if !limitError.RetryAt.Equal(wantRetryAt) {
		t.Fatalf("retryAt = %s, want %s", limitError.RetryAt, wantRetryAt)
	}
}

func TestReserveAPIKeySerializesConcurrentLimitChecks(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	const attempts = 16
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for index := range attempts {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := store.ReserveAPIKey(
				context.Background(),
				apiKeyReservationRequest(
					now,
					fmt.Sprintf("request-%d", index),
					coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1},
				),
			)
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	var succeeded, limited int
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if _, ok := errors.AsType[*coreusage.APIKeyLimitExceededError](err); ok {
			limited++
			continue
		}
		t.Fatalf("concurrent ReserveAPIKey error: %v", err)
	}
	if succeeded != 1 || limited != attempts-1 {
		t.Fatalf("concurrent results = %d succeeded/%d limited", succeeded, limited)
	}
}

func TestAPIKeyReservationTransitionsAreIdempotent(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	request := apiKeyReservationRequest(
		now,
		"request-1",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 2, Tokens: 100, CostMicro: 500},
	)
	reservation, err := store.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkAPIKeyReservationUnknown(
		context.Background(), reservation.ID, "dispatch outcome unknown",
	); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if err = store.MarkAPIKeyReservationUnknown(context.Background(), reservation.ID, "duplicate"); err != nil {
		t.Fatalf("mark unknown again: %v", err)
	}
	if state := readAPIKeyReservationState(t, store, reservation.ID); state != "unknown" {
		t.Fatalf("state = %q, want unknown", state)
	}
	if err = store.CommitAPIKeyReservation(context.Background(), reservation.ID, "processed failure"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err = store.CommitAPIKeyReservation(context.Background(), reservation.ID, "duplicate"); err != nil {
		t.Fatalf("commit again: %v", err)
	}
	if err = store.ReleaseAPIKeyReservation(context.Background(), reservation.ID, "late release"); err != nil {
		t.Fatalf("release committed reservation: %v", err)
	}
	if state := readAPIKeyReservationState(t, store, reservation.ID); state != "committed" {
		t.Fatalf("state = %q, want committed", state)
	}
	reservedRequests, reservedTokens, reservedCost, actualRequests, actualTokens, actualCost :=
		readAPIKeyReservationWindow(t, store, reservation.ID)
	if actualRequests != reservedRequests || actualTokens != reservedTokens || actualCost != reservedCost {
		t.Fatalf(
			"committed actual = %d/%d/%d, want reserved %d/%d/%d",
			actualRequests,
			actualTokens,
			actualCost,
			reservedRequests,
			reservedTokens,
			reservedCost,
		)
	}
	if err = store.ReleaseAPIKeyReservation(
		context.Background(), "rsv_"+strings.Repeat("0", 64), "missing",
	); err == nil {
		t.Fatal("missing reservation transition succeeded")
	}
}

func TestReserveAPIKeyExpiresUnknownReservationConservatively(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour).Add(10 * time.Minute)
	request := apiKeyReservationRequest(
		now,
		"request-1",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 2, Tokens: 100, CostMicro: 500},
	)
	request.TTL = time.Minute
	reservation, err := store.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkAPIKeyReservationUnknown(context.Background(), reservation.ID, "unknown"); err != nil {
		t.Fatal(err)
	}

	request.LogicalRequestID = "request-2"
	request.Now = now.Add(2 * time.Minute)
	_, err = store.ReserveAPIKey(context.Background(), request)
	if _, ok := errors.AsType[*coreusage.APIKeyLimitExceededError](err); !ok {
		t.Fatalf("post-expiry ReserveAPIKey error = %v, want limit error", err)
	}
	if state := readAPIKeyReservationState(t, store, reservation.ID); state != "expired_committed" {
		t.Fatalf("expired state = %q", state)
	}
	reservedRequests, reservedTokens, reservedCost, actualRequests, actualTokens, actualCost :=
		readAPIKeyReservationWindow(t, store, reservation.ID)
	if actualRequests != reservedRequests || actualTokens != reservedTokens || actualCost != reservedCost {
		t.Fatalf(
			"expired actual = %d/%d/%d, want reserved %d/%d/%d",
			actualRequests,
			actualTokens,
			actualCost,
			reservedRequests,
			reservedTokens,
			reservedCost,
		)
	}
}

func persistAPIKeyReservationUsage(t *testing.T, store *Store, detail FlatDetail) bool {
	t.Helper()
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin usage transaction: %v", err)
	}
	added, err := persistEvent(context.Background(), tx, store, &detail, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("persist usage event: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit usage event: %v", err)
	}
	return added
}

func TestUsageEventSettlesAPIKeyReservationIdempotently(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	reservation, err := store.ReserveAPIKey(
		context.Background(),
		apiKeyReservationRequest(
			now,
			"request-1",
			coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	first := FlatDetail{
		Timestamp:     now,
		Model:         "canonical-model",
		APIKey:        "client-key",
		ReservationID: reservation.ID,
		Tokens:        tokenStats{TotalTokens: 40},
		CostStatus:    costStatusComplete,
		costMicro:     300,
		costTrusted:   true,
	}
	if !persistAPIKeyReservationUsage(t, store, first) {
		t.Fatal("first usage event was not added")
	}
	if state := readAPIKeyReservationState(t, store, reservation.ID); state != "settled" {
		t.Fatalf("state = %q, want settled", state)
	}
	_, _, _, actualRequests, actualTokens, actualCost := readAPIKeyReservationWindow(t, store, reservation.ID)
	if actualRequests != 1 || actualTokens != 40 || actualCost != 300 {
		t.Fatalf("first settlement = %d/%d/%d", actualRequests, actualTokens, actualCost)
	}
	if persistAPIKeyReservationUsage(t, store, first) {
		t.Fatal("duplicate usage event was added")
	}
	_, _, _, actualRequests, actualTokens, actualCost = readAPIKeyReservationWindow(t, store, reservation.ID)
	if actualRequests != 1 || actualTokens != 40 || actualCost != 300 {
		t.Fatalf("duplicate settlement changed totals to %d/%d/%d", actualRequests, actualTokens, actualCost)
	}
	second := first
	second.Timestamp = now.Add(time.Second)
	second.Tokens = tokenStats{TotalTokens: 10}
	second.costMicro = 100
	if !persistAPIKeyReservationUsage(t, store, second) {
		t.Fatal("second usage event was not added")
	}
	_, _, _, actualRequests, actualTokens, actualCost = readAPIKeyReservationWindow(t, store, reservation.ID)
	if actualRequests != 1 || actualTokens != 50 || actualCost != 400 {
		t.Fatalf("aggregate settlement = %d/%d/%d", actualRequests, actualTokens, actualCost)
	}
}

func TestPartialCostSettlementKeepsReservedCost(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	reservation, err := store.ReserveAPIKey(
		context.Background(),
		apiKeyReservationRequest(
			now,
			"request-1",
			coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	persistAPIKeyReservationUsage(
		t, store, FlatDetail{
			Timestamp:     now,
			Model:         "canonical-model",
			ReservationID: reservation.ID,
			Tokens:        tokenStats{TotalTokens: 25},
			CostStatus:    costStatusPartial,
			CostReason:    "missing cache rate",
			costMicro:     200,
			costTrusted:   true,
		},
	)
	_, _, reservedCost, actualRequests, actualTokens, actualCost :=
		readAPIKeyReservationWindow(t, store, reservation.ID)
	if actualRequests != 1 || actualTokens != 25 || actualCost != reservedCost || reservedCost != 1_000 {
		t.Fatalf(
			"partial settlement = %d/%d/%d reserved cost %d", actualRequests, actualTokens, actualCost, reservedCost,
		)
	}
}

func TestReserveAPIKeyRejectsTerminalStateReplay(t *testing.T) {
	tests := []struct {
		name      string
		wantState string
	}{
		{name: "released", wantState: "released"},
		{name: "settled", wantState: "settled"},
		{name: "committed", wantState: "committed"},
		{name: "expired", wantState: "expired_committed"},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				store := openTestStore(t)
				now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
				request := apiKeyReservationRequest(
					now,
					"terminal-replay",
					coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
				)
				reservation, err := store.ReserveAPIKey(context.Background(), request)
				if err != nil {
					t.Fatalf("ReserveAPIKey: %v", err)
				}
				switch test.name {
				case "released":
					err = store.ReleaseAPIKeyReservation(context.Background(), reservation.ID, "released")
				case "settled":
					persistAPIKeyReservationUsage(
						t, store, FlatDetail{
							Timestamp:     now,
							Model:         "canonical-model",
							ReservationID: reservation.ID,
							Tokens:        tokenStats{TotalTokens: 1},
							CostStatus:    costStatusComplete,
							costMicro:     1,
							costTrusted:   true,
						},
					)
				case "committed":
					err = store.CommitAPIKeyReservation(context.Background(), reservation.ID, "committed")
				case "expired":
					request.TTL = time.Minute
					if _, err = store.db.Exec(
						"UPDATE api_key_reservation SET expires_at_ns = ? WHERE id = ?",
						now.Add(time.Minute).UnixNano(),
						reservation.ID,
					); err != nil {
						t.Fatalf("shorten reservation lease: %v", err)
					}
					trigger := request
					trigger.LogicalRequestID = "expiry-trigger"
					trigger.Now = now.Add(2 * time.Minute)
					trigger.Limits = []coreusage.APIKeyPolicyLimit{{Window: time.Hour, Requests: 10}}
					if _, err = store.ReserveAPIKey(context.Background(), trigger); err != nil {
						t.Fatalf("trigger reservation expiry: %v", err)
					}
				}
				if err != nil {
					t.Fatalf("prepare %s state: %v", test.name, err)
				}
				if state := readAPIKeyReservationState(t, store, reservation.ID); state != test.wantState {
					t.Fatalf("state = %q, want %q", state, test.wantState)
				}

				_, err = store.ReserveAPIKey(context.Background(), request)
				var replayErr *coreusage.APIKeyReservationReplayError
				if !errors.As(
					err, &replayErr,
				) || replayErr.ReservationID != reservation.ID || replayErr.State != test.wantState {
					t.Fatalf("terminal replay error = %#v, want state %q", err, test.wantState)
				}
			},
		)
	}
}

func TestAPIKeyReservationCommitAndUsageSettlementAreOrderIndependent(t *testing.T) {
	for _, commitFirst := range []bool{true, false} {
		name := "usage_then_commit"
		if commitFirst {
			name = "commit_then_usage"
		}
		t.Run(
			name, func(t *testing.T) {
				store := openTestStore(t)
				now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
				reservation, err := store.ReserveAPIKey(
					context.Background(),
					apiKeyReservationRequest(
						now,
						name,
						coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
					),
				)
				if err != nil {
					t.Fatal(err)
				}
				commit := func() {
					if errCommit := store.CommitAPIKeyReservation(
						context.Background(), reservation.ID, "processed",
					); errCommit != nil {
						t.Fatalf("commit reservation: %v", errCommit)
					}
				}
				settle := func() {
					persistAPIKeyReservationUsage(
						t, store, FlatDetail{
							Timestamp:     now,
							Model:         "canonical-model",
							ReservationID: reservation.ID,
							Tokens:        tokenStats{TotalTokens: 150},
							CostStatus:    costStatusComplete,
							costMicro:     1_200,
							costTrusted:   true,
						},
					)
				}
				if commitFirst {
					commit()
					settle()
				} else {
					settle()
					commit()
				}
				if state := readAPIKeyReservationState(t, store, reservation.ID); state != "committed" {
					t.Fatalf("state = %q, want committed", state)
				}
				_, _, _, actualRequests, actualTokens, actualCost :=
					readAPIKeyReservationWindow(t, store, reservation.ID)
				if actualRequests != 1 || actualTokens != 150 || actualCost != 1_200 {
					t.Fatalf("actual usage = %d/%d/%d", actualRequests, actualTokens, actualCost)
				}
			},
		)
	}
}

func TestExpiredReservationSettlementIsIndependentOfPriorUnknownState(t *testing.T) {
	for _, markUnknown := range []bool{false, true} {
		name := "open"
		if markUnknown {
			name = "unknown"
		}
		t.Run(
			name, func(t *testing.T) {
				store := openTestStore(t)
				now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
				request := apiKeyReservationRequest(
					now,
					"expired-"+name,
					coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
				)
				request.TTL = time.Minute
				reservation, err := store.ReserveAPIKey(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				if markUnknown {
					if err = store.MarkAPIKeyReservationUnknown(
						context.Background(), reservation.ID, "unknown",
					); err != nil {
						t.Fatalf("mark unknown: %v", err)
					}
				}
				trigger := request
				trigger.LogicalRequestID = "expired-trigger-" + name
				trigger.Now = now.Add(2 * time.Minute)
				trigger.Limits = []coreusage.APIKeyPolicyLimit{{Window: time.Hour, Requests: 10}}
				if _, err = store.ReserveAPIKey(context.Background(), trigger); err != nil {
					t.Fatalf("trigger expiry: %v", err)
				}
				persistAPIKeyReservationUsage(
					t, store, FlatDetail{
						Timestamp:     now.Add(2 * time.Minute),
						Model:         "canonical-model",
						ReservationID: reservation.ID,
						Tokens:        tokenStats{TotalTokens: 40},
						CostStatus:    costStatusComplete,
						costMicro:     300,
						costTrusted:   true,
					},
				)
				if state := readAPIKeyReservationState(t, store, reservation.ID); state != "expired_committed" {
					t.Fatalf("state = %q, want expired_committed", state)
				}
				reservedRequests, reservedTokens, reservedCost, actualRequests, actualTokens, actualCost :=
					readAPIKeyReservationWindow(t, store, reservation.ID)
				if actualRequests != reservedRequests || actualTokens != reservedTokens || actualCost != reservedCost {
					t.Fatalf(
						"expired usage = %d/%d/%d, reserved = %d/%d/%d",
						actualRequests,
						actualTokens,
						actualCost,
						reservedRequests,
						reservedTokens,
						reservedCost,
					)
				}
			},
		)
	}
}

func TestAPIKeyReservationClockSurvivesRollbackAndReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	store, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	watermark := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	request := apiKeyReservationRequest(
		watermark,
		"clock-first",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10},
	)
	if _, err = store.ReserveAPIKey(context.Background(), request); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	request.LogicalRequestID = "clock-after-reopen"
	request.Now = watermark.Add(-2 * time.Hour)
	reservation, err := reopened.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatalf("reservation after rollback: %v", err)
	}
	if !reservation.ExpiresAt.Equal(watermark.Add(time.Hour)) {
		t.Fatalf("expiresAt = %s, want %s", reservation.ExpiresAt, watermark.Add(time.Hour))
	}
	var createdNs int64
	if err = reopened.db.QueryRow(
		"SELECT created_at_ns FROM api_key_reservation WHERE id = ?",
		reservation.ID,
	).Scan(&createdNs); err != nil {
		t.Fatalf("query created time: %v", err)
	}
	if createdNs != watermark.UnixNano() {
		t.Fatalf("created time = %d, want %d", createdNs, watermark.UnixNano())
	}
	var persistedClock string
	if err = reopened.db.QueryRow(
		"SELECT value FROM meta WHERE key = ?",
		apiKeyReservationClockKey,
	).Scan(&persistedClock); err != nil {
		t.Fatalf("query reservation clock: %v", err)
	}
	if persistedClock != strconv.FormatInt(watermark.UnixNano(), 10) {
		t.Fatalf("persisted clock = %q", persistedClock)
	}
}

func TestIncompleteCostSettlementPreservesKnownOverage(t *testing.T) {
	for _, status := range []CostStatus{costStatusPartial, CostStatusUnpriced} {
		t.Run(
			string(status), func(t *testing.T) {
				store := openTestStore(t)
				now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
				reservation, err := store.ReserveAPIKey(
					context.Background(),
					apiKeyReservationRequest(
						now,
						"overage-"+string(status),
						coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
					),
				)
				if err != nil {
					t.Fatal(err)
				}
				persistAPIKeyReservationUsage(
					t, store, FlatDetail{
						Timestamp:     now,
						Model:         "canonical-model",
						ReservationID: reservation.ID,
						Tokens:        tokenStats{TotalTokens: 150},
						CostStatus:    status,
						CostReason:    "incomplete pricing",
						costMicro:     1_200,
						costTrusted:   true,
					},
				)
				_, _, _, actualRequests, actualTokens, actualCost :=
					readAPIKeyReservationWindow(t, store, reservation.ID)
				if actualRequests != 1 || actualTokens != 150 || actualCost != 1_200 {
					t.Fatalf("incomplete overage = %d/%d/%d", actualRequests, actualTokens, actualCost)
				}
			},
		)
	}
}

func TestAPIKeyReservationUsageEvidenceWinsTransitionOrdering(t *testing.T) {
	tests := []struct {
		name  string
		steps []string
	}{
		{name: "unknown_then_usage", steps: []string{"unknown", "usage"}},
		{name: "usage_then_unknown", steps: []string{"usage", "unknown"}},
		{name: "release_then_usage", steps: []string{"release", "usage"}},
		{name: "usage_then_release", steps: []string{"usage", "release"}},
		{name: "unknown_release_usage", steps: []string{"unknown", "release", "usage"}},
		{name: "release_unknown_usage", steps: []string{"release", "unknown", "usage"}},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				store := openTestStore(t)
				now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
				reservation, err := store.ReserveAPIKey(
					context.Background(),
					apiKeyReservationRequest(
						now,
						test.name,
						coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
					),
				)
				if err != nil {
					t.Fatal(err)
				}
				for _, step := range test.steps {
					switch step {
					case "unknown":
						err = store.MarkAPIKeyReservationUnknown(context.Background(), reservation.ID, step)
					case "release":
						err = store.ReleaseAPIKeyReservation(context.Background(), reservation.ID, step)
					case "usage":
						persistAPIKeyReservationUsage(
							t, store, FlatDetail{
								Timestamp:     now,
								Model:         "canonical-model",
								ReservationID: reservation.ID,
								CostStatus:    costStatusComplete,
								costTrusted:   true,
							},
						)
					}
					if err != nil {
						t.Fatalf("%s transition: %v", step, err)
					}
				}
				if state := readAPIKeyReservationState(t, store, reservation.ID); state != "settled" {
					t.Fatalf("state = %q, want settled", state)
				}
				_, _, _, actualRequests, actualTokens, actualCost :=
					readAPIKeyReservationWindow(t, store, reservation.ID)
				if actualRequests != 1 || actualTokens != 0 || actualCost != 0 {
					t.Fatalf("actual usage = %d/%d/%d", actualRequests, actualTokens, actualCost)
				}
			},
		)
	}
}

func TestAPIKeyReservationTablesEnforceIntegerStorage(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	request := apiKeyReservationRequest(
		now,
		"integer-storage",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 10, Tokens: 100, CostMicro: 1_000},
	)
	reservation, err := store.ReserveAPIKey(context.Background(), request)
	if err != nil {
		t.Fatalf("ReserveAPIKey: %v", err)
	}
	fingerprint := make([]byte, eventFingerprintBytes)
	fingerprint[len(fingerprint)-1] = 1
	if _, err = store.db.Exec(
		`INSERT INTO api_key_reservation_event(
			reservation_id, event_fingerprint, total_tokens, cost_micro, cost_status, created_at_ns
		) VALUES (?, ?, 1, 1, 'complete', ?)`,
		reservation.ID,
		fingerprint,
		now.UnixNano(),
	); err != nil {
		t.Fatalf("seed reservation event: %v", err)
	}

	for _, table := range []string{
		"api_key_reservation",
		"api_key_reservation_window",
		"api_key_reservation_event",
	} {
		for _, operation := range []string{"insert", "update"} {
			name := "usage_" + table + "_integer_" + operation
			var count int
			if err = store.db.QueryRow(
				"SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?",
				name,
			).Scan(&count); err != nil {
				t.Fatalf("query trigger %s: %v", name, err)
			}
			if count != 1 {
				t.Fatalf("trigger %s count = %d, want 1", name, count)
			}
		}
	}

	insertFingerprint := make([]byte, eventFingerprintBytes)
	insertFingerprint[len(insertFingerprint)-1] = 2
	insertReservationID := apiKeyReservationID(request.KeyIdentity, "integer-insert", request.PolicyProfile)
	tests := []struct {
		name  string
		table string
		query string
		args  []any
	}{
		{
			name:  "reservation insert",
			table: "api_key_reservation",
			query: `INSERT INTO api_key_reservation(
				id, key_identity, logical_request_id, policy_profile,
				requested_model, canonical_model, providers_json, state,
				created_at_ns, expires_at_ns, updated_at_ns
			) VALUES (?, ?, 'integer-insert', 'default', 'model', 'model', '[]', 'open', ?, 2, 2)`,
			args: []any{insertReservationID, request.KeyIdentity, 1.5},
		},
		{
			name:  "reservation update",
			table: "api_key_reservation",
			query: "UPDATE api_key_reservation SET created_at_ns = ? WHERE id = ?",
			args:  []any{1.5, reservation.ID},
		},
		{
			name:  "window insert",
			table: "api_key_reservation_window",
			query: `INSERT INTO api_key_reservation_window(
				reservation_id, window_ns, window_start_ns, window_end_ns,
				max_requests, reserved_requests
			) VALUES (?, ?, 0, 2, 1, 1)`,
			args: []any{reservation.ID, 1.5},
		},
		{
			name:  "window update",
			table: "api_key_reservation_window",
			query: "UPDATE api_key_reservation_window SET max_requests = ? WHERE reservation_id = ?",
			args:  []any{10.5, reservation.ID},
		},
		{
			name:  "event insert",
			table: "api_key_reservation_event",
			query: `INSERT INTO api_key_reservation_event(
				reservation_id, event_fingerprint, total_tokens, cost_micro, cost_status, created_at_ns
			) VALUES (?, ?, ?, 1, 'complete', ?)`,
			args: []any{reservation.ID, insertFingerprint, 1.5, now.UnixNano()},
		},
		{
			name:  "event update",
			table: "api_key_reservation_event",
			query: "UPDATE api_key_reservation_event SET total_tokens = ? WHERE reservation_id = ?",
			args:  []any{1.5, reservation.ID},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				_, errExec := store.db.Exec(test.query, test.args...)
				if errExec == nil || !strings.Contains(
					errExec.Error(),
					test.table+" requires integer storage classes",
				) {
					t.Fatalf("REAL write error = %v, want %s integer storage rejection", errExec, test.table)
				}
			},
		)
	}
}

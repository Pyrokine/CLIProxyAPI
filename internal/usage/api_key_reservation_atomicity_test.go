package usage

import (
	"testing"
	"time"

	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestAPIKeyReservationSQLiteFailureRollsBackWholeReserve(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.db.Exec(
		`CREATE TRIGGER synthetic_fail_reserve BEFORE INSERT ON api_key_reserva` +
			`tion_window BEGIN SELECT RAISE(ABORT, 'synthetic reservation failure')` +
			`; END`,
	); err != nil {
		t.Fatal(err)
	}
	request := apiKeyReservationRequest(
		time.Now().UTC(), "synthetic-reserve-failure", coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1},
	)
	if _, err := store.ReserveAPIKey(t.Context(), request); err == nil {
		t.Fatal("reservation failure returned success")
	}
	for _, table := range []string{"api_key_reservation", "api_key_reservation_window"} {
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial reserve survived in %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := store.db.Exec("DROP TRIGGER synthetic_fail_reserve"); err != nil {
		t.Fatal(err)
	}
	if reservation, err := store.ReserveAPIKey(t.Context(), request); err != nil || reservation.Reused {
		t.Fatalf("rolled-back identity cannot be reserved: %+v err=%v", reservation, err)
	}
}

func TestAPIKeyReservationSettlementFailurePreservesUnknownAndUsageAtomicity(t *testing.T) {
	store := openTestStore(t)
	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	request := apiKeyReservationRequest(
		now, "synthetic-settlement-failure",
		coreusage.APIKeyPolicyLimit{Window: time.Hour, Requests: 1, Tokens: 100, CostMicro: 1000},
	)
	reservation, err := store.ReserveAPIKey(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAPIKeyReservationUnknown(t.Context(), reservation.ID, "synthetic unknown outcome"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`CREATE TRIGGER synthetic_fail_settlement BEFORE INSERT ON ` +
			`api_key_reservation_event BEGIN SELECT RAISE(ABORT, 'synthetic settlement failure'); END`,
	); err != nil {
		t.Fatal(err)
	}
	detail := FlatDetail{
		Timestamp: now, Model: request.CanonicalModel, APIKey: "client-key", ReservationID: reservation.ID,
		Tokens: tokenStats{TotalTokens: 40}, CostStatus: costStatusComplete, costMicro: 300, costTrusted: true,
	}
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistEvent(t.Context(), tx, store, &detail, nil); err == nil {
		_ = tx.Rollback()
		t.Fatal("settlement failure returned success")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"events", "api_key_reservation_event", "hour_bucket"} {
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial usage survived in %s: count=%d err=%v", table, count, err)
		}
	}
	if state := readAPIKeyReservationState(t, store, reservation.ID); state != "unknown" {
		t.Fatalf("failed settlement released unknown outcome: %s", state)
	}
	_, _, _, requests, tokens, cost := readAPIKeyReservationWindow(t, store, reservation.ID)
	if requests != 0 || tokens != 0 || cost != 0 {
		t.Fatalf("failed settlement charged partial usage: %d/%d/%d", requests, tokens, cost)
	}
	request.LogicalRequestID = "synthetic-another-request"
	if _, err := store.ReserveAPIKey(t.Context(), request); err == nil {
		t.Fatal("unknown reservation stopped enforcing hard limit after SQLite failure")
	}
}

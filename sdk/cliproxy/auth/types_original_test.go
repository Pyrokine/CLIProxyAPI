package auth

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func makeTestJWT(expUnix int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(
			`{"exp":%d,"email":"test@example.com"}`, expUnix,
		)),
	)
	return header + "." + payload + ".sig"
}

func TestRecentRequestsSnapshotEmptyReturnsTwentyBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).In(time.Local)
	a := &Auth{}

	got := a.RecentRequestsSnapshot(now)
	if len(got) != recentRequestBucketCount {
		t.Fatalf("len = %d, want %d", len(got), recentRequestBucketCount)
	}

	currentBucketID := now.Unix() / recentRequestBucketSeconds
	baseBucketID := currentBucketID - int64(recentRequestBucketCount-1)
	for i, bucket := range got {
		if bucket.Success != 0 || bucket.Failed != 0 {
			t.Fatalf("bucket[%d] counts = %d/%d, want 0/0", i, bucket.Success, bucket.Failed)
		}
		if strings.TrimSpace(bucket.Time) == "" {
			t.Fatalf("bucket[%d] time label is empty", i)
		}
		expectedBucketID := baseBucketID + int64(i)
		start := time.Unix(expectedBucketID*recentRequestBucketSeconds, 0).In(time.Local)
		end := start.Add(10 * time.Minute)
		expected := start.Format("15:04") + "-" + end.Format("15:04")
		if bucket.Time != expected {
			t.Fatalf("bucket[%d] time = %q, want %q", i, bucket.Time, expected)
		}
	}
}

func TestRecentRequestsSnapshotIncludesCounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).In(time.Local)
	a := &Auth{}

	a.recordRecentRequest(now, true)
	a.recordRecentRequest(now, false)

	got := a.RecentRequestsSnapshot(now)
	if len(got) != recentRequestBucketCount {
		t.Fatalf("len = %d, want %d", len(got), recentRequestBucketCount)
	}

	newest := got[len(got)-1]
	if newest.Success != 1 || newest.Failed != 1 {
		t.Fatalf("newest bucket = success=%d failed=%d, want 1/1", newest.Success, newest.Failed)
	}
}

func TestRecentRequestsSnapshotBucketAdvanceMovesCounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).In(time.Local)
	next := now.Add(10 * time.Minute)
	a := &Auth{}

	a.recordRecentRequest(now, true)
	a.recordRecentRequest(next, false)

	got := a.RecentRequestsSnapshot(next)
	if len(got) != recentRequestBucketCount {
		t.Fatalf("len = %d, want %d", len(got), recentRequestBucketCount)
	}

	secondNewest := got[len(got)-2]
	newest := got[len(got)-1]
	if secondNewest.Success != 1 || secondNewest.Failed != 0 {
		t.Fatalf("second newest bucket = success=%d failed=%d, want 1/0", secondNewest.Success, secondNewest.Failed)
	}
	if newest.Success != 0 || newest.Failed != 1 {
		t.Fatalf("newest bucket = success=%d failed=%d, want 0/1", newest.Success, newest.Failed)
	}
}

func TestAuth_ExpirationTime_JWTExp(t *testing.T) {
	futureTime := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	pastTime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)

	futureJWT := makeTestJWT(futureTime.Unix())
	pastJWT := makeTestJWT(pastTime.Unix())

	// 1. When expired is missing, ExpirationTime should extract exp from access_token JWT
	authOnlyJWT := &Auth{
		Metadata: map[string]any{
			"access_token": futureJWT,
		},
	}
	exp, ok := authOnlyJWT.ExpirationTime()
	if !ok {
		t.Fatal("ExpirationTime() should return true when access_token has valid JWT exp")
	}
	if exp.Unix() != futureTime.Unix() {
		t.Fatalf("ExpirationTime() = %v (unix %d), want %v (unix %d)", exp, exp.Unix(), futureTime, futureTime.Unix())
	}

	// 2. When top-level expired is in the past, but access_token has future JWT exp, prioritize the future JWT exp
	authStaleExpired := &Auth{
		Metadata: map[string]any{
			"expired":      pastTime.Format(time.RFC3339),
			"access_token": futureJWT,
		},
	}
	expStale, okStale := authStaleExpired.ExpirationTime()
	if !okStale {
		t.Fatal("ExpirationTime() should return true for authStaleExpired")
	}
	if expStale.Unix() != futureTime.Unix() {
		t.Fatalf("ExpirationTime() with stale expired metadata = %v, want JWT future exp %v", expStale, futureTime)
	}

	// 3. When both expired metadata and JWT are in the past, return the expired time
	authPastJWT := &Auth{
		Metadata: map[string]any{
			"expired":      pastTime.Format(time.RFC3339),
			"access_token": pastJWT,
		},
	}
	expPast, okPast := authPastJWT.ExpirationTime()
	if !okPast {
		t.Fatal("ExpirationTime() should return true for authPastJWT")
	}
	if !expPast.Before(time.Now()) {
		t.Fatalf("ExpirationTime() = %v, want past time", expPast)
	}

	// 4. When access_token JWT is expired but id_token JWT is in the future, access_token validity is false
	authExpiredAccessFutureID := &Auth{
		Metadata: map[string]any{
			"access_token": pastJWT,
			"id_token":     futureJWT,
		},
	}
	if authExpiredAccessFutureID.HasValidAccessToken(time.Now()) {
		t.Fatal("HasValidAccessToken() should return false when access_token JWT is expired even if id_token is future")
	}
	if exp, ok := authExpiredAccessFutureID.AccessTokenExpirationTime(); !ok || !exp.Before(time.Now()) {
		t.Fatalf("AccessTokenExpirationTime() = (%v, %t), want past time and true", exp, ok)
	}

	// 5. When access_token is empty, HasValidAccessToken is false
	authNoAccess := &Auth{
		Metadata: map[string]any{
			"id_token": futureJWT,
			"expired":  futureTime.Format(time.RFC3339),
		},
	}
	if authNoAccess.HasValidAccessToken(time.Now()) {
		t.Fatal("HasValidAccessToken() should return false when access_token is missing")
	}
}

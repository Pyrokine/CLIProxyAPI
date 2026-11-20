package kimi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAuditRefreshTokenRejectsAlreadyCancelledCaller(t *testing.T) {
	for _, mode := range []string{"cancelled", "expired", "active"} {
		t.Run(
			mode, func(t *testing.T) {
				calls := 0
				client := &DeviceFlowClient{
					httpClient: &http.Client{
						Transport: kimiRoundTripFunc(
							func(request *http.Request) (*http.Response, error) {
								calls++
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header),
									Body: io.NopCloser(
										strings.NewReader(
											`{"access_token":"synthetic-access","refresh_token":` +
												`"synthetic-next","expires_in":` +
												`3600}`,
										),
									),
									Request: request,
								}, nil
							},
						),
					},
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var expected error
				switch mode {
				case "cancelled":
					cancel()
					expected = context.Canceled
				case "expired":
					var stop context.CancelFunc
					ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer stop()
					expected = context.DeadlineExceeded
				}
				token, err := client.RefreshToken(ctx, t.Name()+"-synthetic-refresh")
				if expected != nil {
					if !errors.Is(err, expected) || token != nil || calls != 0 {
						t.Fatalf("token=%v error=%v calls=%d", token, err, calls)
					}
				} else if err != nil || token == nil || token.AccessToken != "synthetic-access" || calls != 1 {
					t.Fatalf("token=%v error=%v calls=%d", token, err, calls)
				}
			},
		)
	}
}

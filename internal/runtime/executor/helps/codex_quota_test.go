package helps

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestParseCodexQuotaEventHeadersOriginalInputs(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	for _, fixture := range []struct {
		name, payload string
		want          map[string]string
		absent        []string
	}{
		{
			name: "PreservesActiveLimit",
			payload: `{"type":"codex.rate_limits","plan_type":"pro","metered_limit_name":"codex_bengalfox","` +
				`rate_limits":{"primary":{"used_percent":2,"window_minutes":10080,"reset_at":1782951970},"` +
				`secondary":null}}`,
			want: map[string]string{
				"Active-Limit": "codex_bengalfox", "Primary-Used-Percent": "2", "Primary-Window-Minutes": "10080",
				"Primary-Reset-At": "1782951970", "Plan-Type": "pro",
			},
		},
		{
			name: "PreservesAdditionalLimitsAndCredits",
			payload: (`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":` +
				`{"allowed":true,"limit_reached":false,`) +
				`"primary":{"used_percent":48,"window_minutes":10080,"reset_after_seconds":523210,` +
				`"reset_at":1786677299},"secondary":null},"additional_rate_limits":{"GPT-5.3-Codex-Spark":{` +
				`"allowed":true,"limit_reached":false,"primary":{"used_percent":3,"window_minutes":300,` +
				`"reset_after_seconds":10148,"reset_at":1787231961},"secondary":{"used_percent":63,` +
				`"window_minutes":10080,"reset_after_seconds":75420,"reset_at":1787290791}}},` +
				`"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`,
			want: map[string]string{
				"Primary-Used-Percent": "48", "Primary-Window-Minutes": "10080",
				"Primary-Reset-After-Seconds":                           "523210",
				"Additional-GPT-5.3-Codex-Spark-Limit-Name":             "GPT-5.3-Codex-Spark",
				"Additional-GPT-5.3-Codex-Spark-Primary-Used-Percent":   "3",
				"Additional-GPT-5.3-Codex-Spark-Primary-Window-Minutes": "300",
				"Additional-GPT-5.3-Codex-Spark-Secondary-Used-Percent": "63",
				"Credits-Has-Credits":                                   "false",
				"Credits-Unlimited":                                     "false",
				"Credits-Balance":                                       "0",
			},
		},
		{
			name: "ReadsQuotaHeadersFromErrorFrames",
			payload: `{"type":"error","status_code":429,"headers":{"X-Codex-Primary-Used-Percent":"100",` +
				`"X-Codex-Primary-Window-Minutes":"10080","X-Codex-Primary-Reset-After-Seconds":"437380",` +
				`"X-Codex-Credits-Balance":"0","X-Codex-Turn-State":"must-not-be-observed",` +
				`"Set-Cookie":"must-not-be-observed"}}`,
			want: map[string]string{
				"Primary-Used-Percent": "100", "Primary-Window-Minutes": "10080",
				"Primary-Reset-After-Seconds": "437380", "Credits-Balance": "0",
			},
			absent: []string{"X-Codex-Turn-State", "Set-Cookie"},
		},
		{
			name: "DoesNotInventActiveLimitAndRejectsIncompleteWindows/valid",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,` +
				`"window_minutes":300,"reset_after_seconds":60}}}`,
			want: map[string]string{"Primary-Reset-After-Seconds": "60"}, absent: []string{"X-Codex-Active-Limit"},
		},
		{
			name:    "DoesNotInventActiveLimitAndRejectsIncompleteWindows/incomplete",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42}}}`,
		},
		{
			name: "DoesNotInventActiveLimitAndRejectsIncompleteWindows/partial",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,"window_minutes":300},` +
				`"secondary":{"used_percent":17,"window_minutes":10080,"reset_after_seconds":60}}}`,
			want: map[string]string{"Secondary-Used-Percent": "17"}, absent: []string{"X-Codex-Primary-Used-Percent"},
		},
		{
			name: "SupportsAdditionalArrayAndCamelCase",
			payload: `{"type":"codex.rate_limits","planType":"pro","meteredLimitName":"premium",` +
				`"rateLimit":{"allowed":true,"limitReached":false,"primary":{"usedPercent":71,` +
				`"windowMinutes":10080,"resetAfterSeconds":60}},"additionalRateLimits":[{` +
				`"limitName":"GPT-5.3-Codex-Spark","rateLimit":{"primary":{"usedPercent":13,` +
				`"windowMinutes":300,"resetAt":1787399817},"secondary":{"usedPercent":51,` +
				`"windowMinutes":10080,"resetAfterSeconds":120}}}]}`,
			want: map[string]string{
				"Active-Limit": "premium", "Plan-Type": "pro", "Primary-Used-Percent": "71",
				"Additional-GPT-5.3-Codex-Spark-Limit-Name":                    "GPT-5.3-Codex-Spark",
				"Additional-GPT-5.3-Codex-Spark-Primary-Used-Percent":          "13",
				"Additional-GPT-5.3-Codex-Spark-Secondary-Reset-After-Seconds": "120",
			},
		},
		{
			name: "EmptyPreferredAliasesKeepNonemptyLegacyValues",
			payload: `{"type":"codex.rate_limits","plan_type":"","planType":"pro","metered_limit_name":" ",` +
				`"meteredLimitName":"premium","additional_rate_limits":[{"limit_name":"","limitName":"Spark",` +
				`"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":0}}]}`,
			want: map[string]string{
				"Plan-Type": "pro", "Active-Limit": "premium", "Additional-Spark-Limit-Name": "Spark",
				"Additional-Spark-Primary-Used-Percent": "0",
			},
		},
		{
			name: "KeepsOnlySafeErrorHeaders",
			payload: `{"type":"error","headers":{"X-Ratelimit-Remaining-Requests":"7","Retry-After":"60","X-` +
				`Codex-Primary-Over-Secondary-Limit-Percent":"0","X-Codex-Turn-State":"secret","` +
				`Authorization":"Bearer synthetic"}}`,
			want:   map[string]string{"Primary-Over-Secondary-Limit-Percent": "0"},
			absent: []string{"X-Codex-Turn-State", "Authorization"},
		},
		{
			name: "KeepsWindowsWhenActiveLimitIsInvalid",
			payload: `{"type":"codex.rate_limits","metered_limit_name":"bad limit","rate_limits":{` +
				`"primary":{"used_percent":1,"window_minutes":300,"reset_after_seconds":60}}}`,
			want: map[string]string{"Primary-Used-Percent": "1"}, absent: []string{"X-Codex-Active-Limit"},
		},
		{
			name: "ObservedProductionEvent",
			payload: (`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":` +
				`{"allowed":true,"limit_reached":false,`) +
				`"primary":{"used_percent":81,"window_minutes":10080,"reset_after_seconds":137160,` +
				`"reset_at":1787588999},"secondary":null},"code_review_rate_limits":{"primary":{` +
				`"used_percent":4,"window_minutes":300,"reset_after_seconds":900}},` +
				`"additional_rate_limits":{"GPT-5.3-Codex-Spark":{"allowed":true,"limit_reached":false,` +
				`"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":18000}}},` +
				`"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"promo":null}`,
			want: map[string]string{
				"Plan-Type": "pro", "Primary-Used-Percent": "81", "Primary-Reset-At": "1787588999",
				"Code-Review-Primary-Used-Percent":                    "4",
				"Additional-Gpt-5.3-Codex-Spark-Limit-Name":           "GPT-5.3-Codex-Spark",
				"Additional-Gpt-5.3-Codex-Spark-Primary-Used-Percent": "0", "Credits-Balance": "0",
				"Credits-Has-Credits": "false",
			},
			absent: []string{"X-Codex-Secondary-Used-Percent"},
		},
		{
			name: "RejectsControlCharactersInLimitName",
			payload: `{"type":"codex.rate_limits","additional_rate_limits":[{"limit_name":"evil\r\nX-Injected: 1",` +
				`"primary":{"used_percent":3,"window_minutes":300,"reset_after_seconds":60}}]}`,
			want:   map[string]string{"Additional-evil-X-Injected-1-Primary-Used-Percent": "3"},
			absent: []string{"X-Codex-Additional-evil-X-Injected-1-Limit-Name"},
		},
		{
			name: "LargeRateLimitsFrameIsParsed",
			payload: `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,"limit_percent":100,` +
				`"window_minutes":10080,"reset_after_seconds":3600,"used_minutes":120},"secondary":{` +
				`"used_percent":1,"limit_percent":100,"window_minutes":300,"reset_after_seconds":60,` +
				`"used_minutes":2},"active_limit":"primary","credits":{"total_credits":"10",` +
				`"available_credits":"5","pct_remaining":"50"}},"padding":"` + strings.Repeat("p", 4096) + `"}`,
			want: map[string]string{"Primary-Used-Percent": "42"},
		},
	} {
		t.Run(
			fixture.name, func(t *testing.T) {
				headers := parseCodexQuotaEventHeaders([]byte(fixture.payload))
				if len(fixture.want) == 0 && headers != nil {
					t.Fatalf("incomplete event produced headers: %#v", headers)
				}
				for suffix, want := range fixture.want {
					if got := headers.Get("X-Codex-" + suffix); got != want {
						t.Errorf("%s=%q, want %q; headers=%#v", suffix, got, want, headers)
					}
				}
				if fixture.name == "KeepsOnlySafeErrorHeaders" &&
					(headers.Get("X-Ratelimit-Remaining-Requests") != "7" || headers.Get("Retry-After") != "60") {
					t.Errorf("lost rate limit or retry header: %#v", headers)
				}
				if fixture.name == "RejectsControlCharactersInLimitName" {
					for name, values := range headers {
						for _, value := range values {
							if strings.ContainsAny(value, "\r\n") {
								t.Errorf("quota header %s carried control characters: %q", name, value)
							}
						}
					}
				}
				for _, name := range fixture.absent {
					if _, exists := headers[http.CanonicalHeaderKey(name)]; exists {
						t.Errorf("unexpected %s: %#v", name, headers)
					}
				}
			},
		)
	}
}

func TestParseCodexQuotaEventHeadersNonQuotaEventDoesNotAllocate(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"sequence_number":7,"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		frame := []byte(payload)
		if allocations := testing.AllocsPerRun(
			100, func() {
				if headers := parseCodexQuotaEventHeaders(frame); headers != nil {
					t.Fatal(headers)
				}
			},
		); allocations != 0 {
			t.Errorf("non-quota %d-byte frame allocated %v, want zero", len(frame), allocations)
		}
	}
}

func TestParseCodexQuotaEventHeadersRejectsInvalidAndEmptyEvents(t *testing.T) {
	for _, payload := range []string{
		``, `{"type":"response.completed"}`,
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":101,` +
			`"window_minutes":300,"reset_after_seconds":60}}}`,
		`{"type":"codex.rate_limits","credits":{"balance":"0"}} trailing`,
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":"NaN",` +
			`"window_minutes":300,"reset_after_seconds":60}}}`,
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":1,` +
			`"window_minutes":300,"reset_after_seconds":"invalid"}}}`,
		`{"type":"error","headers":{"Authorization":"synthetic","X-Codex-Plan-Type":{"nested":1},"Retry-After":null}}`,
		`{"type":"error","headers":{"X-Codex-Injected\r\n-Limit-Reached":"1","X-Codex-Bad:Name-Used-Percent":"2"}}`,
	} {
		if headers := parseCodexQuotaEventHeaders([]byte(payload)); headers != nil {
			t.Errorf("%s produced %#v", payload, headers)
		}
	}
}

func TestParseCodexQuotaEventHeadersUsesManagerBounds(t *testing.T) {
	var limits, headers []string
	for index := range 12 {
		limits = append(
			limits, fmt.Sprintf(
				`"limit-%02d":{"primary":{"used_percent":0,`+
					`"window_minutes":300,"reset_after_seconds":0}}`, index,
			),
		)
	}
	parsed := parseCodexQuotaEventHeaders(
		[]byte(`{"type":"codex.rate_limits","additional_rate_limits":{` +
			strings.Join(limits, ",") + `}}`),
	)
	if len(parsed) != 4*maxCodexAdditionalRateLimits ||
		parsed.Get("X-Codex-Additional-Limit-07-Primary-Used-Percent") != "0" ||
		parsed.Get("X-Codex-Additional-Limit-08-Primary-Used-Percent") != "" {
		t.Fatalf("additional limit bound lost: %#v", parsed)
	}
	for index := range 128 {
		headers = append(headers, fmt.Sprintf(`"X-Codex-Additional-L%03d-Primary-Used-Percent":"1"`, index))
	}
	headers = append(
		headers,
		`"X-Codex-Plan-Type":"pro"`, `"X-Codex-Primary-Used-Percent":"0"`, `"X-Codex-Credits-Balance":"0"`,
		`"X-Codex-Credits-Unlimited":"`+strings.Repeat("x", 513)+`"`, `"Retry-After":"bad\u0085value"`,
	)
	parsed = parseCodexQuotaEventHeaders(
		[]byte(`{"type":"error","headers":{` + strings.Join(headers, ",") + `}}`),
	)
	if len(parsed) != 64 || parsed.Get("X-Codex-Plan-Type") != "pro" ||
		parsed.Get("X-Codex-Primary-Used-Percent") != "0" || parsed.Get("X-Codex-Credits-Balance") != "0" ||
		parsed.Get("Retry-After") != "" || parsed.Get("X-Codex-Credits-Unlimited") != "" {
		t.Fatalf("header retention or value bounds lost: %#v", parsed)
	}
}

func TestParseCodexQuotaEventHeadersReadsTopLevelTypeBeyondPrefix(t *testing.T) {
	payload := []byte(`{"padding":"` + strings.Repeat("p", 4096) + `","type":"rate_limits.updated",` +
		`"rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":0}}}`)
	if headers := parseCodexQuotaEventHeaders(payload); headers.Get("X-Codex-Primary-Used-Percent") != "0" {
		t.Fatal(headers)
	}
	if headers := parseCodexQuotaEventHeaders(
		[]byte(`{"nested":{"type":"codex.rate_limits"},` +
			`"type":"response.completed","credits":{"balance":"0"}}`),
	); headers != nil {
		t.Fatal(headers)
	}
}

func TestGenericWebsocketEntryPointDoesNotObserveCodexQuota(t *testing.T) {
	frame := []byte(`{"type":"error","headers":{"x-ratelimit-remaining-requests":"0","retry-after":"60"}}`)
	for _, provider := range []string{"xai", "codex"} {
		t.Run(
			provider, func(t *testing.T) {
				ctx := internallogging.WithResponseHeadersHolder(t.Context())
				internallogging.SetResponseHeaders(ctx, http.Header{"X-Request-Id": {"xai-1"}})
				EmitWebSocketResponseEvent(ctx, core.Options{}, nil, provider, "quota-model", frame)
				headers := internallogging.GetResponseHeaders(ctx)
				if headers.Get("X-Request-Id") != "xai-1" {
					t.Fatal("quota observation changed original request ID")
				}
				if provider == "codex" {
					if headers.Get("X-Ratelimit-Remaining-Requests") != "0" ||
						headers.Get("Retry-After") != "60" {
						t.Fatalf("Codex quota event was lost: %#v", headers)
					}
				} else if headers.Get("X-Ratelimit-Remaining-Requests") != "" || headers.Get("Retry-After") != "" {
					t.Fatalf("other provider observed Codex quota: %#v", headers)
				}
			},
		)
	}
}

func TestMergeWebsocketQuotaHeaders(t *testing.T) {
	ctx := internallogging.WithResponseHeadersHolder(t.Context())
	internallogging.SetResponseHeaders(ctx, http.Header{"X-Request-Id": {"req-1"}})
	//goland:noinspection SpellCheckingInspection
	internallogging.MergeResponseHeaders(
		ctx, parseCodexQuotaEventHeaders(
			[]byte(("{\n\t\t\"type\":\"codex.rate_limits\",\n\t\t\"rate_limits\":{\"primary\":{\"" +
				"used_percent\":2,\"window_minutes\":10080,\"reset_at\":1782951970}},\n\t\t\"" +
				"metered_limit_name\":\"codex_bengalfox\"\n\t}")),
		),
	)
	headers := internallogging.GetResponseHeaders(ctx)
	//goland:noinspection SpellCheckingInspection
	if headers.Get("X-Request-Id") != "req-1" || headers.Get("X-Codex-Active-Limit") != "codex_bengalfox" {
		t.Fatalf("merged response headers=%#v", headers)
	}
}

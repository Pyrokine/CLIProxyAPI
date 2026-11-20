package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAuditExplicitAmpIdentityOverridesLiveLCPAffinity(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: affinityLastSelector{}, TTL: time.Hour,
		},
	)
	defer selector.Stop()
	a, b := &Auth{ID: "lcp-auth"}, &Auth{ID: "amp-auth"}
	request := func(header string) core.Options {
		headers := make(http.Header)
		if header != "" {
			headers.Set("X-Amp-Thread-Id", header)
		}
		return core.Options{
			Headers: headers,
			OriginalRequest: []byte(`{"messages":[{"role":"user","content":"shared prefix"},{"role":"assist` +
				`ant","content":"shared answer"},{"role":"user","content":"continue"}]}`),
			SourceFormat: translator.FormatOpenAI,
			Metadata:     map[string]any{core.CallerScopeMetadataKey: "amp-lcp-consumer"},
		}
	}
	seed := request("")
	if selected, err := selector.Pick(
		t.Context(), "openai", "model", seed, []*Auth{a},
	); err != nil || selected == nil || selected.ID != a.ID {
		t.Fatalf("LCP seed selection=%#v error=%v", selected, err)
	}
	probe := request("")
	if selected, err := selector.Pick(
		t.Context(), "openai", "model", probe, []*Auth{a, b},
	); err != nil || selected == nil || selected.ID != a.ID || sessionMetadataString(
		probe.Metadata, core.LCPAffinitySessionIDMetadataKey,
	) == "" {
		t.Fatalf("live LCP prefix selection=%#v error=%v metadata=%#v", selected, err, probe.Metadata)
	}
	explicit := request("fixture-thread")
	if selected, err := selector.Pick(
		t.Context(), "openai", "model", explicit, []*Auth{b},
	); err != nil || selected == nil || selected.ID != b.ID {
		t.Fatalf("Amp seed selection=%#v error=%v", selected, err)
	}
	final := request("fixture-thread")
	if selected, err := selector.Pick(
		t.Context(), "openai", "model", final, []*Auth{a, b},
	); err != nil || selected == nil || selected.ID != b.ID {
		t.Fatalf("explicit Amp selection=%#v error=%v metadata=%#v", selected, err, final.Metadata)
	}
	ctx := syncMetadataSessionToContext(util.WithSessionID(context.Background(), "amp:fixture-thread"), final.Metadata)
	metadata := logging.GetClientRequestMetadata(ctx)
	if metadata.SessionID != "amp:fixture-thread" || util.SessionIDFromContext(ctx) != metadata.SessionID ||
		metadata.IsFork ||
		metadata.IsCompaction {
		t.Fatalf("explicit execution metadata=%#v", metadata)
	}
}

func TestAuditNumericForkPublishesDistinctExecutionMetadata(t *testing.T) {
	for _, test := range []struct{ name, first, branch string }{
		{"integer", "9007199254740992", "9007199254740993"},
		{"decimal", "0.123456789012345678901", "0.123456789012345678902"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				selector := NewSessionAffinitySelectorWithConfig(
					SessionAffinityConfig{
						Fallback: affinityLastSelector{}, TTL: time.Hour,
					},
				)
				defer selector.Stop()
				options := func(number string) core.Options {
					payload := fmt.Sprintf(
						`{"messages":[{"role":"user","content":"calculate a value"},{"role":"assistant","`+
							`content":[{"type":"tool_use","id":"tool_1","name":"calculate","input":{"value":%`+
							`s}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","`+
							`content":"ok"}]}]}`,
						number,
					)
					return core.Options{
						OriginalRequest: []byte(payload), SourceFormat: translator.FormatClaude,
						Metadata: map[string]any{core.CallerScopeMetadataKey: "numeric-fork-consumer"},
					}
				}
				auths := []*Auth{{ID: "numeric-auth"}}
				first := options(test.first)
				if selected, err := selector.Pick(
					t.Context(), "claude", "model", first, auths,
				); err != nil || selected == nil {
					t.Fatalf("first selection=%#v error=%v", selected, err)
				}
				firstID := sessionMetadataString(first.Metadata, core.CanonicalSessionIDMetadataKey)
				branch := options(test.branch)
				if selected, err := selector.Pick(
					t.Context(), "claude", "model", branch, auths,
				); err != nil || selected == nil {
					t.Fatalf("branch selection=%#v error=%v", selected, err)
				}
				ctx := syncMetadataSessionToContext(context.Background(), branch.Metadata)
				metadata := logging.GetClientRequestMetadata(ctx)
				if firstID == "" || metadata.SessionID == "" || metadata.SessionID == firstID || !metadata.IsFork ||
					metadata.NodeKind != "fork" ||
					metadata.ParentSessionID == "" ||
					util.SessionIDFromContext(ctx) != metadata.SessionID {
					t.Fatalf(
						"numeric values collapsed: first=%q branch metadata=%#v options=%#v", firstID, metadata,
						branch.Metadata,
					)
				}
				continued := options(test.branch)
				if selected, err := selector.Pick(
					t.Context(), "claude", "model", continued, auths,
				); err != nil || selected == nil {
					t.Fatalf("continued selection=%#v error=%v", selected, err)
				}
				continuedMetadata := logging.GetClientRequestMetadata(
					syncMetadataSessionToContext(
						context.Background(), continued.Metadata,
					),
				)
				if continuedMetadata.SessionID != metadata.SessionID || continuedMetadata.IsFork {
					t.Fatalf("linear continuation became another fork: %#v", continuedMetadata)
				}
			},
		)
	}
}

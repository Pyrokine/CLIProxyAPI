package claude

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

type scopedSignatureKV struct {
	home.KVClient
	raw                     []byte
	key                     string
	reads, writes, expiries int
}

func (*scopedSignatureKV) Enabled() bool     { return true }
func (*scopedSignatureKV) HeartbeatOK() bool { return true }
func (c *scopedSignatureKV) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.reads++
	return append([]byte(nil), c.raw...), key == c.key && c.raw != nil, nil
}

func (c *scopedSignatureKV) KVSet(
	_ context.Context,
	key string,
	value []byte,
	opts home.KVSetOptions,
) (bool, error) {
	c.writes++
	c.key, c.raw = key, append([]byte(nil), value...)
	return opts.EX == 3*time.Hour, nil
}
func (c *scopedSignatureKV) KVExpire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	c.expiries++
	return key == c.key && ttl == 3*time.Hour, nil
}

func TestHomeSignatureResponseToCheckedRequestKeepsOriginalValue(t *testing.T) {
	const model = "claude-sonnet-4-5-thinking"
	const thinkingText = "My thinking process here"
	rawSignature := validClaudeThinkingSignatureForAntigravityTest()
	want, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
	if !compatible {
		t.Fatal("original signature fixture is incompatible")
	}
	for _, requestBody := range []string{`{"model":"` + model + `"}`, `{}`} {
		t.Run(
			requestBody, func(t *testing.T) {
				client := &scopedSignatureKV{}
				ctx := home.WithClientScope(
					t.Context(), home.ClientScope{Client: client, ID: "signature-service", Enabled: true},
				)
				var state any
				for _, chunk := range []string{
					(`{"response":{"candidates":[{"content":{"parts":[{"text":` +
						`"`) +
						thinkingText +
						(`","thought":` +
							`true}]}}]}}`),
					(`{"response":{"candidates":[{"content":{"parts":[{"text":"","thought":` +
						`true,"thoughtSignature":` +
						`"`) +
						rawSignature +
						`"}]}}]}}`,
				} {
					convertAntigravityResponseToClaude(
						ctx,
						model,
						nil,
						[]byte(requestBody),
						[]byte(chunk),
						&state,
					)
				}
				if client.writes != 1 || string(client.raw) != rawSignature ||
					client.key != "cpa:signature:claude:"+home.HashKeyPart(thinkingText) {
					t.Fatalf("writes=%d key=%q cached=%q", client.writes, client.key, client.raw)
				}
				request := []byte(
					(`{"messages":[{"role":"assistant","content":[{"type":"thinking",` +
						`"thinking":` +
						`"`) +
						thinkingText +
						`"},{"type":"text","text":"answer"}]},{"role":"user","content":"next"}]}`,
				)
				body, err := convertClaudeRequestToAntigravityChecked(ctx, model, request, false)
				if err != nil || client.reads != 1 || client.expiries != 1 {
					t.Fatalf("error=%v reads=%d expiries=%d body=%s", err, client.reads, client.expiries, body)
				}
				found := false
				for _, content := range gjson.GetBytes(body, "request.contents").Array() {
					for _, part := range content.Get("parts").Array() {
						if part.Get("thought").Bool() && part.Get("text").String() == thinkingText {
							found = part.Get("thoughtSignature").String() == want
						}
					}
				}
				if !found {
					t.Fatalf("cached signature did not reach final thinking part: %s", body)
				}
			},
		)
	}
}

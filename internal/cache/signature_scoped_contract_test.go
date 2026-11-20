package cache

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	log "github.com/sirupsen/logrus"
)

func TestSignatureRequiredHomeKeepsOriginalKeyAndSlidingTTL(t *testing.T) {
	client := newFakeCodexReasoningReplayKVClient()
	ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, ID: "signature-service", Enabled: true})
	const text = "secret-value"
	signature := strings.Repeat("signature", 8)
	if !CacheSignatureBestEffort(ctx, testModelName, text, signature) {
		t.Fatal("Home signature write failed")
	}
	key := "cpa:signature:claude:" + home.HashKeyPart(text)
	if string(client.values[key]) != signature || client.lastSetTTL != signatureCacheTTL {
		t.Fatalf("value=%q TTL=%s", client.values[key], client.lastSetTTL)
	}
	got, err := GetCachedSignatureRequired(ctx, testModelName, text)
	if err != nil || got != signature || client.expireCount != 1 || client.lastExpireTTL != signatureCacheTTL {
		t.Fatalf("signature=%q error=%v expiry count=%d TTL=%s", got, err, client.expireCount, client.lastExpireTTL)
	}
	if err := DeleteCachedSignatureRequired(ctx, testModelName, text); err != nil || client.delCount != 1 {
		t.Fatalf("delete error=%v calls=%d", err, client.delCount)
	}
	if got, err := GetCachedSignatureRequired(ctx, testModelName, text); got != "" || err != nil {
		t.Fatalf("deleted signature=%q error=%v", got, err)
	}
	if got, err := GetCachedSignatureRequired(
		ctx, "gemini-3-pro-preview", text,
	); got != "skip_thought_signature_validator" || err != nil {
		t.Fatalf("Gemini miss=%q error=%v", got, err)
	}
}

func TestSignatureRequiredHomeErrorsRemainVisibleAndLogsHideValues(t *testing.T) {
	var output bytes.Buffer
	previous := log.StandardLogger().Out
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	sentinel := errors.New("ERR home kv unavailable")
	const text = "cpa:test:secret-key"
	signature := strings.Repeat("secret-value", 8)
	for _, operation := range []string{"get", "expire", "delete", "set"} {
		t.Run(
			operation, func(t *testing.T) {
				client := newFakeCodexReasoningReplayKVClient()
				client.values[signatureKVKey(testModelName, text)] = []byte(signature)
				ctx := home.WithClientScope(t.Context(), home.ClientScope{Client: client, Enabled: true})
				var err error
				switch operation {
				case "get":
					client.getErr = sentinel
					_, err = GetCachedSignatureRequired(ctx, testModelName, text)
				case "expire":
					client.expireErr = sentinel
					_, err = GetCachedSignatureRequired(ctx, testModelName, text)
				case "delete":
					client.delErr = sentinel
					err = DeleteCachedSignatureRequired(ctx, testModelName, text)
				case "set":
					client.setErr = sentinel
					if CacheSignatureBestEffort(ctx, testModelName, text, signature) {
						t.Fatal("failed Home write reported success")
					}
					return
				}
				if !errors.Is(err, sentinel) {
					t.Fatalf("%s error=%v, want original error", operation, err)
				}
			},
		)
	}
	if got := output.String(); !strings.Contains(got, "prefix=cpa:signature:*") ||
		strings.Contains(got, text) || strings.Contains(got, "secret-value") {
		t.Fatalf("signature failure log exposed input or omitted prefix: %s", got)
	}
}

func TestSignatureHomeMissAndWriteFailureKeepLocalStateSeparate(t *testing.T) {
	ClearSignatureCache("")
	t.Cleanup(func() { ClearSignatureCache("") })
	const scopeID = "signature-no-fallback"
	local := home.WithClientScope(t.Context(), home.ClientScope{ID: scopeID})
	signature := strings.Repeat("local-signature", 5)
	if !CacheSignatureBestEffort(local, testModelName, "same-text", signature) {
		t.Fatal("local precondition failed")
	}
	client := newFakeCodexReasoningReplayKVClient()
	remote := home.WithClientScope(t.Context(), home.ClientScope{ID: scopeID, Client: client, Enabled: true})
	if got, err := GetCachedSignatureRequired(remote, testModelName, "same-text"); got != "" || err != nil {
		t.Fatalf("Home miss read local signature: %q, %v", got, err)
	}
	client.setErr = errors.New("ERR home kv unavailable")
	if CacheSignatureBestEffort(remote, testModelName, "same-text", strings.Repeat("replacement", 6)) {
		t.Fatal("failed Home write reported success")
	}
	if got, err := GetCachedSignatureRequired(local, testModelName, "same-text"); got != signature || err != nil {
		t.Fatalf("failed Home write changed local state: %q, %v", got, err)
	}
	ClearSignatureCache(testModelName)
	if client.delCount != 0 {
		t.Fatal("local group clear issued a remote prefix delete")
	}
}

func TestSignatureRequiredLocalScopesRemainIndependent(t *testing.T) {
	ClearSignatureCache("")
	t.Cleanup(func() { ClearSignatureCache("") })
	var contexts = []home.ClientScope{{ID: "service-a"}, {ID: "service-b"}}
	for index, scope := range contexts {
		ctx := home.WithClientScope(t.Context(), scope)
		signature := strings.Repeat(string(rune('a'+index)), 64)
		if !CacheSignatureBestEffort(ctx, testModelName, "same-text", signature) {
			t.Fatal("local write failed")
		}
		if got, err := GetCachedSignatureRequired(ctx, testModelName, "same-text"); err != nil || got != signature {
			t.Fatalf("scope=%s signature=%q error=%v", scope.ID, got, err)
		}
	}
	first := home.WithClientScope(t.Context(), contexts[0])
	if got, err := GetCachedSignatureRequired(
		first, testModelName, "same-text",
	); err != nil || got != strings.Repeat("a", 64) {
		t.Fatalf("second service replaced first signature: %q, %v", got, err)
	}
	if err := DeleteCachedSignatureRequired(first, testModelName, "same-text"); err != nil {
		t.Fatal(err)
	}
	second := home.WithClientScope(t.Context(), contexts[1])
	if got, err := GetCachedSignatureRequired(
		second, testModelName, "same-text",
	); err != nil || got != strings.Repeat("b", 64) {
		t.Fatalf("first delete changed second service: %q, %v", got, err)
	}
	ClearSignatureCache(testModelName)
	if got, err := GetCachedSignatureRequired(second, testModelName, "same-text"); err != nil || got != "" {
		t.Fatalf("group clear left scoped signature: %q, %v", got, err)
	}
}

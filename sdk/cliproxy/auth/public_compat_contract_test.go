package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
)

// noinspection GoDfaErrorMayBeNotNil
func TestPublicForceRefreshKeepsPublishedSafeSummary(t *testing.T) {
	manager := auth.NewManager(nil, nil, nil)
	var refresh = manager.ForceRefreshAuth
	if result, err := refresh(t.Context(), "missing-auth"); result != (auth.ForceRefreshResult{}) ||
		err == nil {
		t.Fatalf("missing credential refresh result=%+v error=%v", result, err)
	}
	credential := &auth.Auth{
		ID: "refresh-id", FileName: "nested/refresh.json", Provider: "codex",
		Metadata: map[string]any{
			"access_token":  "original-access-token",
			"refresh_token": "original-refresh-token",
		},
	}
	if _, err := manager.Register(t.Context(), credential.Clone()); err != nil {
		t.Fatal(err)
	}
	summary, refreshError := refresh(t.Context(), credential.ID)
	if refreshError == nil {
		t.Fatal("missing executor refresh lost its error")
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil || len(fields) != 5 ||
		summary.FileName != "refresh.json" {
		t.Fatalf("refresh summary=%s error=%v", encoded, err)
	}
	if strings.Contains(string(encoded), "original-access-token") || strings.Contains(
		string(encoded), "original-refresh-token",
	) ||
		credential.Index != "" || credential.Metadata["access_token"] != "original-access-token" {
		t.Fatal("refresh summary exposed credentials or changed the supplied Auth")
	}
}

func TestPublicAutoRefreshStopKeepsPublishedContract(t *testing.T) {
	manager := auth.NewManager(nil, nil, nil)
	var stop = manager.StopAutoRefresh
	var lifecycle interface {
		StopAutoRefresh(context.Context) error
	} = manager
	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
	if err := lifecycle.StopAutoRefresh(nil); err != nil {
		t.Fatal(err)
	}
	var nilManager *auth.Manager
	//noinspection GoMaybeNil
	var nilStop = nilManager.StopAutoRefresh
	if err := nilStop(nil); err != nil {
		t.Fatal(err)
	}
}

func TestPublicUpstreamSummaryKeepsPublishedRedactionInputs(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	for _, tc := range []struct {
		name, input, mask, exact, secret string
	}{
		{"bearer header", "authorization: Bearer abcdef+TOPSECRET==", "Authorization: [REDACTED]", "", "TOPSECRET"},
		{
			"basic header", "authorization: Basic my-secret-basic-auth", "Authorization: [REDACTED]", "",
			"my-secret-basic-auth",
		},
		{"custom authorization", "authorization: ApiKey SUPERSECRET", "Authorization: [REDACTED]", "", "SUPERSECRET"},
		{
			"authorization parameters", "Authorization: ApiKey first,SECONDSECRET", "Authorization: [REDACTED]", "",
			"SECONDSECRET",
		},
		{
			"digest authorization",
			`Authorization: Digest username="Mufasa", realm="myrealm", nonce="NONCE", uri="/dir/index.html", ` +
				`response="SIG"`,
			"Authorization: [REDACTED]", "", "NONCE",
		},
		{
			"quoted password", `password="correct horse battery staple"`, "[REDACTED]", "",
			"correct horse battery staple",
		},
		{"quoted key", `api_key="secret,value"`, "[REDACTED]", "", "secret,value"},
		{"bare key", "invalid key sk-live-secret-key-123456", "[REDACTED]", "", "live-secret-key"},
		{"user path", "open /Users/alice/configs/auth.json: permission denied", "[REDACTED_PATH]", "", "alice"},
		{"JSON token", `{"code":"oops","message":"invalid token SUPERSECRET"}`, "[REDACTED]", "", "SUPERSECRET"},
		{"single quoted key", `'api_key'=SUPERSECRET`, "[REDACTED]", "", "SUPERSECRET"},
		{"authorization assignment", "authorization=Bearer SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"invalid API key", "invalid API key SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"invalid access token", "invalid access token SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{
			"proxy userinfo", "proxyconnect tcp: socks5://alice:PASSSECRET@proxy.internal:1080", "[REDACTED_AUTH]", "",
			"PASSSECRET",
		},
		{"signature query", "request failed: https://example.com?sig=SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{
			"escaped JSON token", `{"code":"oops","message":"invalid token \"SUPERSECRET\""}`, "[REDACTED]", "",
			"SUPERSECRET",
		},
		{
			"escaped JSON password", `{"code":"oops","message":"password=\"abc\\\"SUPERSECRET\""}`, "[REDACTED]", "",
			"SUPERSECRET",
		},
		{"natural API key", "upstream rejected API key: SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"natural password", "password is SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"mnt path", "open /mnt/secrets/alice: permission denied", "[REDACTED_PATH]", "", "alice"},
		{
			"windows space", `open C:\Users\Alice Smith\secret.txt: permission denied`, "[REDACTED_PATH]", "",
			"Alice Smith",
		},
		{"cookie", "Cookie: sessionid=COOKIESECRET", "Cookie: [REDACTED]", "", "COOKIESECRET"},
		{"set cookie", "Set-Cookie: session=SETCOOKIESECRET", "Cookie: [REDACTED]", "", "SETCOOKIESECRET"},
		{"private key", "private_key=PRIVATEKEYSECRET", "[REDACTED]", "", "PRIVATEKEYSECRET"},
		{"URI userinfo", "https://user:PASSSECRET@example.com/api", "https://[REDACTED_AUTH]@", "", "PASSSECRET"},
		{"workspace path", "/workspace/tenants/alice/oauth-cache", "[REDACTED_PATH]", "", "alice"},
		{"opt path", "open /opt/cli-proxy/auth/alice.json: failed", "[REDACTED_PATH]", "", "alice"},
		{"windows path", `open C:\Users\alice\secret.txt: failed`, "[REDACTED_PATH]", "", "alice"},
		{
			"two secrets", "failed with api_key=secret-value-123 and token: my-secret-token", "[REDACTED]", "",
			"secret-value-123",
		},
		{"incorrect key", "Incorrect API key provided: SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"credentials", "credentials: SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"environment key", "AWS_SECRET_ACCESS_KEY=SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"relative URI", "//alice:PASSSECRET@example.com/api", "[REDACTED_AUTH]", "", "PASSSECRET"},
		{"run path", "open /run/secrets/alice: permission denied", "[REDACTED_PATH]", "", "alice"},
		{"custom path", "open /custom/tenant/alice: permission denied", "[REDACTED_PATH]", "", "alice"},
		{"UNC path", `open \\server\share\alice\secret.txt: permission denied`, "[REDACTED_PATH]", "", "alice"},
		{"service key", "SERVICE_KEY=SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"OpenAI key", "OPENAI_KEY=SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{"custom key query", "https://example.com?x-key=SUPERSECRET", "[REDACTED]", "", "SUPERSECRET"},
		{
			"space path", "open /custom/tenant/Alice Smith/secret.txt: denied", "[REDACTED_PATH]",
			"open [REDACTED_PATH]: denied", "Smith",
		},
		{"unicode path", "open /custom/租户/alice: denied", "[REDACTED_PATH]", "", "alice"},
		{"single path", "open /alice: permission denied", "[REDACTED_PATH]", "", "alice"},
		{"single unicode path", "stat /客户: no such file or directory", "[REDACTED_PATH]", "", "客户"},
		{"quoted colon path", `open "/tmp/customer:TOPSECRET/creds": denied`, "[REDACTED_PATH]", "", "TOPSECRET"},
		{"multiword password", "password = correct horse battery staple", "[REDACTED]", "", "battery staple"},
		{"multiword credentials", "credentials: alice secret", "[REDACTED]", "", "alice secret"},
		{"assignment in path", `open "/Users/alice/password=foo/bar": denied`, "[REDACTED_PATH]", "", "alice"},
		{"colon component", "open /tmp/customer:TOPSECRET/creds: denied", "[REDACTED_PATH]", "", "TOPSECRET"},
		{
			"colon leaf", "open /tmp/customer:TOPSECRET: denied", "[REDACTED_PATH]", "open [REDACTED_PATH]: denied",
			"TOPSECRET",
		},
		{
			"two paths", "rename /Users/alice/source /Users/bob/private-data: denied", "[REDACTED_PATH]",
			"rename [REDACTED_PATH] [REDACTED_PATH]: denied", "bob",
		},
		{
			"two space paths", "rename /Users/Alice Smith/source /Users/bob/private-data: denied", "[REDACTED_PATH]",
			"rename [REDACTED_PATH] [REDACTED_PATH]: denied", "Alice Smith",
		},
		{
			"space filename", "open /tmp/Alice Smith.txt: denied", "[REDACTED_PATH]", "open [REDACTED_PATH]: denied",
			"Alice Smith",
		},
		{
			"space leaves", "rename /tmp/Alice Smith /tmp/Bob Jones: denied", "[REDACTED_PATH]",
			"rename [REDACTED_PATH] [REDACTED_PATH]: denied", "Alice Smith",
		},
		{
			"colon space leaf", "open /tmp/customer: TOPSECRET: denied", "[REDACTED_PATH]",
			"open [REDACTED_PATH]: denied", "TOPSECRET",
		},
		{
			"parenthesis", "open (/tmp/customer): denied", "[REDACTED_PATH]", "open ([REDACTED_PATH]): denied",
			"customer",
		},
		{"braces", "open {/tmp/customer}: denied", "[REDACTED_PATH]", "open {[REDACTED_PATH]}: denied", "customer"},
		{
			"nested error", "open /tmp/config: permission denied: retry later", "[REDACTED_PATH]",
			"open [REDACTED_PATH]: permission denied: retry later", "config",
		},
		{
			"two known errors", "open /tmp/config: permission denied: access denied", "[REDACTED_PATH]",
			"open [REDACTED_PATH]: permission denied: access denied", "config",
		},
		{
			"uppercase connector", "copy /tmp/a   TO\t/tmp/b: denied", "[REDACTED_PATH]",
			"copy [REDACTED_PATH]   TO\t[REDACTED_PATH]: denied", "",
		},
		{"bounded connector", strings.Repeat("a", 300) + " to /tmp/x: denied", "...", "", "x"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				for _, extract := range []bool{false, true} {
					got := auth.SanitizeUpstreamErrorSummary(tc.input)
					if extract {
						got = auth.ExtractUpstreamErrorSummary(tc.input)
					}
					if !strings.Contains(got, tc.mask) || (tc.exact != "" && got != tc.exact) {
						t.Errorf(
							"summary(extract=%v) = %q, want mask %q, exact %q",
							extract,
							got,
							tc.mask,
							tc.exact,
						)
					}
					if tc.secret != "" && strings.Contains(got, tc.secret) {
						t.Errorf("summary(extract=%v) leaked %q: %q", extract, tc.secret, got)
					}
					if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 256 {
						t.Errorf("summary(extract=%v) exceeds Unicode bound: %q", extract, got)
					}
				}
			},
		)
	}
}

func TestPublicUpstreamSummaryPreservesUsefulText(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"  overload, retry later  ", "overload, retry later"},
		{"", ""},
		{`{"error":{"code":"busy","message":"retry later"}}`, "busy: retry later"},
		{`status code: 503: {"error":{"type":"busy","message":"BUSY, retry later"}}`, "BUSY, retry later"},
		{`{"error":"retry later"}`, "retry later"},
		{`{"type":"busy"}`, "busy"},
		{`{"message":"retry later"}`, "retry later"},
		{strings.Repeat("界", 256), strings.Repeat("界", 256)},
		{strings.Repeat("界", 257), strings.Repeat("界", 253) + "..."},
	} {
		if got := auth.ExtractUpstreamErrorSummary(tc.input); got != tc.want {
			t.Errorf("Extract(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// noinspection GoDeprecation
func TestPublicSessionTreeCompatibilityKeepsCallerOwnedRecords(t *testing.T) {
	selector := auth.NewSessionAffinitySelector(nil)
	defer selector.Stop()
	store := selector.Trees()
	var contract session.SessionTreeStore = store
	root := session.SessionTreeInfo{
		SessionID: "root", AgentName: "main", ClientType: "claude", CallerScope: "caller", AuthID: "a",
		Provider: "claude", Model: "m", NodeKind: "session", Metadata: map[string]any{"fixture": "original"},
	}
	first := contract.RecordNode(root)
	if first == nil || first.TreePath != "root" || first.TreeDepth != 0 || first.CreatedAt.IsZero() ||
		first.UpdatedAt.IsZero() {
		t.Fatalf("root = %+v", first)
	}
	root.Metadata["fixture"] = "caller change"
	first.Metadata["fixture"] = "returned change"
	first.LastAuthID = "returned auth"
	got, ok := contract.GetNode("root")
	if !ok || got.Metadata["fixture"] != "original" || got.LastAuthID != "a" ||
		got.AgentName != root.AgentName ||
		got.ClientType != root.ClientType ||
		got.CallerScope != root.CallerScope ||
		got.LastProvider != root.Provider ||
		got.LastModel != root.Model ||
		got.NodeKind != root.NodeKind {
		t.Fatalf("stored root = %+v, ok=%v", got, ok)
	}
	child := contract.RecordNode(
		session.SessionTreeInfo{
			SessionID: "child", ParentSessionID: "root", IsFork: true, IsCompaction: true,
		},
	)
	contract.RecordNode(session.SessionTreeInfo{SessionID: "grandchild", ParentSessionID: "child"})
	if child.TreePath != "root/child" || child.TreeDepth != 1 || !child.IsFork || !child.IsCompaction ||
		contract.Len() != 3 {
		t.Fatalf("child = %+v, len=%d", child, contract.Len())
	}
	for _, query := range []func(string) []*session.SessionTreeNode{contract.GetTree, contract.GetSubtree} {
		nodes := query("root")
		ids := map[string]bool{}
		for _, node := range nodes {
			ids[node.SessionID] = true
		}
		if !reflect.DeepEqual(ids, map[string]bool{"root": true, "child": true}) {
			t.Fatalf("immediate tree = %v", ids)
		}
	}
	if ancestors := contract.Ancestors("grandchild"); !reflect.DeepEqual(ancestors, []string{"child"}) {
		t.Fatalf("ancestors = %v", ancestors)
	}
	if !contract.UpdateAffinity("root", "b", "", "next") || contract.UpdateAffinity("missing", "x", "x", "x") {
		t.Fatal("affinity update result differs")
	}
	updated, _ := contract.GetNode("root")
	if updated.LastAuthID != "b" || updated.LastProvider != "claude" || updated.LastModel != "next" ||
		!updated.CreatedAt.Equal(first.CreatedAt) ||
		updated.UpdatedAt.Before(first.UpdatedAt) {
		t.Fatalf("updated = %+v", updated)
	}
	clone := updated.Clone()
	clone.Metadata["fixture"] = "clone change"
	if updated.Metadata["fixture"] != "original" {
		t.Fatal("Clone shares top-level metadata")
	}
	encoded, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(
		encoded, &wire,
	); err !=
		nil ||
		string(wire["tree_path"]) !=
			`"root/child"` ||
		string(wire["tree_depth"]) !=
			"1" ||
		string(wire["session_id"]) !=
			`"child"` {
		t.Fatalf("tree JSON = %s, %v", encoded, err)
	}
	if selector.Trees() == store || selector.Trees().Len() != 0 {
		t.Fatal("Trees must return an independent store")
	}
	bound, status := selector.LookupAffinity("claude", "m", "root")
	if bound != "" || status != "unbound" {
		t.Fatalf("compatibility records changed routing: %q, %q", bound, status)
	}
	contract.Clear()
	if contract.Len() != 0 || len(contract.GetTree("root")) != 0 ||
		contract.RecordNode(session.SessionTreeInfo{}) != nil {
		t.Fatal("Clear or empty identity result differs")
	}
	if _, ok = contract.GetNode("root"); ok {
		t.Fatal("Clear retained root")
	}
}

// noinspection GoDeprecation
func TestPublicSessionTreeCompatibilityNilAndConcurrentUse(t *testing.T) {
	var nilStore *session.InMemorySessionTreeStore
	var nilNode *session.SessionTreeNode
	if nilNode.Clone() != nil || nilStore.Len() != 0 ||
		nilStore.RecordNode(session.SessionTreeInfo{SessionID: "nil"}) != nil ||
		nilStore.UpdateAffinity(
			"x", "a", "p", "m",
		) ||
		len(nilStore.GetSubtree("x")) != 0 ||
		len(nilStore.Ancestors("x")) != 0 {
		t.Fatal("nil receiver contract differs")
	}
	nilStore.Clear()
	if node, ok := nilStore.GetNode("x"); node != nil || ok {
		t.Fatal("nil store returned a node")
	}
	store := session.NewInMemorySessionTreeStore(0, time.Hour)
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Go(
			func() {
				// Keep the rune intent explicit for the integer-to-string check in go vet.
				//goland:noinspection GoRedundantConversion
				id := string(rune('a' + worker))
				for range 20 {
					store.RecordNode(session.SessionTreeInfo{SessionID: id})
					store.UpdateAffinity(id, "a", "p", "m")
					store.GetNode(id)
					store.GetTree(id)
					store.Ancestors(id)
					store.Clear()
				}
			},
		)
	}
	workers.Wait()
	store.Clear()
	if store.Len() != 0 {
		t.Fatal("store retained nodes after Clear")
	}
}

func TestPublicClaudeAgentAffinityPreservesParentAndDisabledInheritance(t *testing.T) {
	for _, tc := range []struct {
		provider, model string
		inherit         bool
	}{
		{"claude", "claude-sonnet-5-5", true},
		{"claude", "claude-sonnet-5-5", false},
		{"gemini", "gemini-3.8-flash", false},
		{"antigravity", "gemini-3.8-flash", false},
	} {
		t.Run(
			tc.provider+"/"+map[bool]string{true: "inherit", false: "independent"}[tc.inherit],
			func(t *testing.T) {
				selector := auth.NewSessionAffinitySelectorWithConfig(
					auth.SessionAffinityConfig{
						Fallback: &auth.FillFirstSelector{}, SubagentAffinity: &tc.inherit,
					},
				)
				defer selector.Stop()
				candidates := []*auth.Auth{{ID: "a", Provider: tc.provider}, {ID: "b", Provider: tc.provider}}
				payload := []byte(
					`{"metadata":{"user_id":"{\"device_id\":\"fixture\",\"session_id\":\"root\"}"}}`,
				)
				root, err := selector.Pick(
					t.Context(), tc.provider, tc.model,
					core.Options{OriginalRequest: payload, Metadata: map[string]any{}}, candidates[1:],
				)
				if err != nil || root == nil || root.ID != "b" {
					t.Fatalf("parent = %+v, %v", root, err)
				}
				for range 2 {
					opts := core.Options{
						OriginalRequest: payload, Headers: http.Header{"X-Claude-Code-Agent-Id": {"child"}},
						Metadata: map[string]any{},
					}
					child, err := selector.Pick(t.Context(), tc.provider, tc.model, opts, candidates)
					want := "a"
					if tc.inherit {
						want = "b"
					}
					if err != nil || child == nil || child.ID != want ||
						opts.Metadata[core.CanonicalSessionIDMetadataKey] != "claude:root:agent:child" ||
						opts.Metadata[core.ParentSessionIDMetadataKey] != "claude:root" {
						t.Fatalf("child = %+v, %v, want %s, metadata=%v", child, err, want, opts.Metadata)
					}
				}
			},
		)
	}
	t.Run(
		"explicit parent agent", func(t *testing.T) {
			selector := auth.NewSessionAffinitySelector(&auth.FillFirstSelector{})
			defer selector.Stop()
			candidates := []*auth.Auth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}
			payload := []byte(`{"metadata":{"user_id":"{\"session_id\":\"root\"}"}}`)
			branch := core.Options{
				OriginalRequest: payload, Headers: http.Header{"X-Claude-Code-Agent-Id": {"branch"}},
				Metadata: map[string]any{core.SessionAffinityModelMetadataKey: "claude-sonnet-5-5"},
			}
			selector.Remember("claude", branch, candidates[1])
			opts := core.Options{
				OriginalRequest: payload,
				Headers: http.Header{
					"X-Claude-Code-Agent-Id":        {"leaf"},
					"X-Claude-Code-Parent-Agent-Id": {"branch"},
				},
				Metadata: map[string]any{},
			}
			leaf, err := selector.Pick(t.Context(), "claude", "claude-sonnet-5-5", opts, candidates)
			if err != nil || leaf == nil || leaf.ID != "b" ||
				opts.Metadata[core.ParentSessionIDMetadataKey] != "claude:root:agent:branch" {
				t.Fatalf("leaf = %+v, %v, metadata=%v", leaf, err, opts.Metadata)
			}
		},
	)
}

func TestPublicExtractSessionIDKeepsPublishedSources(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		headers          http.Header
		metadata         map[string]any
	}{
		{name: "Claude header", headers: http.Header{"X-Claude-Code-Session-Id": {"s"}}, want: "claude:s"},
		{name: "Codex dashed header", headers: http.Header{"Session-Id": {"s"}}, want: "s"},
		{name: "Codex underscore header", headers: http.Header{"Session_id": {"s"}}, want: "s"},
		{name: "generic header", headers: http.Header{"X-Session-ID": {"s"}}, want: "s"},
		{name: "affinity header", headers: http.Header{"X-Session-Affinity": {"s"}}, want: "s"},
		{name: "conversation header", headers: http.Header{"Conversation_id": {"s"}}, want: "s"},
		{name: "Amp header", headers: http.Header{"X-Amp-Thread-Id": {"s"}}, want: "s"},
		{name: "request header", headers: http.Header{"X-Client-Request-Id": {"s"}}, want: "s"},
		{name: "conversation object", body: `{"conversation":{"id":"s"}}`, want: "s"},
		{name: "conversation text", body: `{"conversation":"s"}`, want: "s"},
		{name: "cache body", body: `{"prompt_cache_key":"s"}`, want: "s"},
		{name: "session snake", body: `{"session_id":"s"}`, want: "s"},
		{name: "session camel", body: `{"sessionId":"s"}`, want: "s"},
		{name: "user metadata", body: `{"metadata":{"user_id":"s"}}`, want: "user:s"},
		{name: "conversation body", body: `{"conversation_id":"s"}`, want: "s"},
		{
			name: "execution metadata", metadata: map[string]any{core.ExecutionSessionMetadataKey: "s"},
			want: "execution:s",
		},
		{name: "derived metadata", metadata: map[string]any{"derived_session_id": "s"}, want: "derived:s"},
		{name: "root metadata", metadata: map[string]any{"session_root_id": "s"}, want: "derived:s"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				if got := auth.ExtractSessionID(tc.headers, []byte(tc.body), tc.metadata); got != tc.want {
					t.Fatalf("ExtractSessionID = %q, want %q", got, tc.want)
				}
			},
		)
	}
}

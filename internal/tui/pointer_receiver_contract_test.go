package tui

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestTUIModelsUsePointerReceivers(t *testing.T) {
	files := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var sources []*ast.File
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, entry.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if file.Name.Name == "tui" {
			sources = append(sources, file)
		}
	}
	if len(sources) == 0 {
		t.Fatal("TUI source package not found")
	}
	models := make(map[string]bool)
	for _, file := range sources {
		ast.Inspect(
			file, func(node ast.Node) bool {
				if spec, ok := node.(*ast.TypeSpec); ok {
					if _, ok := spec.Type.(*ast.StructType); ok &&
						(spec.Name.Name == "app" || strings.HasSuffix(spec.Name.Name, "Model")) {
						models[spec.Name.Name] = true
					}
				}
				return true
			},
		)
	}
	if len(models) == 0 {
		t.Fatal("no TUI model structs found")
	}
	for _, file := range sources {
		for _, decl := range file.Decls {
			method, ok := decl.(*ast.FuncDecl)
			if !ok || method.Recv == nil {
				continue
			}
			if receiver, ok := method.Recv.List[0].Type.(*ast.Ident); ok && models[receiver.Name] {
				t.Errorf(
					"%s: %s.%s uses a value receiver",
					files.Position(method.Pos()),
					receiver.Name,
					method.Name.Name,
				)
			}
		}
	}
}

type snapshotRoundTripper func(*http.Request) (*http.Response, error)

func (f snapshotRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func snapshotResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestConfigCommandsKeepFieldSnapshot(t *testing.T) {
	for _, edit := range []struct {
		name, path, input string
		want              any
		wantErr           bool
	}{
		{name: "toggle", path: "logging-to-file", want: false},
		{name: "integer", path: "request-retry", input: "7", want: 7},
		{name: "string", path: "proxy-url", input: "http://fixture.invalid:8080", want: "http://fixture.invalid:8080"},
		{name: "invalid integer", path: "request-retry", input: "invalid", wantErr: true},
	} {
		for _, change := range []string{"unchanged", "failed reload", "replaced field"} {
			t.Run(
				edit.name+"/"+change, func(t *testing.T) {
					cfg := map[string]any{"logging-to-file": true, "request-retry": float64(3), "proxy-url": ""}
					puts, gets := 0, 0
					client := &Client{baseURL: "https://snapshot.invalid"}
					client.http = &http.Client{
						Transport: snapshotRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								switch request.Method {
								case http.MethodPut:
									puts++
									if request.URL.Path != "/v0/management/"+edit.path {
										t.Fatalf("save path = %s", request.URL.Path)
									}
									var body map[string]json.RawMessage
									if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
										t.Fatal(err)
									}
									wantValue, err := json.Marshal(edit.want)
									if err != nil {
										t.Fatal(err)
									}
									if len(body) != 1 || string(body["value"]) != string(wantValue) {
										t.Fatalf("save body = %s, want value=%s", body["value"], wantValue)
									}
									cfg[edit.path] = edit.want
									return snapshotResponse(`{}`), nil
								case http.MethodGet:
									gets++
									if request.URL.Path != "/v0/management/config" {
										t.Fatalf("reload path = %s", request.URL.Path)
									}
									body, err := json.Marshal(cfg)
									if err != nil {
										t.Fatal(err)
									}
									return snapshotResponse(string(body)), nil
								default:
									t.Fatalf("unexpected request = %s %s", request.Method, request.URL)
									return nil, nil
								}
							},
						),
					}
					root := newApp(client, "", nil)
					root.authenticated = true
					root.activeTab = tabConfig
					root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
					root.Update(configDataMsg{config: cfg})
					index := -1
					for i, field := range root.config.fields {
						if field.apiPath == edit.path {
							index = i
						}
					}
					if index < 0 {
						t.Fatalf("editable field %s not found", edit.path)
					}
					root.config.cursor = index
					_, command := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					if edit.path != "logging-to-file" {
						root.config.textInput.SetValue(edit.input)
						_, command = root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					}
					if command == nil || puts != 0 || gets != 0 {
						t.Fatal("edit did not create a deferred command")
					}
					switch change {
					case "failed reload":
						root.Update(configDataMsg{err: errors.New("fixture reload failed")})
					case "replaced field":
						root.config.fields[index] = configField{
							apiPath: "wrong-field", kind: "string", value: "later value",
						}
					}
					root.config.cursor = 0
					root.config.textInput.SetValue("later input")
					root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
					result, ok := command().(configUpdateMsg)
					if !ok || result.path != edit.path || (result.err != nil) != edit.wantErr {
						t.Fatalf("save result = %#v", result)
					}
					if !edit.wantErr && result.value != edit.want {
						t.Fatalf("save result value = %#v, want %#v", result.value, edit.want)
					}
					_, reload := root.Update(result)
					if root.activeTab != tabAuthFiles || root.config.message == "" || reload == nil {
						t.Fatal("inactive config tab lost its save result or reload command")
					}
					if edit.path == "logging-to-file" && root.logsEnabled {
						t.Fatal("app did not consume the original logging setting")
					}
					root.Update(reload())
					wantPuts := 1
					if edit.wantErr {
						wantPuts = 0
					}
					if puts != wantPuts || gets != 1 || root.config.err != nil || len(root.config.fields) == 0 {
						t.Fatalf("save/reload = PUT %d GET %d error %v", puts, gets, root.config.err)
					}
				},
			)
		}
	}
}

func TestOAuthCommandsKeepClientSnapshot(t *testing.T) {
	for _, action := range []string{"start", "callback", "cancel", "poll"} {
		t.Run(
			action, func(t *testing.T) {
				requests := 0
				const state = "fixture&state"
				const callbackURL = "https://callback.invalid/?code=fixture&state=fixture%26state"
				client := &Client{baseURL: "https://snapshot.invalid", secretKey: "fixture-management"}
				client.http = &http.Client{
					Transport: snapshotRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							requests++
							if request.Header.Get("Authorization") != "Bearer fixture-management" {
								t.Fatal("command used a different management client")
							}
							switch action {
							case "start":
								if request.Method != http.MethodGet ||
									request.URL.RequestURI() != "/v0/management/fixture-auth-url?is_webui=true" {
									t.Fatalf("start request = %s %s", request.Method, request.URL)
								}
								return snapshotResponse(
									`{"url":"https://provider.invalid/authorize","state":"fixture&state",` +
										`"expires_in":` +
										`60}`,
								), nil
							case "callback":
								if request.Method != http.MethodPost ||
									request.URL.Path != "/v0/management/oauth-callback" {
									t.Fatalf("callback request = %s %s", request.Method, request.URL)
								}
								var body map[string]string
								if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
									t.Fatal(err)
								}
								if len(body) != 3 || body["provider"] != "fixture" ||
									body["state"] != state || body["redirect_url"] != callbackURL {
									t.Fatalf("callback body = %#v", body)
								}
								return snapshotResponse(`{}`), nil
							case "cancel":
								if request.Method != http.MethodDelete ||
									request.URL.Path != "/v0/management/oauth-session" ||
									request.URL.Query().Get("state") != state {
									t.Fatalf("cancel request = %s %s", request.Method, request.URL)
								}
								return snapshotResponse(`{"cancelled":true}`), nil
							default:
								if request.Method != http.MethodGet ||
									request.URL.Path != "/v0/management/get-auth-status" ||
									request.URL.Query().Get("state") != state {
									t.Fatalf("poll request = %s %s", request.Method, request.URL)
								}
								return snapshotResponse(`{"status":"ok"}`), nil
							}
						},
					),
				}
				model := newOAuthTabModel(client)
				model.setSize(120, 32)
				model.state, model.authState, model.providerKey, model.pollGeneration = oauthRemote, state, "fixture", 7
				flowErr := errors.New("fixture poll timeout")
				var command tea.Cmd
				switch action {
				case "start":
					provider := oauthProvider{
						name: "Fixture", key: "fixture", apiPath: "fixture-auth-url", deviceFlow: true,
					}
					command = model.startOAuth(provider, 7)
				case "callback":
					command = model.submitCallback(callbackURL)
				case "cancel":
					command = model.cancelOAuthSession("  "+state+"  ", 7, flowErr)
				case "poll":
					command = model.pollOAuthStatus(state, 60, true, 7)
				}
				model, _ = model.update(
					oauthPollMsg{state: state, generation: 7, done: true, message: "completed"},
				)
				model.resetRemoteOAuth()
				model.client, model.authState, model.providerKey, model.pollGeneration = nil, "later-state",
					"later-provider", 8
				if requests != 0 {
					t.Fatal("command issued a request before execution")
				}
				switch result := command().(type) {
				case oauthStartMsg:
					if result.err != nil || result.state != state || result.generation != 7 ||
						result.providerName != "Fixture" || result.providerKey != "fixture" ||
						!result.deviceFlow || result.expiresIn != 60 {
						t.Fatalf("start result = %#v", result)
					}
				case oauthCallbackSubmitMsg:
					if result.err != nil || result.state != state || result.generation != 7 {
						t.Fatalf("callback result = %#v", result)
					}
				case oauthCancelMsg:
					if result.err != nil || !result.cancelled || result.state != state ||
						result.generation != 7 || !errors.Is(result.flowErr, flowErr) {
						t.Fatalf("cancel result = %#v", result)
					}
				case oauthPollMsg:
					if result.err != nil || !result.done || result.state != state || result.generation != 7 {
						t.Fatalf("poll result = %#v", result)
					}
				default:
					t.Fatalf("unexpected result = %#v", result)
				}
				if requests != 1 {
					t.Fatalf("requests = %d, want 1", requests)
				}
			},
		)
	}
}

func TestAppAuthEditKeepsOwnedModel(t *testing.T) {
	for field, key := range []rune{'1', '2', '3'} {
		t.Run(
			string(key), func(t *testing.T) {
				root := newApp(nil, "", nil)
				root.authenticated = true
				root.activeTab = tabAuthFiles
				root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
				root.auth.files = []map[string]any{{"name": "fixture.json", "prefix": "old-prefix"}}
				_, command := root.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
				if command == nil || !root.auth.editing || root.auth.editField != field ||
					root.auth.editFileName != "fixture.json" {
					t.Fatal("app overwrote the model's inline edit state")
				}
			},
		)
	}
}

func TestAppOAuthCommandsAfterTabSwitch(t *testing.T) {
	previousOpen := openOAuthBrowser
	openOAuthBrowser = func(string) error { return nil }
	t.Cleanup(func() { openOAuthBrowser = previousOpen })
	for _, action := range []string{"start", "callback", "cancel", "poll"} {
		t.Run(
			action, func(t *testing.T) {
				requests := 0
				client := &Client{baseURL: "https://snapshot.invalid"}
				client.http = &http.Client{
					Transport: snapshotRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							requests++
							switch request.URL.Path {
							case "/v0/management/gemini-cli-auth-url":
								return snapshotResponse(
									`{"url":"https://provider.invalid/authorize","state":"fixture-state",` +
										`"flow":"device"}`,
								), nil
							case "/v0/management/oauth-callback":
								return snapshotResponse(`{}`), nil
							case "/v0/management/oauth-session":
								return snapshotResponse(`{"cancelled":true}`), nil
							case "/v0/management/get-auth-status":
								return snapshotResponse(`{"status":"ok"}`), nil
							default:
								t.Fatalf("unexpected request = %s %s", request.Method, request.URL)
								return nil, nil
							}
						},
					),
				}
				root := newApp(client, "", nil)
				root.authenticated = true
				root.activeTab = tabOAuth
				root.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
				var command tea.Cmd
				switch action {
				case "start":
					_, command = root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				case "poll":
					_, command = root.Update(
						oauthStartMsg{
							url: "https://provider.invalid/authorize", state: "fixture-state", providerKey: "gemini",
							deviceFlow: true,
						},
					)
				default:
					root.oauth.state, root.oauth.authState, root.oauth.providerKey = oauthRemote,
						"fixture-state", "gemini"
					if action == "callback" {
						root.oauth.inputActive = true
						root.oauth.callbackInput.SetValue(
							"https://callback.invalid/?code=fixture&state=fixture-state",
						)
						_, command = root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					} else {
						_, command = root.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
						if root.oauth.state != oauthCancelling || root.oauth.pollGeneration != 1 {
							t.Fatal("app overwrote cancellation state with an earlier model value")
						}
					}
				}
				if command == nil || requests != 0 {
					t.Fatal("OAuth action did not create a deferred command")
				}
				root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
				returned, followup := root.Update(command())
				if returned != &root || root.activeTab != tabUsage || requests != 1 {
					t.Fatal("OAuth response changed root ownership or active tab")
				}
				switch action {
				case "start":
					if root.oauth.state != oauthRemote || root.oauth.authState != "fixture-state" ||
						followup == nil {
						t.Fatal("background start result or polling command lost")
					}
				case "callback":
					if root.oauth.state != oauthRemote || !strings.Contains(
						root.oauth.message, "✓ ",
					) || followup != nil {
						t.Fatal("background callback result lost")
					}
				case "cancel":
					if root.oauth.state != oauthIdle || root.oauth.authState != "" || followup != nil {
						t.Fatal("background cancellation result lost")
					}
				case "poll":
					if root.oauth.state != oauthSuccess || followup != nil {
						t.Fatal("background poll result lost")
					}
				}
			},
		)
	}
}

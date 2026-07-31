package auth

import (
	"context"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type labeledPluginAuthParser struct {
	id string
}

func (p labeledPluginAuthParser) ParseAuth(
	_ context.Context,
	req pluginapi.AuthParseRequest,
) (*cliproxyauth.Auth, bool, error) {
	return &cliproxyauth.Auth{
		ID:       p.id,
		Provider: req.Provider,
		FileName: req.FileName,
	}, true, nil
}

func TestPluginAuthParserIsolatedByContext(t *testing.T) {
	RegisterPluginAuthParser(labeledPluginAuthParser{id: "global"})
	t.Cleanup(func() { RegisterPluginAuthParser(nil) })

	ctxA := WithPluginAuthParser(context.Background(), labeledPluginAuthParser{id: "service-a"})
	ctxB := WithPluginAuthParser(context.Background(), labeledPluginAuthParser{id: "service-b"})
	ctxNil := WithPluginAuthParser(context.Background(), nil)

	for name, test := range map[string]struct {
		ctx    context.Context
		wantID string
	}{
		"service-a":     {ctx: ctxA, wantID: "service-a"},
		"service-b":     {ctx: ctxB, wantID: "service-b"},
		"compatibility": {ctx: context.Background(), wantID: "global"},
	} {
		auth, handled, errParse := ParsePluginAuthFile(
			test.ctx,
			"plugin-provider",
			"/tmp/plugin.json",
			"plugin.json",
			[]byte(`{"type":"plugin-provider"}`),
		)
		if errParse != nil {
			t.Fatalf("%s ParsePluginAuthFile() error = %v", name, errParse)
		}
		if !handled || auth == nil || auth.ID != test.wantID {
			t.Fatalf("%s auth = %#v, handled = %t", name, auth, handled)
		}
	}

	auth, handled, errParse := ParsePluginAuthFile(
		ctxNil,
		"plugin-provider",
		"/tmp/plugin.json",
		"plugin.json",
		[]byte(`{"type":"plugin-provider"}`),
	)
	if errParse != nil || handled || auth != nil {
		t.Fatalf("explicit nil parser = (%#v, %t, %v), want no parser", auth, handled, errParse)
	}
}

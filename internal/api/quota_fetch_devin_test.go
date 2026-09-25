package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestFetchDevinQuotaUsesSelectedAuthIdentityOnly(t *testing.T) {
	var callsA atomic.Int32
	var callsB atomic.Int32
	newServer := func(label string, daily, weekly uint64, calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			if request.Method != http.MethodPost || request.URL.Path != devinauth.DevinGetUserStatusPath {
				t.Fatalf("%s request = %s %s", label, request.Method, request.URL.Path)
			}
			if request.Header.Get("Sentry-Trace") != "" {
				t.Fatalf("%s status request included Sentry-Trace", label)
			}
			if request.Header.Get("Authorization") != "Basic devin-session-token$"+label+"-devin-session-token$"+label {
				t.Fatalf("%s Authorization = %q", label, request.Header.Get("Authorization"))
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatalf("%s read request: %v", label, err)
			}
			if !bytes.Contains(body, []byte("devin-session-token$"+label)) {
				t.Fatalf("%s request metadata omitted selected token", label)
			}
			_, _ = writer.Write(testAPIQuotaDevinStatus(daily, weekly))
		}))
	}
	serverA := newServer("A", 0, 40, &callsA)
	defer serverA.Close()
	serverB := newServer("B", 80, 90, &callsB)
	defer serverB.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	var indexA string
	for _, auth := range []*coreauth.Auth{
		{
			ID: "devin-a", Index: "devin-a-index", FileName: "devin-shared.json", Provider: "devin",
			Attributes: map[string]string{
				"api_key": "devin-session-token$A", "base_url": serverA.URL, "device_seed": "seed-a",
				coreauth.AttributeAuthIndexSeed: "devin-user-a",
			},
		},
		{
			ID: "devin-b", Index: "devin-b-index", FileName: "devin-shared.json", Provider: "devin",
			Attributes: map[string]string{
				"api_key": "devin-session-token$B", "base_url": serverB.URL, "device_seed": "seed-b",
				coreauth.AttributeAuthIndexSeed: "devin-user-b",
			},
		},
	} {
		registered, err := manager.Register(context.Background(), auth)
		if err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		if auth.ID == "devin-a" {
			indexA = registered.Index
		}
	}

	result, err := fetchDevinQuotaResultContext(
		context.Background(), &config.Config{}, manager, indexA,
	)
	if err != nil {
		t.Fatalf("fetchDevinQuotaResultContext() error = %v", err)
	}
	if callsA.Load() != 1 || callsB.Load() != 0 {
		t.Fatalf("status calls A/B = %d/%d, want 1/0", callsA.Load(), callsB.Load())
	}
	if result.Source != "devin_user_status" || result.Completeness != quota.FetchComplete || result.ObservedAt.IsZero() {
		t.Fatalf("fetch result = %#v", result)
	}
	if !gjson.GetBytes(result.Data, "daily_quota_present").Bool() ||
		gjson.GetBytes(result.Data, "daily_quota_remaining_percent").Int() != 0 ||
		gjson.GetBytes(result.Data, "weekly_quota_remaining_percent").Int() != 40 {
		t.Fatalf("status payload = %s", result.Data)
	}
	if strings.Contains(string(result.Data), "devin-session-token$") {
		t.Fatalf("quota payload exposed session token: %s", result.Data)
	}

	if _, err = fetchDevinQuotaResultContext(
		context.Background(), &config.Config{}, manager, "missing-index",
	); err == nil {
		t.Fatal("missing Devin auth index returned success")
	}
}

func TestRegisterAuthManagerForQuotaKeepsDevinSiblingIdentities(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	indexes := make(map[string]string)
	for _, auth := range []*coreauth.Auth{
		{
			ID: "devin-a", FileName: "devin-shared.json", Provider: "devin",
			Attributes: map[string]string{coreauth.AttributeAuthIndexSeed: "devin-user-a"},
		},
		{
			ID: "devin-b", FileName: "devin-shared.json", Provider: "devin", Disabled: true,
			Attributes: map[string]string{coreauth.AttributeAuthIndexSeed: "devin-user-b"},
		},
	} {
		registered, err := manager.Register(context.Background(), auth)
		if err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		indexes[auth.ID] = registered.Index
	}
	if indexes["devin-a"] == "" || indexes["devin-b"] == "" || indexes["devin-a"] == indexes["devin-b"] {
		t.Fatalf("Devin sibling indexes = %#v, want distinct non-empty indexes", indexes)
	}
	scheduler := quota.NewScheduler(quota.DefaultConfig(), nil)
	if count := registerAuthManagerForQuota(scheduler, manager); count != 2 {
		t.Fatalf("registered count = %d, want 2", count)
	}

	status := scheduler.GetStatus().Credentials
	keyA := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", indexes["devin-a"])
	keyB := quota.BindingEntryKey(quota.TypeDevin, "devin-shared.json", indexes["devin-b"])
	if len(status) != 2 || status[keyA] == nil || status[keyB] == nil {
		t.Fatalf("registered Devin bindings = %#v", status)
	}
	if status[keyA].Disabled || !status[keyB].Disabled {
		t.Fatalf("disabled state A/B = %t/%t", status[keyA].Disabled, status[keyB].Disabled)
	}
	if credType, ok := quotaCredentialTypeForProvider("cognition"); !ok || credType != quota.TypeDevin {
		t.Fatalf("cognition type = %q/%t", credType, ok)
	}
	if credType, ok := quotaCredentialTypeForFileName("devin-shared.json"); !ok || credType != quota.TypeDevin {
		t.Fatalf("Devin filename type = %q/%t", credType, ok)
	}
}

func testAPIQuotaDevinStatus(daily, weekly uint64) []byte {
	planInfo := testAPIQuotaDevinBytes(nil, 2, []byte("Pro"))
	plan := testAPIQuotaDevinBytes(nil, 1, planInfo)
	plan = testAPIQuotaDevinVarint(plan, 14, daily)
	plan = testAPIQuotaDevinVarint(plan, 15, weekly)
	user := testAPIQuotaDevinBytes(nil, 3, []byte("user"))
	user = testAPIQuotaDevinBytes(user, 7, []byte("devin@example.com"))
	user = testAPIQuotaDevinBytes(user, 13, plan)
	return testAPIQuotaDevinBytes(nil, 1, user)
}

func testAPIQuotaDevinBytes(dst []byte, number protowire.Number, value []byte) []byte {
	dst = protowire.AppendTag(dst, number, protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}

func testAPIQuotaDevinVarint(dst []byte, number protowire.Number, value uint64) []byte {
	dst = protowire.AppendTag(dst, number, protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

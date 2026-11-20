package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const catalogCLISecret = "synthetic-catalog-cli-access-credential"

const catalogCLIBody = `{"models":[{"slug":"synthetic-model","description":"Available to ` +
	`business and enterprise ` +
	`subscriptions"}]}`

func TestCatalogExporterCommandPreservesPublicPlans(t *testing.T) {
	for _, plan := range []string{"enterprise", "business"} {
		t.Run(
			plan, func(t *testing.T) {
				dir := t.TempDir()
				authDir := filepath.Join(dir, "auths")
				if err := os.Mkdir(authDir, 0o700); err != nil {
					t.Fatal(err)
				}
				auth, err := json.Marshal(
					map[string]any{
						"type": "codex", "access_token": catalogCLISecret, "plan_type": plan,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(authDir, "synthetic.json"), auth, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("port: 8317\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCatalogExporterCommandHelper$")
				command.Dir = dir
				command.Env = []string{
					"CPA_CATALOG_CLI_HELPER=1", "CPA_CATALOG_CLI_DIR=" + dir,
					"HOME=" + dir, "PATH=/usr/bin:/bin", "GOMAXPROCS=2",
				}
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("catalog CLI failed: %v; %s", err, output)
				}
				if strings.Contains(string(output), catalogCLISecret) {
					t.Fatal("catalog CLI printed its synthetic credential")
				}
				raw, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
				if err != nil {
					t.Fatal(err)
				}
				if count, err := countModels(raw); err != nil || count != 1 {
					t.Fatalf("written catalog count=%d error=%v", count, err)
				}
				if !strings.Contains(string(raw), "business and enterprise") || strings.Contains(
					string(raw), catalogCLISecret,
				) {
					t.Fatalf("written catalog lost public plan text or contains a credential: %s", raw)
				}
			},
		)
	}
}

func TestCatalogExporterCommandHelper(t *testing.T) {
	if os.Getenv("CPA_CATALOG_CLI_HELPER") != "1" {
		return
	}
	dir := os.Getenv("CPA_CATALOG_CLI_DIR")
	var calls int
	http.DefaultTransport = roundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" ||
				request.URL.Path != "/backend-api/codex"+codexModelsPath {
				return nil, errors.New("unexpected isolated catalog request")
			}
			if request.Header.Get("Authorization") != "Bearer "+catalogCLISecret {
				return nil, errors.New("unexpected synthetic authorization")
			}
			return response(http.StatusOK, catalogCLIBody), nil
		},
	)
	flag.CommandLine = flag.NewFlagSet("catalog-cli-fixture", flag.ExitOnError)
	os.Args = []string{
		os.Args[0], "-config", filepath.Join(dir, "config.yaml"),
		"-auths-dir", filepath.Join(dir, "auths"), "-output", filepath.Join(dir, "catalog.json"),
	}
	main()
	if calls != 1 {
		t.Fatalf("catalog CLI used %d requests, want one isolated request", calls)
	}
}

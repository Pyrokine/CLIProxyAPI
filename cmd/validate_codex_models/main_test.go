package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCodexModelsCLI(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "validate_codex_models")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build validator: %v\n%s", err, output)
	}

	model := map[string]any{
		"slug": "gpt-5.5", "display_name": "Synthetic GPT", "description": "Synthetic catalog fixture",
		"base_instructions": "Synthetic instructions", "minimal_client_version": "0.124.0",
		"visibility": "list", "default_reasoning_level": "medium",
		"context_window": 272000, "max_context_window": 872000, "priority": 13,
		"supported_reasoning_levels": []map[string]string{{"effort": "medium"}},
	}
	encode := func(models ...map[string]any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{"models": models})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	valid := encode(model)
	duplicate := encode(model, model)
	delete(model, "base_instructions")
	missingField := encode(model)
	model["base_instructions"] = "Synthetic instructions"
	model["slug"] = "synthetic-model"
	missingDefault := encode(model)

	tests := []struct {
		name   string
		data   []byte
		args   []string
		exit   int
		stderr string
	}{
		{name: "valid", data: valid},
		{name: "malformed", data: []byte(`{"models":`), exit: 1, stderr: "decode Codex client model catalog"},
		{name: "empty", data: []byte(`{"models":[]}`), exit: 1, stderr: "has no models"},
		{name: "duplicate", data: duplicate, exit: 1, stderr: "duplicate slug"},
		{name: "missing-field", data: missingField, exit: 1, stderr: `field "base_instructions"`},
		{name: "missing-default", data: missingDefault, exit: 1, stderr: "missing default template"},
		{
			name: "missing-file", args: []string{"--file", filepath.Join(dir, "missing.json")}, exit: 1,
			stderr: "error: read",
		},
		{name: "directory", args: []string{"--file", dir}, exit: 1, stderr: "error: read"},
		{name: "required-file", args: []string{}, exit: 2, stderr: "error: --file is required"},
		{name: "blank-file", args: []string{"--file", " "}, exit: 2, stderr: "error: --file is required"},
		{name: "unknown-flag", args: []string{"--unknown"}, exit: 2, stderr: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				args := tt.args
				var inputPath string
				if tt.data != nil {
					inputPath = filepath.Join(dir, tt.name+".json")
					if err := os.WriteFile(inputPath, tt.data, 0o600); err != nil {
						t.Fatal(err)
					}
					args = []string{"--file", inputPath}
				}
				cmd := exec.Command(binary, args...)
				cmd.Dir = dir
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				exit := 0
				if err != nil {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) {
						t.Fatalf("start validator: %v", err)
					}
					exit = exitErr.ExitCode()
				}
				if exit != tt.exit {
					t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", exit, tt.exit, stdout.String(), stderr.String())
				}
				if tt.exit == 0 {
					if want := "Validated Codex client model catalog: " + inputPath + "\n"; stdout.String() != want ||
						stderr.Len() != 0 {
						t.Fatalf(
							"stdout=%q stderr=%q, want %q and empty stderr", stdout.String(), stderr.String(), want,
						)
					}
				} else if stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.stderr) {
					t.Fatalf(
						"stdout=%q stderr=%q, want empty stdout and stderr containing %q",
						stdout.String(), stderr.String(), tt.stderr,
					)
				}
				if tt.data != nil {
					after, err := os.ReadFile(inputPath)
					if err != nil || !bytes.Equal(after, tt.data) {
						t.Fatalf("input changed: err=%v", err)
					}
				}
			},
		)
	}
}

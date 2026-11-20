# validate_codex_models

Validate a local Codex client model catalog before distributing it

Run from the repository root

```sh
go run ./cmd/validate_codex_models --file ./codex_client_models.json
```

On success, stdout contains `Validated Codex client model catalog: <path>`
The command reads the supplied file and uses the registry's catalog validation rules

For scripts that need the validator's exit code directly, build the executable

```sh
go build -o /tmp/validate_codex_models ./cmd/validate_codex_models
/tmp/validate_codex_models --file ./codex_client_models.json
```

| Exit code | Meaning                                   | Output                    |
|-----------|-------------------------------------------|---------------------------|
| 0         | Valid catalog                             | Success message on stdout |
| 1         | File read or catalog validation failed    | Error on stderr           |
| 2         | Required `--file` missing or invalid flag | Error on stderr           |

`go run` reports a failing program's exit code in its output and returns its own process status

Run the offline CLI regression from the repository root

```sh
go test ./cmd/validate_codex_models
```

See [DESIGN.md](DESIGN.md) for validation and test coverage

package util

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var inPlaceSJSONTokens = []string{"ReplaceInPlace", "Optimistic"}

var inPlaceSJSONAllowlist = map[string]struct{}{}

func forEachSourceFile(t *testing.T, root string, visit func(rel string, data []byte)) {
	t.Helper()
	err := filepath.WalkDir(
		root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "vendor", "node_modules", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, errRel := filepath.Rel(root, path)
			if errRel != nil {
				return errRel
			}
			data, errRead := os.ReadFile(path)
			if errRead != nil {
				return errRead
			}
			visit(filepath.ToSlash(rel), data)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
}

func TestNoInPlaceSJSONWrites(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	forEachSourceFile(
		t, root, func(rel string, data []byte) {
			if _, allowed := inPlaceSJSONAllowlist[rel]; allowed {
				return
			}
			for _, token := range inPlaceSJSONTokens {
				if strings.Contains(string(data), token) {
					offenders = append(offenders, rel+" uses "+token)
				}
			}
		},
	)
	if len(offenders) > 0 {
		t.Fatalf(
			"in-place sjson writes would corrupt no-copy GJSON results that alias the same buffer:\n  %s\n"+
				"Either keep the default (allocating) sjson call, or prove no no-copy result derived from "+
				"that buffer is still alive and add the file to inPlaceSJSONAllowlist.",
			strings.Join(offenders, "\n  "),
		)
	}
}

var inPlaceByteWritePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bcopy\([a-zA-Z_][A-Za-z0-9_.]*\[`),
	regexp.MustCompile(`^\s*[a-zA-Z_][A-Za-z0-9_.]*\[[a-zA-Z0-9_]+] = 0\r?$`),
}

type reviewedInPlaceByteWrite struct {
	count  int
	reason string
}

var reviewedInPlaceByteWrites = map[string]reviewedInPlaceByteWrite{
	"internal/api/handlers/management/config_basic.go": {
		1,
		"copies validated key-file bytes into a local fixed-size key array",
	},
	"internal/client/codex/live/tcp_proxy.go": {
		1,
		"copies payload bytes into a freshly allocated WebSocket frame",
	},
	"internal/logging/requestid.go": {
		2,
		"copies digest and fallback prefix bytes into fresh local fixed-size request ID arrays",
	},
	"internal/runtime/executor/claude_signing.go": {
		2,
		"仅修改 bytes.Clone 的五位 CCH，GJSON 结果来自原始 body，hash view 由独立 append 构造且不返回别名",
	},
	"internal/usage/backup.go": {
		1,
		"copies a decoded fingerprint into a local fixed-size array",
	},
	"internal/usage/sqlite.go": {
		1,
		"copies a local SHA-256 digest into a local fixed-size fingerprint array",
	},
	"internal/usage/sqlite_query.go": {
		1,
		"copies a database blob into a newly built usage detail fingerprint",
	},
	"sdk/auth/manager.go": {
		1,
		"copies an independently hex-decoded digest into a fresh PersistedVersion SHA256 array",
	},
	"sdk/cliproxy/auth/conductor.go": {
		1,
		"copies a decoded persisted digest into a new version value",
	},
	"sdk/cliproxy/auth/durable_delete.go": {1, "copies a decoded journal digest into a new version value"},
}

func TestInPlaceByteWritesAreReviewed(t *testing.T) {
	for _, issue := range inPlaceByteWriteIssues(t, repoRoot(t), reviewedInPlaceByteWrites) {
		t.Error(issue)
	}
}

func inPlaceByteWriteIssues(t *testing.T, root string, reviewedWrites map[string]reviewedInPlaceByteWrite) []string {
	t.Helper()
	found := make(map[string][]string)
	forEachSourceFile(
		t, root, func(rel string, data []byte) {
			for line := range strings.SplitSeq(string(data), "\n") {
				for _, pattern := range inPlaceByteWritePatterns {
					if pattern.MatchString(line) {
						found[rel] = append(found[rel], strings.TrimSpace(line))
					}
				}
			}
		},
	)
	var issues []string
	for rel, lines := range found {
		reviewed, ok := reviewedWrites[rel]
		if !ok {
			issues = append(
				issues, fmt.Sprintf(
					"unreviewed in-place byte write in %s:\n  %s\nProve that no no-copy GJSON result derived "+
						"from that buffer is still alive, then record it in reviewedInPlaceByteWrites.",
					rel, strings.Join(lines, "\n  "),
				),
			)
			continue
		}
		if len(lines) != reviewed.count {
			issues = append(
				issues, fmt.Sprintf(
					"%s has %d in-place byte write(s), reviewed %d (%s):\n  %s",
					rel, len(lines), reviewed.count, reviewed.reason, strings.Join(lines, "\n  "),
				),
			)
		}
	}
	for rel := range reviewedWrites {
		if _, ok := found[rel]; !ok {
			issues = append(
				issues, fmt.Sprintf(
					"stale entry in reviewedInPlaceByteWrites: %s no longer contains an in-place byte write", rel,
				),
			)
		}
	}
	return issues
}

func TestInPlaceByteWriteGuardHandlesLineEndingsAndReviewBoundaries(t *testing.T) {
	for _, ending := range []struct {
		name, value string
	}{{"LF", "\n"}, {"CRLF", "\r\n"}} {
		t.Run(
			ending.name, func(t *testing.T) {
				root := t.TempDir()
				for _, file := range []string{
					".hidden/zero.go", "plain.go", "ignored_test.go", "ignored.txt",
					".git/ignored.go", "vendor/ignored.go", "node_modules/ignored.go", "testdata/ignored.go",
				} {
					path := filepath.Join(root, filepath.FromSlash(file))
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					line := "buffer[index] = 0"
					if file == "plain.go" {
						line = "copy(destination[:], source)"
					}
					if err := os.WriteFile(path, []byte(line+ending.value), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for _, check := range []struct {
					name   string
					review map[string]reviewedInPlaceByteWrite
					want   string
				}{
					{
						"reviewed", map[string]reviewedInPlaceByteWrite{
							".hidden/zero.go": {1, "fixture zero write"}, "plain.go": {1, "fixture copy"},
						}, "",
					},
					{
						"unknown", map[string]reviewedInPlaceByteWrite{
							"plain.go": {1, "fixture copy"},
						}, "unreviewed in-place byte write in .hidden/zero.go",
					},
					{
						"stale", map[string]reviewedInPlaceByteWrite{
							".hidden/zero.go": {1, "fixture zero write"}, "plain.go": {1, "fixture copy"},
							"removed.go": {1, "fixture removed write"},
						}, "stale entry in reviewedInPlaceByteWrites: removed.go",
					},
					{
						"count", map[string]reviewedInPlaceByteWrite{
							".hidden/zero.go": {2, "fixture incorrect count"}, "plain.go": {1, "fixture copy"},
						}, ".hidden/zero.go has 1 in-place byte write(s), reviewed 2",
					},
				} {
					t.Run(
						check.name, func(t *testing.T) {
							issues := inPlaceByteWriteIssues(t, root, check.review)
							if check.want == "" {
								if len(issues) != 0 {
									t.Fatalf("reviewed fixture rejected: %v", issues)
								}
							} else if len(issues) != 1 || !strings.Contains(issues[0], check.want) {
								t.Fatalf("issues = %v, want one containing %q", issues, check.want)
							}
						},
					)
				}
			},
		)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, errStat := os.Stat(filepath.Join(dir, "go.mod")); errStat == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

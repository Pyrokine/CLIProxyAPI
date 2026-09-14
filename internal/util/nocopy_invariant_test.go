package util

import (
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
				"Either keep the default (allocating) sjson call, or prove no no-copy result derived from that buffer is still alive and add the file to inPlaceSJSONAllowlist.",
			strings.Join(offenders, "\n  "),
		)
	}
}

var inPlaceByteWritePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bcopy\([a-zA-Z_][A-Za-z0-9_.]*\[`),
	regexp.MustCompile(`^\s*[a-zA-Z_][A-Za-z0-9_.]*\[[a-zA-Z0-9_]+] = 0$`),
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
	"sdk/cliproxy/auth/conductor.go": {
		1,
		"copies a decoded persisted digest into a new version value",
	},
	"sdk/cliproxy/auth/durable_delete.go": {1, "copies a decoded journal digest into a new version value"},
}

func TestInPlaceByteWritesAreReviewed(t *testing.T) {
	root := repoRoot(t)
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
	for rel, lines := range found {
		reviewed, ok := reviewedInPlaceByteWrites[rel]
		if !ok {
			t.Errorf(
				"unreviewed in-place byte write in %s:\n  %s\nProve that no no-copy GJSON result derived from that buffer is still alive, then record it in reviewedInPlaceByteWrites.",
				rel, strings.Join(lines, "\n  "),
			)
			continue
		}
		if len(lines) != reviewed.count {
			t.Errorf(
				"%s has %d in-place byte write(s), reviewed %d (%s):\n  %s",
				rel, len(lines), reviewed.count, reviewed.reason, strings.Join(lines, "\n  "),
			)
		}
	}
	for rel := range reviewedInPlaceByteWrites {
		if _, ok := found[rel]; !ok {
			t.Errorf("stale entry in reviewedInPlaceByteWrites: %s no longer contains an in-place byte write", rel)
		}
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

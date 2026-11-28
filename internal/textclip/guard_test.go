package textclip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRuneBoundaryCutsLiveHere pins the point of this package: a string cut to
// a byte budget on a UTF-8 boundary must be owned here so the ~20 historical
// copies cannot drift apart (one of them byte-sliced multi-byte characters).
//
// The allowlist holds byte-slice windowing that deliberately avoids a string
// round-trip: role-file frontmatter, a tool's raw output buffer, and a search
// snippet window. Every other production file must delegate to textclip.
func TestRuneBoundaryCutsLiveHere(t *testing.T) {
	allowed := map[string]string{
		"internal/textclip/textclip.go":       "the owner",
		"internal/agent/subagent/role_def.go": "byte-slice frontmatter window",
		"internal/tools/result.go":            "raw output byte buffer",
		"internal/tools/find.go":              "byte-slice search snippet",
	}
	skipDirs := map[string]bool{
		".git": true, ".yaah": true, ".beads": true, ".dolt": true,
		".scratch": true, ".claude": true, ".ghost": true, ".qwen": true,
		".hermes": true, ".kilo": true, ".serena": true, ".entire": true,
		"dist": true, "node_modules": true, "worktrees": true,
	}

	root := filepath.Clean(filepath.Join("..", ".."))
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), "utf8.RuneStart") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if reason, ok := allowed[rel]; ok {
			t.Logf("%s: allowed (%s)", rel, reason)
			return nil
		}
		offenders = append(offenders, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("utf8.RuneStart cut loops outside textclip: %v; delegate to internal/textclip instead", offenders)
	}
}

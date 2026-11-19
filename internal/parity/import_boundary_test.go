package parity

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoInternalToolsBackEdges pins the package-family boundary the
// architecture review flagged. The engine (the loop and its pipeline) and the
// built-in tool surface are wired together by the composition root, not by
// importing each other:
//
//   - internal/tools must not import internal/agent (Stage 2);
//   - the loop and its middleware pipeline must not import internal/tools
//     (Stage 3).
//
// internal/agent/runner is deliberately exempt: it is the composition layer
// that actually constructs the concrete built-in tools. Test files are exempt
// on both sides — they may use the real registry and role/service fixtures.
func TestNoInternalToolsBackEdges(t *testing.T) {
	prefix := "github.com/goatherdai/yaah/internal/"
	cases := []struct {
		glob   string
		banned string
		why    string
	}{
		{"../../internal/tools/*.go", prefix + "agent", "tools must not import the agent family"},
		{"../../internal/agent/*.go", prefix + "tools", "the loop must not import the tool surface"},
		{"../../internal/agent/pipeline/*.go", prefix + "tools", "the pipeline must not import the tool surface"},
	}
	for _, tc := range cases {
		files, err := filepath.Glob(tc.glob)
		if err != nil {
			t.Fatalf("list %s: %v", tc.glob, err)
		}
		if len(files) == 0 {
			t.Fatalf("no files matched %s", tc.glob)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			for _, imp := range f.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("unquote import in %s: %v", name, err)
				}
				if path == tc.banned || strings.HasPrefix(path, tc.banned+"/") {
					t.Errorf("%s imports %s; %s", name, path, tc.why)
				}
			}
		}
	}
}

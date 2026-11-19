package parity

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestToolsDoesNotImportAgentFamily pins the package-family boundary the
// architecture review flagged: internal/tools supplies the engine's tool
// surface, so it must not import internal/agent (or any subpackage) back.
// The composition root wires the two together instead. Test files are exempt
// — they may use the real role registry and supervision service as fixtures.
func TestToolsDoesNotImportAgentFamily(t *testing.T) {
	files, err := filepath.Glob("../../internal/tools/*.go")
	if err != nil {
		t.Fatalf("list tools sources: %v", err)
	}
	const prefix = "github.com/goatherdai/yaah/internal/agent"
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
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				t.Errorf("%s imports %s; tools must not import the agent family", name, path)
			}
		}
	}
}

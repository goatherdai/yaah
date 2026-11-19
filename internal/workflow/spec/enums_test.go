package spec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// declaredEnumValues parses this package's non-test sources and returns the
// string literals of every typed constant, keyed by the declared type name.
// It is the declaration side of the registry invariant: enumRegistry is also
// hand-written, so a new Kind*/Policy*/... constant that is not added to the
// registry would be invisible to the decoder — and to the resolved-graph
// parity test that reads EnumValues.
func declaredEnumValues(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list spec sources: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typ, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s in %s: %v", lit.Value, name, err)
					}
					out[typ.Name] = append(out[typ.Name], value)
				}
			}
		}
	}
	return out
}

// TestEnumRegistryMatchesDeclaredConstants closes the remaining hole in the
// spec/ir enum parity guard: it asserts that the hand-written enumRegistry
// names exactly the constants the source declares for every registered type.
// A new spec.Kind* constant that is not registered now fails here rather than
// silently escaping both the decoder and the resolved-graph conversions.
func TestEnumRegistryMatchesDeclaredConstants(t *testing.T) {
	declared := declaredEnumValues(t)
	for typ, registered := range enumRegistry {
		name := typ.Name()
		values, ok := declared[name]
		if !ok {
			t.Errorf("enum type %s is registered but declares no string constants", name)
			continue
		}
		a := append([]string(nil), values...)
		b := append([]string(nil), registered...)
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Errorf("%s: declared constants %v disagree with enumRegistry %v", name, values, registered)
		}
	}
}

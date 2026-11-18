package ir

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/goatherdai/yaah/internal/workflow/spec"
)

// mirror pairs an authoring enum with the converter that maps it onto the
// resolved graph's parallel enum. The test below drives every converter from
// the spec registry, so adding an enum value on the authoring side without a
// mapping on the resolved side fails CI instead of failing at run time.
type mirror struct {
	name     string
	specZero any
	convert  func(string) (string, error)
}

func TestConvertersMirrorSpecRegistry(t *testing.T) {
	mirrors := []mirror{
		{"node kind", spec.NodeKind(""), func(s string) (string, error) {
			v, err := convertKind(spec.NodeKind(s))
			return string(v), err
		}},
		{"lane", spec.Lane(""), func(s string) (string, error) {
			v, err := convertLane(spec.Lane(s))
			return string(v), err
		}},
		{"network mode", spec.NetworkMode(""), func(s string) (string, error) {
			v, err := convertNetwork(spec.NetworkMode(s))
			return string(v), err
		}},
		{"failure policy", spec.FailurePolicy(""), func(s string) (string, error) {
			v, err := convertFailurePolicy(spec.FailurePolicy(s))
			return string(v), err
		}},
		{"item failure", spec.ItemFailure(""), func(s string) (string, error) {
			v, err := convertItemFailure(spec.ItemFailure(s))
			return string(v), err
		}},
		{"join", spec.Join(""), func(s string) (string, error) {
			v, err := convertJoin(spec.Join(s))
			return string(v), err
		}},
		{"settle action", spec.SettleAction(""), func(s string) (string, error) {
			v, err := convertSettleAction(spec.SettleAction(s))
			return string(v), err
		}},
		{"conflict policy", spec.ConflictPolicy(""), func(s string) (string, error) {
			v, err := convertConflictPolicy(spec.ConflictPolicy(s))
			return string(v), err
		}},
		{"empty policy", spec.EmptyPolicy(""), func(s string) (string, error) {
			v, err := convertEmptyPolicy(spec.EmptyPolicy(s))
			return string(v), err
		}},
		{"settle order", spec.SettleOrder(""), func(s string) (string, error) {
			v, err := convertSettleOrder(spec.SettleOrder(s))
			return string(v), err
		}},
		{"aggregate strategy", spec.AggregateStrategy(""), func(s string) (string, error) {
			v, err := convertAggregateStrategy(spec.AggregateStrategy(s))
			return string(v), err
		}},
		{"output type", spec.OutputType(""), func(s string) (string, error) {
			v, err := convertOutputType(spec.OutputType(s))
			return string(v), err
		}},
		{"param type", spec.ParamType(""), func(s string) (string, error) {
			v, err := convertParamType(spec.ParamType(s))
			return string(v), err
		}},
		{"grant access", spec.GrantAccess(""), func(s string) (string, error) {
			v, err := convertGrantAccess(spec.GrantAccess(s))
			return string(v), err
		}},
	}

	for _, m := range mirrors {
		t.Run(m.name, func(t *testing.T) {
			values := spec.EnumValues(m.specZero)
			if len(values) == 0 {
				t.Fatalf("spec.EnumValues(%T) is empty — wrong type or unregistered enum", m.specZero)
			}
			seen := make(map[string]bool, len(values))
			for _, v := range values {
				got, err := m.convert(v)
				if err != nil {
					t.Errorf("authoring value %q has no resolved mapping: %v", v, err)
					continue
				}
				if got != v {
					t.Errorf("authoring value %q maps to resolved %q; the resolved enum must mirror it", v, got)
				}
				seen[got] = true
			}
			if len(seen) != len(values) {
				t.Errorf("%d authoring values mapped to %d distinct resolved values", len(values), len(seen))
			}
			if _, err := m.convert("__not_a_declared_value__"); err == nil {
				t.Error("converter accepted an unknown value")
			}
		})
	}
}

// TestPolicySplitMirrorsSpecRegistry covers the one enum that is NOT 1:1:
// spec.Policy is the shared authoring key whose values split into the
// select and settle halves of the resolved graph. Every authoring value must
// land in exactly one half, keeping the same string, and neither half may
// accept a value from the other.
func TestPolicySplitMirrorsSpecRegistry(t *testing.T) {
	values := spec.EnumValues(spec.Policy(""))
	if len(values) == 0 {
		t.Fatal("spec.EnumValues(spec.Policy(\"\")) is empty")
	}
	selectCount, settleCount := 0, 0
	for _, v := range values {
		sel, serr := convertSelectPolicy(spec.Policy(v))
		set, seterr := convertSettlePolicy(spec.Policy(v))
		if (serr == nil) == (seterr == nil) {
			t.Errorf("policy %q mapped to both halves or neither", v)
			continue
		}
		got := string(sel)
		if serr != nil {
			got = string(set)
			settleCount++
		} else {
			selectCount++
		}
		if got != v {
			t.Errorf("policy %q mapped to resolved %q", v, got)
		}
	}
	if selectCount != 3 || settleCount != 2 {
		t.Errorf("split produced %d select and %d settle values, want 3 and 2", selectCount, settleCount)
	}
	for _, v := range []string{string(SelectJudge), string(SelectFirstSuccess), string(SelectSmallestDiff)} {
		if _, err := convertSettlePolicy(spec.Policy(v)); err == nil {
			t.Errorf("settle converter accepted select policy %q", v)
		}
	}
	for _, v := range []string{string(SettlePathDisjoint), string(SettleThreeWay)} {
		if _, err := convertSelectPolicy(spec.Policy(v)); err == nil {
			t.Errorf("select converter accepted settle policy %q", v)
		}
	}
}

// declaredOnAbortValues parses graph.go for typed OnAbort constants, so the
// resolved side of the parity check enumerates the actual declarations rather
// than a second hand-typed list that could drift from them.
func declaredOnAbortValues(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "graph.go", nil, 0)
	if err != nil {
		t.Fatalf("parse graph.go: %v", err)
	}
	var out []string
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
			if !ok || typ.Name != "OnAbort" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, value)
			}
		}
	}
	return out
}

// TestAbortPolicyMirrorsSpecRegistry covers the enum that resolution casts
// directly rather than routing through a converter: the authoring values and
// the resolved declarations must name the same set in both directions.
func TestAbortPolicyMirrorsSpecRegistry(t *testing.T) {
	values := spec.EnumValues(spec.OnAbortPolicy(""))
	resolved := declaredOnAbortValues(t)
	if len(values) == 0 || len(resolved) == 0 {
		t.Fatalf("empty enum: spec=%v resolved=%v", values, resolved)
	}
	a := append([]string(nil), values...)
	b := append([]string(nil), resolved...)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		t.Errorf("abort policies disagree: spec=%v resolved=%v", values, resolved)
	}
}

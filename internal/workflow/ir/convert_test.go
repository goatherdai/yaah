package ir

import (
	"slices"
	"testing"

	"github.com/goatherdai/yaah/internal/workflow/spec"
)

// Compile-time pins: the one-to-one resolved enums are aliases of the
// authoring types, not parallel declarations. If one is ever split back into
// a distinct type, these assignments stop compiling.
var (
	_ Kind              = spec.KindDelegate
	_ Lane              = spec.LaneReport
	_ NetworkMode       = spec.NetworkAllowAll
	_ FailurePolicy     = spec.OnFailureAbort
	_ ItemFailure       = spec.OnItemAbort
	_ Join              = spec.JoinAll
	_ SettleAction      = spec.SettleSelect
	_ ConflictPolicy    = spec.OnConflictStop
	_ EmptyPolicy       = spec.OnEmptyPass
	_ SettleOrder       = spec.OrderSmallestDiffFirst
	_ AggregateStrategy = spec.AggregateConcat
	_ OutputType        = spec.OutputReport
	_ ParamType         = spec.ParamString
	_ GrantAccess       = spec.AccessReadOnly
	_ OnAbort           = spec.OnAbortHalt
)

// TestAliasedEnumsAcceptSpecRegistry pins that every value the decoder
// accepts is also accepted on the resolved side, and that an unknown value is
// still rejected for hand-built documents. With aliases there is no
// conversion table to drift; this checks the single shared validator.
func TestAliasedEnumsAcceptSpecRegistry(t *testing.T) {
	cases := []struct {
		name  string
		zero  any
		check func(string) bool
	}{
		{"node kind", spec.NodeKind(""), func(s string) bool { return validateEnum(spec.NodeKind(s)) }},
		{"lane", spec.Lane(""), func(s string) bool { return validateEnum(spec.Lane(s)) }},
		{"network mode", spec.NetworkMode(""), func(s string) bool { return validateEnum(spec.NetworkMode(s)) }},
		{"failure policy", spec.FailurePolicy(""), func(s string) bool { return validateEnum(spec.FailurePolicy(s)) }},
		{"item failure", spec.ItemFailure(""), func(s string) bool { return validateEnum(spec.ItemFailure(s)) }},
		{"join", spec.Join(""), func(s string) bool { return validateEnum(spec.Join(s)) }},
		{"settle action", spec.SettleAction(""), func(s string) bool { return validateEnum(spec.SettleAction(s)) }},
		{"conflict policy", spec.ConflictPolicy(""), func(s string) bool { return validateEnum(spec.ConflictPolicy(s)) }},
		{"empty policy", spec.EmptyPolicy(""), func(s string) bool { return validateEnum(spec.EmptyPolicy(s)) }},
		{"settle order", spec.SettleOrder(""), func(s string) bool { return validateEnum(spec.SettleOrder(s)) }},
		{"aggregate strategy", spec.AggregateStrategy(""), func(s string) bool { return validateEnum(spec.AggregateStrategy(s)) }},
		{"output type", spec.OutputType(""), func(s string) bool { return validateEnum(spec.OutputType(s)) }},
		{"param type", spec.ParamType(""), func(s string) bool { return validateEnum(spec.ParamType(s)) }},
		{"grant access", spec.GrantAccess(""), func(s string) bool { return validateEnum(spec.GrantAccess(s)) }},
		{"abort policy", spec.OnAbortPolicy(""), func(s string) bool { return validateEnum(spec.OnAbortPolicy(s)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := spec.EnumValues(c.zero)
			if len(values) == 0 {
				t.Fatalf("spec.EnumValues(%T) is empty", c.zero)
			}
			for _, v := range values {
				if !c.check(v) {
					t.Errorf("registered authoring value %q rejected on the resolved side", v)
				}
			}
			if c.check("__not_a_declared_value__") {
				t.Error("unknown value accepted")
			}
		})
	}
}

// TestPolicySplitMirrorsSpecRegistry covers the one enum that is NOT 1:1:
// spec.Policy is the shared authoring key whose values split into select and
// settle halves. Every authoring value must land in exactly one half.
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

// TestAbortConstantsMatchSpec pins the aliased abort constants by value.
func TestAbortConstantsMatchSpec(t *testing.T) {
	want := spec.EnumValues(spec.OnAbortPolicy(""))
	got := []string{string(AbortHalt), string(AbortReleaseAll), string(AbortDiscardAll)}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("abort constants %v disagree with spec registry %v", got, want)
	}
}

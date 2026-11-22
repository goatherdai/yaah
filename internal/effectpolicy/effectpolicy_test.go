package effectpolicy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sampleEffects are the fixtures most tests share: a handful of observations
// that differ by kind and by field shape.
func sampleEffects() map[string]Effect {
	return map[string]Effect{
		"write": {
			Kind: "write",
			Fields: map[string]any{
				"path":    "/tmp/a.txt",
				"size":    12,
				"existed": nil,
				"tags":    []any{"draft", "review"},
			},
		},
		"write-src": {
			Kind:   "write",
			Fields: map[string]any{"path": "/src/main.go", "size": 40},
		},
		"delete": {
			Kind:   "delete",
			Fields: map[string]any{"path": "/tmp/a.txt"},
		},
		"bash": {
			Kind:   "bash",
			Fields: map[string]any{"command": "rm -rf /tmp/a.txt"},
		},
	}
}

func TestEffectField(t *testing.T) {
	t.Run("kind is a reserved field", func(t *testing.T) {
		e := Effect{Kind: "write"}
		v, ok := e.Field(KindField)
		if !ok || v != "write" {
			t.Fatalf("Field(%q) = %v, %v; want write, true", KindField, v, ok)
		}
	})
	t.Run("present field", func(t *testing.T) {
		e := sampleEffects()["write"]
		if v, ok := e.Field("path"); !ok || v != "/tmp/a.txt" {
			t.Fatalf("Field(path) = %v, %v; want /tmp/a.txt, true", v, ok)
		}
	})
	t.Run("absent field", func(t *testing.T) {
		e := sampleEffects()["write"]
		if v, ok := e.Field("nope"); ok || v != nil {
			t.Fatalf("Field(nope) = %v, %v; want nil, false", v, ok)
		}
	})
	t.Run("String is stable", func(t *testing.T) {
		e := Effect{Kind: "write", Fields: map[string]any{"b": 1, "a": 2}}
		const want = `effect(kind="write" a=2 b=1)`
		if got := e.String(); got != want {
			t.Fatalf("String() = %s; want %s", got, want)
		}
	})
}

func TestFieldOperators(t *testing.T) {
	effects := sampleEffects()
	write := effects["write"]

	cases := []struct {
		name string
		m    Match
		want bool
	}{
		{"eq hit", Field("path", Eq, "/tmp/a.txt"), true},
		{"eq miss", Field("path", Eq, "/x"), false},
		{"eq across numeric types", Field("size", Eq, 12.0), true},
		{"ne hit", Field("path", Ne, "/x"), true},
		{"ne equal", Field("path", Ne, "/tmp/a.txt"), false},
		{"ne on absent field", Field("nope", Ne, "x"), false},
		{"lt hit", Field("size", Lt, 20), true},
		{"lt boundary", Field("size", Lt, 12), false},
		{"lte boundary", Field("size", Lte, 12), true},
		{"gt hit", Field("size", Gt, 11), true},
		{"gt miss", Field("size", Gt, 12), false},
		{"gte boundary", Field("size", Gte, 12), true},
		{"gt lexicographic", Field("path", Gt, "/tmp/a"), true},
		{"gt mismatched types", Field("path", Gt, 5), false},
		{"lt mismatched types", Field("size", Lt, "big"), false},
		{"in hit", Field("path", In, []any{"/tmp/a.txt", "/tmp/b.txt"}), true},
		{"in miss", Field("path", In, []any{"/tmp/b.txt"}), false},
		{"not_in hit", Field("path", NotIn, []any{"/tmp/b.txt"}), true},
		{"not_in miss", Field("path", NotIn, []any{"/tmp/a.txt"}), false},
		{"is_none on nil value", Field("existed", IsNone, nil), true},
		{"is_none on present value", Field("path", IsNone, nil), false},
		{"is_none on absent field", Field("nope", IsNone, nil), false},
		{"is_not_none on present value", Field("path", IsNotNone, nil), true},
		{"is_not_none on nil value", Field("existed", IsNotNone, nil), false},
		{"is_not_none on absent field", Field("nope", IsNotNone, nil), false},
		{"startswith hit", Field("path", StartsWith, "/tmp/"), true},
		{"startswith miss", Field("path", StartsWith, "/src/"), false},
		{"endswith hit", Field("path", EndsWith, ".txt"), true},
		{"endswith miss", Field("path", EndsWith, ".go"), false},
		{"contains substring", Field("path", Contains, "tmp"), true},
		{"contains element", Field("tags", Contains, "draft"), true},
		{"contains element miss", Field("tags", Contains, "final"), false},
		{"contains wrong type", Field("size", Contains, "tmp"), false},
		{"matches hit", Field("path", Matches, `^/tmp/.*\.txt$`), true},
		{"matches miss", Field("path", Matches, `^/src/`), false},
		{"matches on non-string", Field("size", Matches, `1`), false},
		{"all", All(), true},
		{"nothing", Nothing(), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.Matches(write); got != tc.want {
				t.Fatalf("%s.Matches(%s) = %v; want %v", tc.m, write, got, tc.want)
			}
		})
	}
}

func TestExact(t *testing.T) {
	effects := sampleEffects()
	for name, tc := range map[string]struct {
		m    Match
		want bool
	}{
		"write":  {Exact("write"), true},
		"delete": {Exact("delete"), false},
		"bash":   {Exact("bash"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.m.Matches(effects["write"]); got != tc.want {
				t.Fatalf("%s.Matches(write) = %v; want %v", tc.m, got, tc.want)
			}
		})
	}
}

func TestFieldMalformedArgsMatchNothing(t *testing.T) {
	cases := []struct {
		name string
		m    Match
	}{
		{"uncompilable regexp", Field("path", Matches, "(")},
		{"in over a string", Field("path", In, "/tmp/a.txt")},
		{"in over nil", Field("path", In, nil)},
		{"startswith over an int", Field("size", StartsWith, 12)},
		{"endswith over an int", Field("size", EndsWith, 12)},
		{"unknown operator", Field("path", Op("bogus"), "x")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.m.IsEmpty() {
				t.Fatalf("%s should match nothing", tc.m)
			}
			for _, e := range sampleEffects() {
				if tc.m.Matches(e) {
					t.Fatalf("%s matched %s; want no match", tc.m, e)
				}
			}
		})
	}
}

// TestCombinators is the truth table for And, Or, Not, and Difference over a
// three-effect probe space.
func TestCombinators(t *testing.T) {
	effects := sampleEffects()
	probes := []Effect{effects["write"], effects["delete"], effects["bash"]}

	writeM := Exact("write")
	deleteM := Exact("delete")
	both := writeM.Or(deleteM)

	cases := []struct {
		name string
		m    Match
		want []bool // write, delete, bash
	}{
		{"and", writeM.And(both), []bool{true, false, false}},
		{"and disjoint", writeM.And(deleteM), []bool{false, false, false}},
		{"or", both, []bool{true, true, false}},
		{"not", writeM.Not(), []bool{false, true, true}},
		{"not all", All().Not(), []bool{false, false, false}},
		{"not nothing", Nothing().Not(), []bool{true, true, true}},
		{"double negation", writeM.Not().Not(), []bool{true, false, false}},
		{"difference", both.Difference(writeM), []bool{false, true, false}},
		{"difference clears", both.Difference(both), []bool{false, false, false}},
		{"and all", writeM.And(All()), []bool{true, false, false}},
		{"or nothing", writeM.Or(Nothing()), []bool{true, false, false}},
		{"and nothing", writeM.And(Nothing()), []bool{false, false, false}},
		{"or all", writeM.Or(All()), []bool{true, true, true}},
		{"not a conjunction", writeM.And(Field("path", StartsWith, "/tmp")).Not(),
			[]bool{false, true, true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, e := range probes {
				if got := tc.m.Matches(e); got != tc.want[i] {
					t.Fatalf("%s.Matches(%s) = %v; want %v", tc.m, e, got, tc.want[i])
				}
			}
		})
	}
}

// writeSurface is the reference plan used across the plan tests: writes are
// allowed everywhere except under /tmp, and bash is denied outright.
func writeSurface() Plan {
	return Plan{}.
		AllowOnly(Exact("write")).
		DenyKind(Exact("write").And(Field("path", StartsWith, "/tmp"))).
		DenyTool("bash")
}

func TestEffectiveSurface(t *testing.T) {
	effects := sampleEffects()

	allowWrite := Plan{}.AllowOnly(Exact("write"))
	srcOnly := Plan{}.AllowOnly(Exact("write")).AllowOnly(Field("path", StartsWith, "/src"))
	denyBash := Plan{}.DenyTool("bash")
	contradiction := Plan{}.AllowOnly(Exact("write")).DenyKind(Exact("write"))

	cases := []struct {
		name string
		plan Plan
		// want is keyed by sample effect name.
		want map[string]bool
	}{
		{
			name: "allow only",
			plan: allowWrite,
			want: map[string]bool{"write": true, "write-src": true, "delete": false, "bash": false},
		},
		{
			name: "deny only",
			plan: denyBash,
			want: map[string]bool{"write": true, "write-src": true, "delete": true, "bash": false},
		},
		{
			name: "allow and deny",
			plan: writeSurface(),
			want: map[string]bool{"write": false, "write-src": true, "delete": false, "bash": false},
		},
		{
			name: "stacked allows narrow",
			plan: srcOnly,
			want: map[string]bool{"write": false, "write-src": true, "delete": false, "bash": false},
		},
		{
			name: "allow and deny contradict",
			plan: contradiction,
			want: map[string]bool{"write": false, "write-src": false, "delete": false, "bash": false},
		},
		{
			name: "zero plan permits everything",
			plan: Plan{},
			want: map[string]bool{"write": true, "write-src": true, "delete": true, "bash": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, want := range tc.want {
				if got := tc.plan.Permits(effects[name]); got != want {
					t.Fatalf("Permits(%s) = %v; want %v", name, got, want)
				}
			}
		})
	}
}

func TestSubsetOf(t *testing.T) {
	writeM := Exact("write")

	tests := []struct {
		name string
		m    Match
		o    Match
		want bool
		note string
	}{
		{
			name: "identical",
			m:    writeM, o: writeM, want: true,
		},
		{
			name: "narrower conjunction",
			m:    writeM.And(Field("path", StartsWith, "/tmp")), o: writeM, want: true,
		},
		{
			name: "narrower disjunction member",
			m:    writeM, o: writeM.Or(Exact("delete")), want: true,
		},
		{
			name: "wider is not a subset",
			m:    writeM.Or(Exact("delete")), o: writeM, want: false,
		},
		{
			name: "empty is a subset of everything",
			m:    Nothing(), o: writeM, want: true,
		},
		{
			name: "everything is not a subset of a kind",
			m:    All(), o: writeM, want: false,
		},
		{
			name: "everything is a subset of everything",
			m:    All(), o: All(), want: true,
		},
		{
			name: "bounds proven",
			m:    Field("size", Gt, 5), o: Field("size", Gt, 3), want: true,
		},
		{
			name: "set proven",
			m:    Field("path", In, []any{"/a"}), o: Field("path", In, []any{"/a", "/b"}), want: true,
		},
		{
			name: "prefix proven",
			m:    Field("path", StartsWith, "/tmp/x"), o: Field("path", StartsWith, "/tmp/"), want: true,
		},
		{
			name: "negated deny weakens",
			m:    writeM.Difference(Field("path", StartsWith, "/tmp")), o: All(), want: true,
		},
		{
			// Integrality is not modelled: over integers every x > 5 also
			// satisfies x >= 6, but the matcher cannot prove it.
			name: "conservative false on integrality",
			m:    Field("size", Gt, 5), o: Field("size", Gte, 6), want: false,
			note: "x > 5 implies x >= 6 only on an integer domain",
		},
		{
			// The union is everything, but neither disjunct alone covers the
			// premise, so the syntactic check cannot see it.
			name: "conservative false on a split union",
			m:    Field("size", Gt, 0),
			o:    Field("kind_hint", Eq, 1).Or(Field("kind_hint", Eq, 1).Not()),
			want: false,
			note: "y == 1 or y != 1 is everything, but no single clause proves it",
		},
		{
			name: "range gap is not covered",
			m:    Field("size", Gt, 5),
			o:    Field("size", Lt, 0).Or(Field("size", Gt, 100)),
			want: false,
			note: "the premise neither implies x < 0 nor x > 100",
		},
		{
			name: "substring proven",
			m:    Field("path", Contains, "/tmp/sub"), o: Field("path", Contains, "tmp"), want: true,
		},
		{
			name: "suffix proven",
			m:    Field("path", EndsWith, ".txt"), o: Field("path", EndsWith, "txt"), want: true,
		},
		{
			name: "nil pin implies any other test",
			m:    Field("existed", IsNone, nil), o: Field("existed", Ne, 1), want: true,
		},
		{
			name: "excluding nil implies presence",
			m:    Field("x", Ne, nil), o: Field("x", IsNotNone, nil), want: true,
		},
		{
			name: "excluded set narrows",
			m:    Field("path", NotIn, []any{"/a", "/b"}), o: Field("path", NotIn, []any{"/a"}), want: true,
		},
		{
			name: "excluded set does not widen",
			m:    Field("path", NotIn, []any{"/a"}), o: Field("path", NotIn, []any{"/a", "/b"}), want: false,
		},
		{
			name: "regexp implies presence",
			m:    Field("path", Matches, "^/a"), o: Field("path", IsNotNone, nil), want: true,
		},
		{
			name: "upper bound proven",
			m:    Field("size", Lt, 3), o: Field("size", Lt, 10), want: true,
		},
		{
			name: "upper bound crosses strictness",
			m:    Field("size", Lte, 3), o: Field("size", Lt, 4), want: true,
		},
		{
			name: "strict bound excludes its own value",
			m:    Field("size", Gt, 5), o: Field("size", Ne, 5), want: true,
		},
		{
			name: "membership implies inequality",
			m:    Field("path", In, []any{"/a"}), o: Field("path", Ne, "/b"), want: true,
		},
		{
			name: "membership implies excluded set",
			m:    Field("path", In, []any{"/a"}), o: Field("path", NotIn, []any{"/b"}), want: true,
		},
		{
			name: "different fields never entail",
			m:    Field("path", Eq, "/a"), o: Field("command", Eq, "ls"), want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.SubsetOf(tc.o); got != tc.want {
				t.Fatalf("%s.SubsetOf(%s) = %v; want %v (%s)", tc.m, tc.o, got, tc.want, tc.note)
			}
		})
	}
}

func TestEquivalentTo(t *testing.T) {
	p := Exact("write").And(Field("path", StartsWith, "/tmp"))
	q := Field("path", StartsWith, "/tmp").And(Exact("write"))

	tests := []struct {
		name string
		a    Match
		b    Match
		want bool
		note string
	}{
		{"identical", Exact("write"), Exact("write"), true, ""},
		{"conjunct order", p, q, true, ""},
		{"disjunct order", Exact("write").Or(Exact("delete")), Exact("delete").Or(Exact("write")), true, ""},
		{"all", All(), All(), true, ""},
		{"nothing", Nothing(), Nothing(), true, ""},
		{"empty set atom is nothing", Field("path", In, []any{}), Nothing(), true, ""},
		{"different kinds", Exact("write"), Exact("delete"), false, ""},
		{"all versus nothing", All(), Nothing(), false, ""},
		{
			// Both denote the same set; different atoms, so the syntactic
			// comparison declines to prove it.
			name: "conservative false on alternative spellings",
			a:    Field("path", Eq, "/a"),
			b:    Field("path", In, []any{"/a"}),
			want: false,
			note: "path == /a and path in [/a] are the same set",
		},
		{
			name: "conservative false on a tautology",
			a:    All(),
			b:    Field("x", Eq, 1).Or(Field("x", Eq, 1).Not()),
			want: false,
			note: "the right side is everything by excluded middle",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.EquivalentTo(tc.b); got != tc.want {
				t.Fatalf("%s.EquivalentTo(%s) = %v; want %v (%s)", tc.a, tc.b, got, tc.want, tc.note)
			}
			if got := tc.b.EquivalentTo(tc.a); got != tc.want {
				t.Fatalf("symmetry: %s.EquivalentTo(%s) = %v; want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestIsEmpty(t *testing.T) {
	tests := []struct {
		name string
		m    Match
		want bool
	}{
		{"nothing", Nothing(), true},
		{"all", All(), false},
		{"kind", Exact("write"), false},
		{"contradictory kinds", Exact("write").And(Exact("delete")), true},
		{"complement", Exact("write").Difference(Exact("write")), true},
		{"empty set", Field("path", In, []any{}), true},
		{"disjoint bounds", Field("size", Gt, 5).And(Field("size", Lt, 3)), true},
		{"touching bounds exclude the point", Field("size", Gt, 3).And(Field("size", Lte, 3)), true},
		{"overlapping bounds", Field("size", Gt, 1).And(Field("size", Lte, 3)), false},
		{"value violates a bound", Field("size", Eq, 1).And(Field("size", Gt, 3)), true},
		{"nil pin and a value pin", Field("size", IsNone, nil).And(Field("size", Eq, 1)), true},
		{"malformed field", Field("path", Matches, "("), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.IsEmpty(); got != tc.want {
				t.Fatalf("%s.IsEmpty() = %v; want %v", tc.m, got, tc.want)
			}
		})
	}
}

func TestCanonical(t *testing.T) {
	write := Exact("write")
	tmp := Field("path", StartsWith, "/tmp")

	cases := map[string]Match{
		"all":             All(),
		"nothing":         Nothing(),
		"atom":            write,
		"conjunction":     write.And(tmp),
		"disjunction":     write.Or(Exact("delete")),
		"negation":        write.Difference(tmp),
		"nested":          write.And(tmp.Not()).Or(Exact("delete").And(Field("size", Gt, 5))),
		"plan surface":    writeSurface().EffectiveSurface(),
		"contradiction":   write.And(write.Not()),
		"double negation": write.Not().Not(),
		"empty set atom":  Field("path", In, []any{}),
	}

	effects := sampleEffects()
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			once := m.Canonical()
			twice := once.Canonical()
			if once.String() != twice.String() {
				t.Fatalf("Canonical is not idempotent:\n once: %s\ntwice: %s", once, twice)
			}
			for effectName, e := range effects {
				if got, want := once.Matches(e), m.Matches(e); got != want {
					t.Fatalf("Canonical changed semantics on %s: got %v; want %v (%s)", effectName, got, want, m)
				}
			}
			if got, want := once.IsEmpty(), m.IsEmpty(); got != want {
				t.Fatalf("Canonical changed IsEmpty: got %v; want %v", got, want)
			}
		})
	}

	t.Run("conjunction is rendered in a stable order", func(t *testing.T) {
		a := Field("path", StartsWith, "/tmp")
		if got, want := a.And(write).Canonical().String(), write.And(a).Canonical().String(); got != want {
			t.Fatalf("canonical forms differ: %s versus %s", got, want)
		}
	})
}

func TestClauseBudget(t *testing.T) {
	// One clause more than the canonical form is allowed to carry.
	big := Field("x", Eq, 0)
	for i := 1; i <= maxClauses; i++ {
		big = big.Or(Field("x", Eq, i))
	}

	t.Run("matching stays exact", func(t *testing.T) {
		for _, tc := range []struct {
			e    Effect
			want bool
		}{
			{Effect{Fields: map[string]any{"x": 0}}, true},
			{Effect{Fields: map[string]any{"x": maxClauses}}, true},
			{Effect{Fields: map[string]any{"x": maxClauses + 1}}, false},
			{Effect{Fields: map[string]any{}}, false},
		} {
			if got := big.Matches(tc.e); got != tc.want {
				t.Fatalf("big.Matches(%s) = %v; want %v", tc.e, got, tc.want)
			}
		}
	})

	t.Run("canonical falls back to the matcher", func(t *testing.T) {
		if got, want := big.Canonical().String(), big.String(); got != want {
			t.Fatalf("Canonical() = %s; want the matcher unchanged, %s", got, want)
		}
	})

	t.Run("relations answer false without proof", func(t *testing.T) {
		if big.IsEmpty() {
			t.Fatal("IsEmpty() = true; a budget overflow must not claim emptiness")
		}
		// Self-relations are true set-theoretically, yet the overflowed matcher
		// cannot prove them: every answer collapses to the conservative false.
		if big.SubsetOf(big) {
			t.Fatal("SubsetOf(self) = true; a budget overflow must not claim entailment")
		}
		if big.EquivalentTo(big) {
			t.Fatal("EquivalentTo(self) = true; a budget overflow must not claim equivalence")
		}
		if big.SubsetOf(All()) {
			t.Fatal("SubsetOf(all()) = true; a budget overflow must not claim entailment")
		}
	})
}

func TestPlanSubsetOf(t *testing.T) {
	parent := Plan{}.AllowOnly(Exact("write"))
	narrow := Plan{}.AllowOnly(Exact("write").And(Field("path", StartsWith, "/src")))
	wider := Plan{}
	deniedBash := Plan{}.DenyTool("bash")
	deniedTmpWrite := Plan{}.AllowOnly(Exact("write")).
		DenyKind(Exact("write").And(Field("path", StartsWith, "/tmp")))

	tests := []struct {
		name  string
		child Plan
		root  Plan
		want  bool
	}{
		{"identical", parent, parent, true},
		{"narrower allow", narrow, parent, true},
		{"wider allow", wider, parent, false},
		{"child respects an unrelated deny", parent, deniedBash, true},
		{"child narrows inside a deny", deniedTmpWrite, deniedBash, true},
		{"child admits a denied kind", parent, deniedTmpWrite, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.child.SubsetOf(tc.root); got != tc.want {
				t.Fatalf("child %s .SubsetOf(%s) = %v; want %v", tc.child, tc.root, got, tc.want)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	effects := sampleEffects()
	parent := Plan{}.AllowOnly(Exact("write"))

	t.Run("Check permits", func(t *testing.T) {
		if err := parent.Check(effects["write-src"]); err != nil {
			t.Fatalf("Check(write-src) = %v; want nil", err)
		}
	})

	t.Run("Check rejects", func(t *testing.T) {
		err := parent.Check(effects["bash"])
		if !errors.Is(err, ErrEffectNotPermitted) {
			t.Fatalf("Check(bash) = %v; want ErrEffectNotPermitted", err)
		}
		if want := "effect not permitted by plan"; !strings.Contains(err.Error(), want) {
			t.Fatalf("Check(bash) message %q does not contain %q", err, want)
		}
	})

	t.Run("Conforms accepts a narrower child", func(t *testing.T) {
		child := Plan{}.AllowOnly(Exact("write").And(Field("path", StartsWith, "/src")))
		if err := Conforms(child, parent); err != nil {
			t.Fatalf("Conforms = %v; want nil", err)
		}
	})

	t.Run("Conforms rejects a wider child", func(t *testing.T) {
		if err := Conforms(Plan{}, parent); !errors.Is(err, ErrEffectSurfaceTooWide) {
			t.Fatalf("Conforms = %v; want ErrEffectSurfaceTooWide", err)
		}
	})

	t.Run("Conforms rejects an empty child surface", func(t *testing.T) {
		if err := Conforms(Plan{}.AllowOnly(Nothing()), parent); !errors.Is(err, ErrEffectSurfaceEmpty) {
			t.Fatalf("Conforms = %v; want ErrEffectSurfaceEmpty", err)
		}
	})

	t.Run("sentinels are distinct", func(t *testing.T) {
		if errors.Is(ErrEffectNotPermitted, ErrEffectSurfaceTooWide) ||
			errors.Is(ErrEffectNotPermitted, ErrEffectSurfaceEmpty) ||
			errors.Is(ErrEffectSurfaceTooWide, ErrEffectSurfaceEmpty) {
			t.Fatal("policy sentinels must be distinct")
		}
	})
}

func TestString(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"all", All().String(), "all()"},
		{"nothing", Nothing().String(), "nothing()"},
		{"kind", Exact("write").String(), `kind == "write"`},
		{"prefix", Field("path", StartsWith, "/tmp").String(), `path starts with "/tmp"`},
		{"regexp", Field("path", Matches, "^/a").String(), `path ~ "^/a"`},
		{"nil test", Field("existed", IsNone, nil).String(), "existed is null"},
		{"set", Field("path", In, []any{"/b", "/a"}).String(), `path in ["/a", "/b"]`},
		{"not", Exact("write").Not().String(), `not(kind == "write")`},
		{"and", Exact("write").And(Field("path", StartsWith, "/tmp")).String(),
			`and(kind == "write", path starts with "/tmp")`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %s; want %s", tc.got, tc.want)
			}
		})
	}
}

// TestNotInImpliesPresenceOnlyWithNil pins the excluded-set rule: an excluded
// set rules out the nil value only when it names nil, so only then does it
// prove presence. A set that does not name nil still admits a present field
// whose value is nil, which IsNotNone rejects.
func TestNotInImpliesPresenceOnlyWithNil(t *testing.T) {
	withoutNil := Field("x", NotIn, []any{"blocked"})
	withNil := Field("x", NotIn, []any{"blocked", nil})
	presence := Field("x", IsNotNone, nil)

	nilValue := Effect{Kind: "write", Fields: map[string]any{"x": nil}}
	blocked := Effect{Kind: "write", Fields: map[string]any{"x": "blocked"}}
	other := Effect{Kind: "write", Fields: map[string]any{"x": "ok"}}

	// Matches is the ground truth the proofs must respect.
	if !withoutNil.Matches(nilValue) {
		t.Fatal("x not in [blocked] must match a present nil value")
	}
	if presence.Matches(nilValue) {
		t.Fatal("x is not null must not match a present nil value")
	}
	if withNil.Matches(nilValue) {
		t.Fatal("x not in [blocked, nil] must not match a present nil value")
	}

	t.Run("nil-excluding set proves presence", func(t *testing.T) {
		if !withNil.SubsetOf(presence) {
			t.Fatal("x not in [blocked, nil] must prove x is not null")
		}
		child := Plan{}.AllowOnly(withNil)
		parent := Plan{}.AllowOnly(presence)
		if err := Conforms(child, parent); err != nil {
			t.Fatalf("Conforms = %v; want nil", err)
		}
	})

	t.Run("nil-admitting set does not", func(t *testing.T) {
		if withoutNil.SubsetOf(presence) {
			t.Fatal("x not in [blocked] admits a nil value, so it must not prove presence")
		}
		child := Plan{}.AllowOnly(withoutNil)
		parent := Plan{}.AllowOnly(presence)
		if err := Conforms(child, parent); !errors.Is(err, ErrEffectSurfaceTooWide) {
			t.Fatalf("Conforms = %v; want ErrEffectSurfaceTooWide", err)
		}
	})

	for _, e := range []Effect{other} {
		if !withNil.SubsetOf(presence) {
			t.Fatalf("subset with %s = false; want true", e)
		}
		if !withoutNil.Matches(e) || !presence.Matches(e) {
			t.Fatalf("%s: both matchers must accept a present non-nil value", e)
		}
	}
	if withoutNil.Matches(blocked) {
		t.Fatal("x not in [blocked] must reject the excluded value")
	}
	if !presence.Matches(blocked) {
		t.Fatal("x is not null must accept a present non-nil value")
	}
}

// TestLargeIntegersStayDistinct pins the comparison to exact integral values: a
// float64 conversion rounds two neighbouring large integers onto the same
// value, which would let one satisfy an equality policy written for the other.
func TestLargeIntegersStayDistinct(t *testing.T) {
	const maxUint64 = ^uint64(0)        // 18446744073709551615
	const prevUint64 = maxUint64 - 1    // 18446744073709551614
	const maxInt64 = uint64(1<<63 - 1)  // 9223372036854775807
	const prevInt64 = uint64(1<<63 - 2) // 9223372036854775806

	// The premise: float64 rounds each pair onto one value, which is exactly
	// what made the old comparison collapse them.
	if float64(maxUint64) != float64(prevUint64) {
		t.Fatalf("premise: float64(%d) and float64(%d) differ", prevUint64, maxUint64)
	}
	if float64(int64(maxInt64)) != float64(int64(prevInt64)) {
		t.Fatalf("premise: float64(%d) and float64(%d) differ", prevInt64, maxInt64)
	}

	cases := []struct {
		name   string
		m      Match
		fields map[string]any
		want   bool
	}{
		{"uint64 near max", Field("size", Eq, maxUint64), map[string]any{"size": prevUint64}, false},
		{"uint64 exact", Field("size", Eq, maxUint64), map[string]any{"size": maxUint64}, true},
		{"int64 near max", Field("size", Eq, int64(maxInt64)), map[string]any{"size": int64(prevInt64)}, false},
		{"int64 exact", Field("size", Eq, int64(maxInt64)), map[string]any{"size": int64(maxInt64)}, true},
		{"uint64 neighbour is below", Field("size", Gt, prevUint64), map[string]any{"size": maxUint64}, true},
		{"uint64 bound excludes itself", Field("size", Gt, maxUint64), map[string]any{"size": maxUint64}, false},
		{"uint64 neighbour is not above", Field("size", Lt, maxUint64), map[string]any{"size": prevUint64}, true},
		{"signed negative excludes unsigned", Field("size", Eq, int64(-1)), map[string]any{"size": maxUint64}, false},
		{"signed bound admits unsigned", Field("size", Gt, int64(-1)), map[string]any{"size": maxUint64}, true},
		{"unsigned bound keeps the negative below it", Field("size", Lt, uint64(0)), map[string]any{"size": int64(-1)}, true},
		{"signed negative is not above zero", Field("size", Gt, uint64(0)), map[string]any{"size": int64(-1)}, false},
		{"integer is not the rounded float", Field("size", Eq, maxUint64), map[string]any{"size": float64(maxUint64)}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Effect{Kind: "write", Fields: tc.fields}
			if got := tc.m.Matches(e); got != tc.want {
				t.Fatalf("%s.Matches(%s) = %v; want %v", tc.m, e, got, tc.want)
			}
		})
	}

	t.Run("subset proofs are not corrupted", func(t *testing.T) {
		narrow := Field("size", In, []any{prevUint64})
		wide := Field("size", In, []any{maxUint64})
		if narrow.SubsetOf(wide) {
			t.Fatal("a set holding one uint64 must not prove a set holding its neighbour")
		}
		if Field("size", Eq, maxUint64).SubsetOf(Field("size", Eq, prevUint64)) {
			t.Fatal("an equality on one uint64 must not prove an equality on its neighbour")
		}
		if Field("size", Eq, maxUint64).EquivalentTo(Field("size", Eq, prevUint64)) {
			t.Fatal("adjacent uint64 equality atoms are not equivalent")
		}
		if !Field("size", In, []any{maxUint64}).SubsetOf(Field("size", Eq, maxUint64)) {
			t.Fatal("a singleton set must prove the equality of its member")
		}
	})
}

// TestValueIdentityIsUnambiguous pins atom identity to a structural encoding: a
// rendering makes []string{"a b", "c"} and []string{"a", "b c"} look alike,
// while the runtime equality they stand for distinguishes them.
func TestValueIdentityIsUnambiguous(t *testing.T) {
	spaced := []string{"a b", "c"}
	apart := []string{"a", "b c"}

	// The premise: one rendering, two distinct values.
	if fmt.Sprint(spaced) != fmt.Sprint(apart) {
		t.Fatalf("premise: %q and %q render differently", fmt.Sprint(spaced), fmt.Sprint(apart))
	}

	eqSpaced := Field("tags", Eq, spaced)
	eqApart := Field("tags", Eq, apart)
	spacedEffect := Effect{Kind: "write", Fields: map[string]any{"tags": spaced}}
	apartEffect := Effect{Kind: "write", Fields: map[string]any{"tags": apart}}

	if !eqSpaced.Matches(spacedEffect) || eqSpaced.Matches(apartEffect) {
		t.Fatal("an equality atom must accept only its own value")
	}
	if !eqApart.Matches(apartEffect) || eqApart.Matches(spacedEffect) {
		t.Fatal("an equality atom must accept only its own value")
	}
	if eqSpaced.SubsetOf(eqApart) || eqApart.SubsetOf(eqSpaced) {
		t.Fatal("equalities on distinct values are not subsets of each other")
	}
	if eqSpaced.EquivalentTo(eqApart) {
		t.Fatal("equalities on distinct values are not equivalent")
	}
	if !eqSpaced.And(eqApart).IsEmpty() {
		t.Fatal("a field cannot equal two distinct values")
	}
	if Field("tags", In, []any{spaced}).SubsetOf(Field("tags", In, []any{apart})) {
		t.Fatal("a set holding one value must not prove a set holding another")
	}
	// The same collision one level down, inside a map value.
	mapSpaced := map[string]any{"k": spaced}
	mapApart := map[string]any{"k": apart}
	if Field("meta", Eq, mapSpaced).SubsetOf(Field("meta", Eq, mapApart)) {
		t.Fatal("map values must keep an unambiguous identity")
	}
	if Field("meta", Eq, mapSpaced).EquivalentTo(Field("meta", Eq, mapApart)) {
		t.Fatal("map values must keep an unambiguous identity")
	}
}

// TestNegationComplement pins dnfNegate to the runtime complement. Match.Matches
// negates an atom's boolean result, so Not(atom) also matches effects whose
// field is absent, or present with a value the atom cannot compare. Mapping Not
// onto the opposite atom alone drops those effects and over-claims.
func TestNegationComplement(t *testing.T) {
	absent := Effect{Kind: "write"}
	wrongType := Effect{Kind: "write", Fields: map[string]any{"x": "s"}}
	one := Effect{Kind: "write", Fields: map[string]any{"p": 1}}
	two := Effect{Kind: "write", Fields: map[string]any{"p": 2}}

	t.Run("Not(eq) covers an absent field but ne does not", func(t *testing.T) {
		notEq := Field("p", Eq, 1).Not()
		ne := Field("p", Ne, 1)

		if !notEq.Matches(absent) {
			t.Fatal(`not(p == 1) must match an effect with no p`)
		}
		if ne.Matches(absent) {
			t.Fatal("p != 1 must not match an effect with no p")
		}
		if notEq.SubsetOf(ne) {
			t.Fatal("not(p == 1) is not a subset of p != 1: only the first matches an absent field")
		}
		if !ne.SubsetOf(notEq) {
			t.Fatal("p != 1 is a subset of not(p == 1)")
		}
		for _, e := range []Effect{one, two} {
			if notEq.Matches(e) != ne.Matches(e) {
				t.Fatalf("the two matchers must agree on the present value %s", e)
			}
		}

		child := Plan{}.AllowOnly(Field("p", Eq, 1).Not())
		parent := Plan{}.AllowOnly(Field("p", Ne, 1))
		if err := Conforms(child, parent); !errors.Is(err, ErrEffectSurfaceTooWide) {
			t.Fatalf("Conforms = %v; want ErrEffectSurfaceTooWide", err)
		}
		if err := Conforms(parent, child); err != nil {
			t.Fatalf("Conforms = %v; want nil", err)
		}
	})

	t.Run("complementary bounds stay non-empty on an absent field", func(t *testing.T) {
		m := Field("p", Lt, 5).Not().And(Field("p", Gte, 5).Not())
		if !m.Matches(absent) {
			t.Fatal("an effect with no p must match both negations")
		}
		if m.IsEmpty() {
			t.Fatal("IsEmpty = true; an absent field matches the conjunction")
		}
		if m.SubsetOf(Field("p", Eq, 5)) {
			t.Fatal("an absent field matches the conjunction but not p == 5")
		}
		if m.EquivalentTo(Nothing()) {
			t.Fatal("the conjunction matches effects, so it is not Nothing")
		}
	})

	t.Run("an uncomparable value survives a comparison negation", func(t *testing.T) {
		m := Field("x", Lt, 1).Not().And(Field("x", Eq, "s"))
		if !m.Matches(wrongType) {
			t.Fatalf(`not(x < 1) must match x = "s", which no comparison orders`)
		}
		if m.IsEmpty() {
			t.Fatalf(`IsEmpty = true; x = "s" matches the matcher`)
		}
		if !m.SubsetOf(Field("x", Eq, "s")) {
			t.Fatal(`the matcher conjoins x == "s", so it is a subset of it`)
		}
	})

	t.Run("string negations keep their wrong-typed case", func(t *testing.T) {
		notPrefix := Field("p", StartsWith, "s").Not()
		if !notPrefix.Matches(absent) || !notPrefix.Matches(wrongType) {
			t.Fatal("not(p starts with s) must match absent and non-string values")
		}
		// Its canonical form must keep those effects, or the proofs would
		// reason about a smaller surface than the matcher really accepts.
		canonical := notPrefix.Canonical()
		if canonical.String() != canonical.Canonical().String() {
			t.Fatalf("Canonical is not idempotent: %s", canonical)
		}
		for _, e := range []Effect{absent, wrongType, one, two} {
			if got, want := canonical.Matches(e), notPrefix.Matches(e); got != want {
				t.Fatalf("Canonical changed semantics on %s: got %v; want %v", e, got, want)
			}
		}
	})
}

// TestProofsAgreeWithMatches is a soundness sweep: whenever an effect matches
// the narrow matcher and not the wide one, SubsetOf must decline, and a matcher
// reported empty must match no probe at all.
func TestProofsAgreeWithMatches(t *testing.T) {
	probes := []Effect{
		{Kind: "write"},
		{Kind: "write", Fields: map[string]any{"p": 1}},
		{Kind: "write", Fields: map[string]any{"p": 5}},
		{Kind: "write", Fields: map[string]any{"p": "s"}},
		{Kind: "write", Fields: map[string]any{"p": nil}},
		{Kind: "delete", Fields: map[string]any{"p": 2}},
	}
	matchers := map[string]Match{
		"all":         All(),
		"nothing":     Nothing(),
		"eq1":         Field("p", Eq, 1),
		"ne1":         Field("p", Ne, 1),
		"lt5":         Field("p", Lt, 5),
		"gte5":        Field("p", Gte, 5),
		"notEq1":      Field("p", Eq, 1).Not(),
		"notLt5":      Field("p", Lt, 5).Not(),
		"notBounds":   Field("p", Lt, 5).Not().And(Field("p", Gte, 5).Not()),
		"write":       Exact("write"),
		"writeNotEq1": Exact("write").And(Field("p", Eq, 1).Not()),
		"inSet":       Field("p", In, []any{1, "s"}),
		"notIn1":      Field("p", NotIn, []any{1}),
		"notInNil":    Field("p", NotIn, []any{1, nil}),
		"isNone":      Field("p", IsNone, nil),
		"isNotNone":   Field("p", IsNotNone, nil),
		"prefix":      Field("p", StartsWith, "s"),
		"notPrefix":   Field("p", StartsWith, "s").Not(),
	}

	for name, m := range matchers {
		if m.IsEmpty() {
			for _, e := range probes {
				if m.Matches(e) {
					t.Fatalf("%s.IsEmpty() = true, but it matches %s", name, e)
				}
			}
		}
		for oname, o := range matchers {
			if m.SubsetOf(o) {
				for _, e := range probes {
					if m.Matches(e) && !o.Matches(e) {
						t.Fatalf("%s.SubsetOf(%s) = true, but %s matches only the first", name, oname, e)
					}
				}
			}
			if m.EquivalentTo(o) {
				for _, e := range probes {
					if m.Matches(e) != o.Matches(e) {
						t.Fatalf("%s.EquivalentTo(%s) = true, but they differ on %s", name, oname, e)
					}
				}
			}
		}
	}
}

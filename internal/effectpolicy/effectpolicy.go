// Package effectpolicy is a small structural authority model for observed
// effects: what an agent did (an Effect: a Kind plus named fields) and which
// surface of effects a Plan permits.
//
// It is a leaf package: standard library only, no yaah imports, no
// configuration loading, no model calls. The intent is the structural core of
// Shepherd's effects/policy.py — the vocabulary a sub-agent → parent
// conformance check will later be built on. The monitor-assignment compiler,
// handle/observe grant enforcement, kind hierarchies, and YAML loaders are
// deliberately absent.
//
// # Soundness of the reasoning API
//
// IsEmpty, SubsetOf and EquivalentTo must be SOUND and CONSERVATIVE. They may
// under-approximate — returning false when the sets are in fact in the desired
// relation but the matcher cannot prove it — but they must NEVER
// over-approximate: a true result always means the relation really holds.
//
// The reasoning works on Canonical, a disjunctive normal form over the field
// space: a disjunction of clauses, each clause a conjunction of atoms like
// `kind == "write"` or `path starts with "/tmp"`. Each question then reduces
// to a syntactic check between clauses:
//
//   - IsEmpty: no clause survives after clauses with contradictory atoms are
//     dropped. Nothing() is the empty surface.
//   - SubsetOf: every clause of the narrower matcher is entailed by some
//     clause of the wider one, where entailment is decided atom by atom by a
//     small implication theory over a single field.
//   - EquivalentTo: the two canonical clause sets are identical.
//
// # Where this under-approximates
//
// Each of these is a place a true relation is reported as false, never the
// reverse:
//
//   - Integrality. `x > 5` does not prove `x >= 6`, because the field domain
//     is untyped and fractional values would break the entailment.
//   - Alternative atom spellings. `path == "/a"` and `path in ["/a"]` denote
//     the same set but canonicalize to different atoms, so they are not
//     EquivalentTo and only one direction of SubsetOf is provable.
//   - Disjunctive splitting. A clause that implies a union without implying
//     any single member is not proved: `x > 0` is not shown to be a subset of
//     `y == 1 or y != 1`, although that union is everything.
//   - Regex and string algebras beyond simple subsumption. `matches("^/a")`
//     entails nothing but `is not null`; prefix and substring atoms entail
//     only shorter prefixes and substrings of the same string.
//   - Contradiction detection. Clauses are only provably empty when atoms
//     conflict pairwise through the rules in clauseUnsat; richer
//     unsatisfiability (three bounds, regex intersections) reads as non-empty.
//   - The clause budget. A canonical form whose disjunction exceeds
//     maxClauses clauses is abandoned; Canonical then returns the matcher
//     unchanged and every relation query answers false.
package effectpolicy

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// KindField is the reserved field name that carries Effect.Kind, so a matcher
// pins the effect kind with the same vocabulary it uses for any other field.
const KindField = "kind"

// Effect is the minimal observed unit of behavior: a Kind such as "write",
// "delete", "bash", or "file_patch", plus the named fields that describe what
// the effect touched (path, command, existed).
type Effect struct {
	Kind   string
	Fields map[string]any
}

// Field returns the named field's value and whether it is present. The
// effect's Kind is reachable as the reserved field name KindField.
func (e Effect) Field(name string) (any, bool) {
	if name == KindField {
		return e.Kind, true
	}
	v, ok := e.Fields[name]
	return v, ok
}

// String renders the effect with its fields in name order, so the output is
// stable for logs and test expectations.
func (e Effect) String() string {
	names := make([]string, 0, len(e.Fields))
	for name := range e.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("effect(")
	b.WriteString(KindField)
	b.WriteByte('=')
	b.WriteString(renderValue(e.Kind))
	for _, name := range names {
		b.WriteByte(' ')
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(renderValue(e.Fields[name]))
	}
	b.WriteByte(')')
	return b.String()
}

// Op is a field predicate operator. A field that is absent, or whose value has
// the wrong type for the operator, makes the predicate false — never an error
// and never a panic.
type Op string

// The field operators. Eq and Ne are exact value equality; Lt/Lte/Gt/Gte
// compare numbers numerically and strings lexicographically and are false for
// any other pairing; In/NotIn test membership in a set; StartsWith, EndsWith,
// and Contains are string (or, for Contains, collection) tests; Matches is a
// regular expression match; IsNone and IsNotNone test for a nil value.
const (
	Eq         Op = "eq"
	Ne         Op = "ne"
	Lt         Op = "lt"
	Lte        Op = "lte"
	Gt         Op = "gt"
	Gte        Op = "gte"
	In         Op = "in"
	NotIn      Op = "not_in"
	StartsWith Op = "startswith"
	EndsWith   Op = "endswith"
	IsNone     Op = "is_none"
	IsNotNone  Op = "is_not_none"
	Contains   Op = "contains"
	Matches    Op = "matches"
)

// matchKind tags a Match node. The zero value is kindAll, so the zero Match
// matches every effect.
type matchKind uint8

const (
	kindAll matchKind = iota
	kindNothing
	kindAtom
	kindAnd
	kindOr
	kindNot
)

// Match is an immutable structural predicate over an Effect. Every
// combinator returns a new Match and never mutates its receiver, so a Match
// can be shared by concurrent readers.
type Match struct {
	kind matchKind
	atom atom
	kids []Match
}

// All returns a Match that matches every effect. It is also the zero Match.
func All() Match { return Match{kind: kindAll} }

// Nothing returns a Match that matches no effect.
func Nothing() Match { return Match{kind: kindNothing} }

// Exact returns a Match on the effect kind, e.g. Exact("write").
func Exact(kind string) Match { return Field(KindField, Eq, kind) }

// Field returns a Match testing one named field against a constant. An
// operator whose argument cannot be used (In over a non-collection, Matches
// over an uncompilable expression, an unknown Op) yields Nothing, so a
// malformed matcher narrows to the empty surface rather than widening to All.
func Field(name string, op Op, value any) Match {
	a, ok := newAtom(name, op, value)
	if !ok {
		return Nothing()
	}
	return Match{kind: kindAtom, atom: a}
}

// Matches reports whether the effect satisfies the predicate.
func (m Match) Matches(e Effect) bool {
	switch m.kind {
	case kindAll:
		return true
	case kindNothing:
		return false
	case kindAtom:
		v, ok := e.Field(m.atom.field)
		if !ok {
			return false
		}
		return m.atom.holds(v)
	case kindAnd:
		for i := range m.kids {
			if !m.kids[i].Matches(e) {
				return false
			}
		}
		return true
	case kindOr:
		for i := range m.kids {
			if m.kids[i].Matches(e) {
				return true
			}
		}
		return false
	case kindNot:
		if len(m.kids) == 0 {
			return false
		}
		return !m.kids[0].Matches(e)
	}
	return false
}

// And returns the conjunction of m and o.
func (m Match) And(o Match) Match { return combine(kindAnd, kindAll, kindNothing, m, o) }

// Or returns the disjunction of m and o.
func (m Match) Or(o Match) Match { return combine(kindOr, kindNothing, kindAll, m, o) }

// combine builds an And (kindAnd) or Or (kindOr) node. identity is the operand
// that makes the result the other operand; absorbing is the operand that
// swallows it.
func combine(kind, identity, absorbing matchKind, m, o Match) Match {
	if m.kind == absorbing || o.kind == absorbing {
		return Match{kind: absorbing}
	}
	if m.kind == identity {
		return o
	}
	if o.kind == identity {
		return m
	}
	kids := make([]Match, 0, 4)
	for _, src := range []Match{m, o} {
		if src.kind == kind {
			kids = append(kids, src.kids...)
			continue
		}
		kids = append(kids, src)
	}
	return Match{kind: kind, kids: kids}
}

// Not returns the complement of m.
func (m Match) Not() Match {
	switch m.kind {
	case kindAll:
		return Nothing()
	case kindNothing:
		return All()
	case kindNot:
		if len(m.kids) == 0 {
			return All()
		}
		return m.kids[0]
	}
	return Match{kind: kindNot, kids: []Match{m}}
}

// Difference returns the effects that m matches and o does not.
func (m Match) Difference(o Match) Match { return m.And(o.Not()) }

// IsEmpty reports whether m matches no effect. It is conservative: false when
// emptiness cannot be proved.
func (m Match) IsEmpty() bool {
	d := m.dnf()
	if d.unknown {
		return false
	}
	return len(canonicalClauses(d)) == 0
}

// SubsetOf reports whether every effect m matches is also matched by o. It is
// conservative: false when the entailment cannot be proved.
func (m Match) SubsetOf(o Match) bool {
	a, b := m.dnf(), o.dnf()
	if a.unknown || b.unknown {
		return false
	}
	narrow := canonicalClauses(a)
	if len(narrow) == 0 {
		return true
	}
	wide := canonicalClauses(b)
	if len(wide) == 0 {
		return false
	}
	for _, c := range narrow {
		if !coveredByAny(c, wide) {
			return false
		}
	}
	return true
}

// EquivalentTo reports whether m and o match exactly the same effects. It is
// conservative: false when equivalence cannot be proved.
func (m Match) EquivalentTo(o Match) bool {
	a, b := m.dnf(), o.dnf()
	if a.unknown || b.unknown {
		return false
	}
	ac, bc := canonicalClauses(a), canonicalClauses(b)
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if clauseKey(ac[i]) != clauseKey(bc[i]) {
			return false
		}
	}
	return true
}

// Canonical returns a normalizing equivalent of m: a disjunction of clauses,
// each clause a conjunction of field atoms sorted by field and operator, and
// the clauses themselves sorted. Canonical is idempotent — canonicalizing a
// canonical form returns it unchanged — which is what makes SubsetOf and
// EquivalentTo a syntactic check. When the canonical form would exceed the
// clause budget, the matcher is returned unchanged and the relation queries
// answer false.
func (m Match) Canonical() Match {
	d := m.dnf()
	if d.unknown {
		return m
	}
	clauses := canonicalClauses(d)
	if len(clauses) == 0 {
		return Nothing()
	}
	kids := make([]Match, 0, len(clauses))
	for _, c := range clauses {
		kids = append(kids, andAtoms(c))
	}
	if len(kids) == 1 {
		return kids[0]
	}
	return Match{kind: kindOr, kids: kids}
}

// String renders the matcher as a readable expression.
func (m Match) String() string {
	switch m.kind {
	case kindAll:
		return "all()"
	case kindNothing:
		return "nothing()"
	case kindAtom:
		return m.atom.String()
	case kindAnd, kindOr:
		name := "and"
		if m.kind == kindOr {
			name = "or"
		}
		parts := make([]string, 0, len(m.kids))
		for i := range m.kids {
			parts = append(parts, m.kids[i].String())
		}
		return name + "(" + strings.Join(parts, ", ") + ")"
	case kindNot:
		if len(m.kids) == 0 {
			return "not(all())"
		}
		return "not(" + m.kids[0].String() + ")"
	}
	return "unknown()"
}

// Plan is an immutable authority plan: a stack of allow clauses intersected
// with the negation of a stack of deny clauses. Each AllowOnly narrows; each
// DenyKind adds to what is denied. The zero Plan permits everything.
type Plan struct {
	allows []Match
	denies []Match
}

// AllowOnly AND-composes m into the allow list. An empty allow list allows
// everything, so each call can only narrow the surface.
func (p Plan) AllowOnly(m Match) Plan {
	return Plan{allows: append(slices.Clone(p.allows), m), denies: slices.Clone(p.denies)}
}

// DenyKind OR-composes m into the deny list, so repeated calls each widen what
// is denied.
func (p Plan) DenyKind(m Match) Plan {
	return Plan{allows: slices.Clone(p.allows), denies: append(slices.Clone(p.denies), m)}
}

// DenyTool denies one effect kind, e.g. DenyTool("bash").
func (p Plan) DenyTool(name string) Plan { return p.DenyKind(Exact(name)) }

// EffectiveSurface returns the matcher for the effects this plan permits: the
// conjunction of the allow clauses, minus the disjunction of the deny clauses.
func (p Plan) EffectiveSurface() Match {
	surface := All()
	for _, m := range p.allows {
		surface = surface.And(m)
	}
	denied := Nothing()
	for _, m := range p.denies {
		denied = denied.Or(m)
	}
	return surface.Difference(denied)
}

// Permits reports whether the plan's effective surface covers the effect.
func (p Plan) Permits(e Effect) bool { return p.EffectiveSurface().Matches(e) }

// SubsetOf reports whether every effect this plan permits is also permitted by
// o. Like Match.SubsetOf it is conservative: false when the entailment cannot
// be proved.
func (p Plan) SubsetOf(o Plan) bool {
	return p.EffectiveSurface().SubsetOf(o.EffectiveSurface())
}

// Check returns nil when the plan permits the effect, and an error wrapping
// ErrEffectNotPermitted when it does not.
func (p Plan) Check(e Effect) error {
	if p.Permits(e) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrEffectNotPermitted, e)
}

// String renders the plan's clause stacks.
func (p Plan) String() string {
	return "plan(allow: [" + renderMatches(p.allows) + "] deny: [" + renderMatches(p.denies) + "])"
}

func renderMatches(ms []Match) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, ", ")
}

// The policy errors: an effect outside the plan's surface, a child surface
// wider than its caller's, and an empty surface. They are sentinels so callers
// can match them with errors.Is; Check and Conforms wrap them with the
// offending effect or surface. The Err prefix follows the exported-sentinel
// convention shared with the rest of the tree.
var (
	// ErrEffectNotPermitted reports an effect outside the plan's surface.
	ErrEffectNotPermitted = errors.New("effect not permitted by plan")
	// ErrEffectSurfaceTooWide reports a child surface that is not a subset of a
	// caller's surface.
	ErrEffectSurfaceTooWide = errors.New("effect surface is not a subset of the caller's surface")
	// ErrEffectSurfaceEmpty reports a surface that permits nothing.
	ErrEffectSurfaceEmpty = errors.New("effect surface is empty")
)

// Conforms reports whether the child plan stays inside the parent's surface,
// the authority relation between a sub-agent and its caller. An empty child
// surface is rejected as a delegation error rather than silently accepted: a
// child permitted to do nothing is almost always a typo in a tool or kind name.
func Conforms(child, parent Plan) error {
	surface := child.EffectiveSurface()
	if surface.IsEmpty() {
		return fmt.Errorf("%w: %s", ErrEffectSurfaceEmpty, surface)
	}
	if !child.SubsetOf(parent) {
		return fmt.Errorf("%w: child %s, parent %s",
			ErrEffectSurfaceTooWide, surface, parent.EffectiveSurface())
	}
	return nil
}

// atomOp is the internal operator set. It is the public Op set plus the
// negations of the non-negatable-by-an-Op testers (StartsWith, EndsWith,
// Contains, Matches), so that pushing a Not into a clause never needs a second
// node type.
type atomOp uint8

const (
	opEq atomOp = iota
	opNe
	opLt
	opLte
	opGt
	opGte
	opIn
	opNotIn
	opStartsWith
	opNotStartsWith
	opEndsWith
	opNotEndsWith
	opContains
	opNotContains
	opMatches
	opNotMatches
	opIsNone
	opIsNotNone
)

var atomOpNames = [...]string{
	opEq:            "eq",
	opNe:            "ne",
	opLt:            "lt",
	opLte:           "lte",
	opGt:            "gt",
	opGte:           "gte",
	opIn:            "in",
	opNotIn:         "not_in",
	opStartsWith:    "startswith",
	opNotStartsWith: "not_startswith",
	opEndsWith:      "endswith",
	opNotEndsWith:   "not_endswith",
	opContains:      "contains",
	opNotContains:   "not_contains",
	opMatches:       "matches",
	opNotMatches:    "not_matches",
	opIsNone:        "is_none",
	opIsNotNone:     "is_not_none",
}

func (o atomOp) String() string {
	if int(o) < len(atomOpNames) {
		return atomOpNames[o]
	}
	return "op(?)"
}

func (o atomOp) symbol() string {
	switch o {
	case opEq:
		return "=="
	case opNe:
		return "!="
	case opLt:
		return "<"
	case opLte:
		return "<="
	case opGt:
		return ">"
	case opGte:
		return ">="
	case opStartsWith:
		return "starts with"
	case opNotStartsWith:
		return "does not start with"
	case opEndsWith:
		return "ends with"
	case opNotEndsWith:
		return "does not end with"
	case opContains:
		return "contains"
	case opNotContains:
		return "does not contain"
	case opMatches:
		return "~"
	case opNotMatches:
		return "!~"
	}
	return o.String()
}

// atom is one field test: the unit of a canonical clause.
type atom struct {
	field string
	op    atomOp
	value any
	set   []any
	re    *regexp.Regexp
}

func newAtom(name string, op Op, value any) (atom, bool) {
	switch op {
	case Eq, Ne, Lt, Lte, Gt, Gte:
		return atom{field: name, op: atomOps[op], value: value}, true
	case In, NotIn:
		set, ok := toAnySlice(value)
		if !ok {
			return atom{}, false
		}
		return atom{field: name, op: atomOps[op], set: normalizeSet(set)}, true
	case StartsWith, EndsWith:
		s, ok := value.(string)
		if !ok {
			return atom{}, false
		}
		return atom{field: name, op: atomOps[op], value: s}, true
	case Contains:
		return atom{field: name, op: opContains, value: value}, true
	case Matches:
		s, ok := value.(string)
		if !ok {
			return atom{}, false
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return atom{}, false
		}
		return atom{field: name, op: opMatches, value: s, re: re}, true
	case IsNone:
		return atom{field: name, op: opIsNone}, true
	case IsNotNone:
		return atom{field: name, op: opIsNotNone}, true
	}
	return atom{}, false
}

var atomOps = map[Op]atomOp{
	Eq:         opEq,
	Ne:         opNe,
	Lt:         opLt,
	Lte:        opLte,
	Gt:         opGt,
	Gte:        opGte,
	In:         opIn,
	NotIn:      opNotIn,
	StartsWith: opStartsWith,
	EndsWith:   opEndsWith,
	IsNone:     opIsNone,
	IsNotNone:  opIsNotNone,
}

// negate returns the atom that denotes the complement of a. Every operator has
// a partner, so negation never leaves the atom language.
func (a atom) negate() atom {
	out := a
	switch a.op {
	case opEq:
		out.op = opNe
	case opNe:
		out.op = opEq
	case opLt:
		out.op = opGte
	case opLte:
		out.op = opGt
	case opGt:
		out.op = opLte
	case opGte:
		out.op = opLt
	case opIn:
		out.op = opNotIn
	case opNotIn:
		out.op = opIn
	case opStartsWith:
		out.op = opNotStartsWith
	case opNotStartsWith:
		out.op = opStartsWith
	case opEndsWith:
		out.op = opNotEndsWith
	case opNotEndsWith:
		out.op = opEndsWith
	case opContains:
		out.op = opNotContains
	case opNotContains:
		out.op = opContains
	case opMatches:
		out.op = opNotMatches
	case opNotMatches:
		out.op = opMatches
	case opIsNone:
		out.op = opIsNotNone
	case opIsNotNone:
		out.op = opIsNone
	}
	return out
}

// holds evaluates the atom against a present field value. A value of the wrong
// type for the operator is false under both a test and its negation: the
// field simply does not satisfy the atom.
func (a atom) holds(v any) bool {
	switch a.op {
	case opEq:
		return valuesEqual(v, a.value)
	case opNe:
		return !valuesEqual(v, a.value)
	case opLt:
		c, ok := compareValues(v, a.value)
		return ok && c < 0
	case opLte:
		c, ok := compareValues(v, a.value)
		return ok && c <= 0
	case opGt:
		c, ok := compareValues(v, a.value)
		return ok && c > 0
	case opGte:
		c, ok := compareValues(v, a.value)
		return ok && c >= 0
	case opIn:
		return setContains(a.set, v)
	case opNotIn:
		return !setContains(a.set, v)
	case opStartsWith:
		s, ok := v.(string)
		return ok && strings.HasPrefix(s, stringOf(a.value))
	case opNotStartsWith:
		s, ok := v.(string)
		return ok && !strings.HasPrefix(s, stringOf(a.value))
	case opEndsWith:
		s, ok := v.(string)
		return ok && strings.HasSuffix(s, stringOf(a.value))
	case opNotEndsWith:
		s, ok := v.(string)
		return ok && !strings.HasSuffix(s, stringOf(a.value))
	case opContains:
		return a.contains(v)
	case opNotContains:
		if !a.containable(v) {
			return false
		}
		return !a.contains(v)
	case opMatches:
		s, ok := v.(string)
		return ok && a.re != nil && a.re.MatchString(s)
	case opNotMatches:
		s, ok := v.(string)
		return ok && a.re != nil && !a.re.MatchString(s)
	case opIsNone:
		return v == nil
	case opIsNotNone:
		return v != nil
	}
	return false
}

// contains tests substring membership for string fields and element
// membership for collection fields.
func (a atom) contains(v any) bool {
	if s, ok := v.(string); ok {
		needle, ok := a.value.(string)
		return ok && strings.Contains(s, needle)
	}
	set, ok := toAnySlice(v)
	return ok && setContains(set, a.value)
}

// containable reports whether v has a type Contains is defined for, so that
// NotContains can stay false on a wrong-typed field.
func (a atom) containable(v any) bool {
	if _, ok := v.(string); ok {
		return true
	}
	_, ok := toAnySlice(v)
	return ok
}

// key is the structural identity of the atom, used for sorting, deduplication,
// and clause comparison.
func (a atom) key() string {
	var b strings.Builder
	b.WriteString(a.field)
	b.WriteByte('|')
	b.WriteString(a.op.String())
	b.WriteByte('|')
	if a.set != nil {
		b.WriteByte('[')
		for i, x := range a.set {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(valueKey(x))
		}
		b.WriteByte(']')
	}
	b.WriteString(valueKey(a.value))
	return b.String()
}

func (a atom) String() string {
	switch a.op {
	case opIsNone:
		return a.field + " is null"
	case opIsNotNone:
		return a.field + " is not null"
	case opIn:
		return a.field + " in " + renderSet(a.set)
	case opNotIn:
		return a.field + " not in " + renderSet(a.set)
	}
	return a.field + " " + a.op.symbol() + " " + renderValue(a.value)
}

// dnf is a disjunction of clauses, each clause a conjunction of atoms. Zero
// clauses is false (Nothing); a single empty clause is true (All).
type dnf struct {
	clauses [][]atom
	unknown bool
}

// maxClauses bounds the disjunction size. A matcher that exceeds it loses its
// canonical form and all relation queries answer false.
const maxClauses = 256

func (m Match) dnf() dnf {
	switch m.kind {
	case kindAll:
		return dnf{clauses: [][]atom{{}}}
	case kindNothing:
		return dnf{}
	case kindAtom:
		return dnf{clauses: [][]atom{{m.atom}}}
	case kindAnd:
		parts := make([]dnf, 0, len(m.kids))
		for i := range m.kids {
			parts = append(parts, m.kids[i].dnf())
		}
		return dnfProduct(parts)
	case kindOr:
		out := dnf{}
		for i := range m.kids {
			sub := m.kids[i].dnf()
			if sub.unknown {
				return dnf{unknown: true}
			}
			out.clauses = append(out.clauses, sub.clauses...)
			if len(out.clauses) > maxClauses {
				return dnf{unknown: true}
			}
		}
		return out
	case kindNot:
		if len(m.kids) == 0 {
			return dnf{clauses: [][]atom{{}}}
		}
		return dnfNegate(m.kids[0].dnf())
	}
	return dnf{unknown: true}
}

// dnfProduct conjoins the parts, distributing the disjunction.
func dnfProduct(parts []dnf) dnf {
	out := dnf{clauses: [][]atom{{}}}
	for _, p := range parts {
		if p.unknown {
			return dnf{unknown: true}
		}
		if len(p.clauses) == 0 {
			return dnf{}
		}
		if len(out.clauses)*len(p.clauses) > maxClauses {
			return dnf{unknown: true}
		}
		next := make([][]atom, 0, len(out.clauses)*len(p.clauses))
		for _, left := range out.clauses {
			for _, right := range p.clauses {
				merged := make([]atom, 0, len(left)+len(right))
				merged = append(merged, left...)
				merged = append(merged, right...)
				next = append(next, merged)
			}
		}
		out.clauses = next
	}
	return out
}

// dnfNegate pushes negation through the disjunction by De Morgan and
// distributes the resulting conjunction back into disjunctive normal form.
func dnfNegate(x dnf) dnf {
	if x.unknown {
		return dnf{unknown: true}
	}
	if len(x.clauses) == 0 {
		return dnf{clauses: [][]atom{{}}}
	}
	parts := make([]dnf, 0, len(x.clauses))
	for _, clause := range x.clauses {
		if len(clause) == 0 {
			return dnf{}
		}
		alts := make([][]atom, 0, len(clause))
		for _, a := range clause {
			alts = append(alts, []atom{a.negate()})
		}
		parts = append(parts, dnf{clauses: alts})
	}
	return dnfProduct(parts)
}

// canonicalClauses drops contradictory clauses, sorts the atoms inside each
// clause, and sorts and deduplicates the clauses. Dropping a contradictory
// clause is exact, not an approximation: clauseUnsat only reports clauses that
// really match nothing.
func canonicalClauses(d dnf) [][]atom {
	sorted := make([][]atom, 0, len(d.clauses))
	for _, clause := range d.clauses {
		if clauseUnsat(clause) {
			continue
		}
		sorted = append(sorted, canonicalClause(clause))
	}
	sort.SliceStable(sorted, func(i, j int) bool { return clauseKey(sorted[i]) < clauseKey(sorted[j]) })
	uniq := make([][]atom, 0, len(sorted))
	for i, clause := range sorted {
		if i > 0 && clauseKey(clause) == clauseKey(sorted[i-1]) {
			continue
		}
		uniq = append(uniq, clause)
	}
	return uniq
}

func canonicalClause(clause []atom) []atom {
	seen := make(map[string]struct{}, len(clause))
	out := make([]atom, 0, len(clause))
	for _, a := range clause {
		k := a.key()
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func clauseKey(clause []atom) string {
	parts := make([]string, 0, len(clause))
	for _, a := range clause {
		parts = append(parts, a.key())
	}
	return strings.Join(parts, " & ")
}

func andAtoms(clause []atom) Match {
	switch len(clause) {
	case 0:
		return All()
	case 1:
		return Match{kind: kindAtom, atom: clause[0]}
	}
	kids := make([]Match, 0, len(clause))
	for _, a := range clause {
		kids = append(kids, Match{kind: kindAtom, atom: a})
	}
	return Match{kind: kindAnd, kids: kids}
}

// coveredByAny reports whether some wide clause entails the narrow clause.
func coveredByAny(narrow []atom, wide [][]atom) bool {
	for _, d := range wide {
		if clauseEntails(narrow, d) {
			return true
		}
	}
	return false
}

// clauseEntails reports whether every effect satisfying the premise clause also
// satisfies every atom of the conclusion clause.
func clauseEntails(premise, conclusion []atom) bool {
	for _, target := range conclusion {
		proved := false
		for _, src := range premise {
			if atomImplies(src, target) {
				proved = true
				break
			}
		}
		if !proved {
			return false
		}
	}
	return true
}

// atomImplies reports whether every effect satisfying src also satisfies
// target. It is conservative: false when the theory cannot prove it.
func atomImplies(src, target atom) bool {
	if src.field != target.field {
		return false
	}
	if src.key() == target.key() {
		return true
	}
	switch src.op {
	case opEq:
		// A pinned value decides any other atom on the field.
		return target.holds(src.value)
	case opIsNone:
		return target.holds(nil)
	case opIn:
		// A finite set entails a target only when every member satisfies it.
		for _, x := range src.set {
			if !target.holds(x) {
				return false
			}
		}
		return true
	case opNotIn:
		switch target.op {
		case opNotIn:
			return setSubset(target.set, src.set)
		case opNe:
			return setContains(src.set, target.value)
		case opIsNotNone:
			return !setContains(src.set, nil)
		}
		return false
	case opNe:
		// Excluding nil is excluding the nil value.
		if target.op == opIsNotNone {
			return src.value == nil
		}
		return false
	case opIsNotNone:
		if target.op == opNe {
			return target.value == nil
		}
		return false
	case opGt, opGte, opLt, opLte:
		return comparisonImplies(src, target)
	case opStartsWith:
		prefix, ok := src.value.(string)
		if !ok {
			return false
		}
		switch target.op {
		case opStartsWith:
			other, ok := target.value.(string)
			return ok && strings.HasPrefix(prefix, other)
		case opContains:
			other, ok := target.value.(string)
			return ok && strings.Contains(prefix, other)
		case opIsNotNone:
			return true
		}
		return false
	case opEndsWith:
		suffix, ok := src.value.(string)
		if !ok {
			return false
		}
		switch target.op {
		case opEndsWith:
			other, ok := target.value.(string)
			return ok && strings.HasSuffix(suffix, other)
		case opContains:
			other, ok := target.value.(string)
			return ok && strings.Contains(suffix, other)
		case opIsNotNone:
			return true
		}
		return false
	case opContains:
		needle, ok := src.value.(string)
		if !ok {
			return false
		}
		switch target.op {
		case opContains:
			other, ok := target.value.(string)
			return ok && strings.Contains(needle, other)
		case opIsNotNone:
			return true
		}
		return false
	case opMatches:
		return target.op == opIsNotNone && src.re != nil
	}
	return false
}

// comparisonImplies decides entailment between two bounds on one field.
func comparisonImplies(src, target atom) bool {
	if target.op == opIsNotNone {
		// A comparable value is present and never nil.
		return true
	}
	if c, ok := compareValues(src.value, target.value); ok {
		switch src.op {
		case opGt:
			switch target.op {
			case opGt, opGte:
				return c >= 0
			}
		case opGte:
			switch target.op {
			case opGt:
				return c > 0
			case opGte:
				return c >= 0
			}
		case opLt:
			switch target.op {
			case opLt, opLte:
				return c <= 0
			}
		case opLte:
			switch target.op {
			case opLt:
				return c < 0
			case opLte:
				return c <= 0
			}
		}
	}
	// A strict bound excludes its own bound value and nothing else.
	if (src.op == opGt || src.op == opLt) && target.op == opNe {
		return valuesEqual(src.value, target.value)
	}
	return false
}

// clauseUnsat reports whether the conjunction of atoms provably matches
// nothing. It is conservative: a false here keeps a clause that may really be
// empty, which only weakens IsEmpty and SubsetOf.
func clauseUnsat(clause []atom) bool {
	for _, a := range clause {
		if a.op == opIn && len(a.set) == 0 {
			return true
		}
	}
	for i := 0; i < len(clause); i++ {
		for j := i + 1; j < len(clause); j++ {
			if !atomsCompatible(clause[i], clause[j]) {
				return true
			}
		}
	}
	return false
}

// atomsCompatible reports whether some effect could satisfy both atoms. False
// means provably unsatisfiable, so it must only be returned when no value can
// satisfy both.
func atomsCompatible(a, b atom) bool {
	if a.field != b.field {
		return true
	}
	if a.key() == b.key() {
		return true
	}
	if a.op == opEq {
		return b.holds(a.value)
	}
	if b.op == opEq {
		return a.holds(b.value)
	}
	if a.op == opIsNone {
		return b.holds(nil)
	}
	if b.op == opIsNone {
		return a.holds(nil)
	}
	if a.op == opIn {
		return anyHolds(a.set, b)
	}
	if b.op == opIn {
		return anyHolds(b.set, a)
	}
	if conflict, decided := boundsConflict(a, b); decided {
		return !conflict
	}
	return true
}

// anyHolds reports whether any member of the set satisfies the atom.
func anyHolds(set []any, a atom) bool {
	for _, x := range set {
		if a.holds(x) {
			return true
		}
	}
	return false
}

func isComparisonOp(op atomOp) bool {
	switch op {
	case opLt, opLte, opGt, opGte:
		return true
	}
	return false
}

func isLowerBound(op atomOp) bool { return op == opGt || op == opGte }

func isStrictBound(op atomOp) bool { return op == opGt || op == opLt }

// boundsConflict reports whether two comparison atoms on the same field have
// disjoint ranges. decided is false when the pair proves nothing, e.g. two
// lower bounds, which always overlap.
func boundsConflict(a, b atom) (conflict, decided bool) {
	if !isComparisonOp(a.op) || !isComparisonOp(b.op) {
		return false, false
	}
	lower, upper := a, b
	if !isLowerBound(lower.op) {
		lower, upper = upper, lower
	}
	if !isLowerBound(lower.op) || isLowerBound(upper.op) {
		return false, false
	}
	c, ok := compareValues(lower.value, upper.value)
	if !ok {
		return false, false
	}
	switch {
	case c > 0:
		return true, true
	case c == 0:
		// The windows meet at one point, which a strict bound excludes.
		return isStrictBound(lower.op) || isStrictBound(upper.op), true
	}
	return false, true
}

// setSubset reports whether every member of a is a member of b.
func setSubset(a, b []any) bool {
	for _, x := range a {
		if !setContains(b, x) {
			return false
		}
	}
	return true
}

func setContains(set []any, v any) bool {
	for _, x := range set {
		if valuesEqual(x, v) {
			return true
		}
	}
	return false
}

// valuesEqual compares two field values, treating numbers of different Go types
// as comparable and falling back to deep equality for everything else.
func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, ok := toFloat(a); ok {
		if y, ok := toFloat(b); ok {
			return x == y
		}
	}
	return reflect.DeepEqual(a, b)
}

// compareValues orders two values numerically or lexicographically. ok is false
// when the pair is not comparable, which makes every comparison operator false.
func compareValues(a, b any) (int, bool) {
	x, ok := toFloat(a)
	if !ok {
		s, ok := a.(string)
		if !ok {
			return 0, false
		}
		t, ok := b.(string)
		if !ok {
			return 0, false
		}
		return strings.Compare(s, t), true
	}
	y, ok := toFloat(b)
	if !ok {
		return 0, false
	}
	switch {
	case x < y:
		return -1, true
	case x > y:
		return 1, true
	}
	return 0, true
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// toAnySlice adapts any slice or array to []any. A string is not a collection
// here, so In over a string yields a matcher that never matches.
func toAnySlice(v any) ([]any, bool) {
	switch set := v.(type) {
	case nil:
		return nil, false
	case []any:
		return set, true
	case []string:
		out := make([]any, len(set))
		for i, s := range set {
			out[i] = s
		}
		return out, true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// normalizeSet copies the set into a stable order so that set-valued atoms have
// a deterministic identity.
func normalizeSet(set []any) []any {
	out := make([]any, len(set))
	copy(out, set)
	sort.SliceStable(out, func(i, j int) bool { return valueKey(out[i]) < valueKey(out[j]) })
	return out
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func valueKey(v any) string { return fmt.Sprintf("%T:%v", v, v) }

func renderValue(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprint(v)
}

func renderSet(set []any) string {
	parts := make([]string, 0, len(set))
	for _, x := range set {
		parts = append(parts, renderValue(x))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

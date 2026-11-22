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
// # Negation is exact
//
// The atom alphabet carries three presence literals — `f is absent`,
// `f is present`, and an explicit `not(atom)` — so the complement of any atom
// is expressible in it. An atom matches only an effect whose field is present,
// so its runtime complement also covers every effect whose field is absent;
// and a value of the wrong type for a typed operator satisfies neither the
// atom nor its opposite, so the complement covers that case too. Negation
// therefore emits the absent-field clause plus either the opposite atom (only
// for the operators whose opposite is exactly complementary) or the explicit
// negated literal. Substituting the opposite atom alone would drop those
// effects and let SubsetOf, IsEmpty, and EquivalentTo over-claim.
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
//   - Value identity outside the constant domain. Atom identity is a
//     structural encoding of nil, booleans, strings, numbers, slices, arrays,
//     and maps, so two constants the runtime distinguishes never share an
//     identity. An exotic value (a function, a channel, a struct with
//     unexported state) falls back to a Go-syntax rendering whose identity is
//     only best effort.
//   - Contradiction detection. Clauses are only provably empty when atoms
//     conflict pairwise through the rules in clauseUnsat; richer
//     unsatisfiability (three bounds, regex intersections) reads as non-empty.
//   - The clause budget. A canonical form whose disjunction exceeds
//     maxClauses clauses is abandoned; Canonical then returns the matcher
//     unchanged and every relation query answers false.
package effectpolicy

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
			return m.atom.op == opAbsent
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
// presence literals (opAbsent, opPresent) and the explicit negated literal
// (opNot), so that the complement of any atom is expressible exactly and
// pushing a Not into a clause never needs a second node type.
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
	opEndsWith
	opContains
	opMatches
	opIsNone
	opIsNotNone
	opAbsent
	opPresent
	opNot
)

var atomOpNames = [...]string{
	opEq:         "eq",
	opNe:         "ne",
	opLt:         "lt",
	opLte:        "lte",
	opGt:         "gt",
	opGte:        "gte",
	opIn:         "in",
	opNotIn:      "not_in",
	opStartsWith: "startswith",
	opEndsWith:   "endswith",
	opContains:   "contains",
	opMatches:    "matches",
	opIsNone:     "is_none",
	opIsNotNone:  "is_not_none",
	opAbsent:     "absent",
	opPresent:    "present",
	opNot:        "not",
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
	case opEndsWith:
		return "ends with"
	case opContains:
		return "contains"
	case opMatches:
		return "~"
	case opAbsent:
		return "is absent"
	case opPresent:
		return "is present"
	}
	return o.String()
}

// atom is one field test: the unit of a canonical clause. inner is set only on
// the explicit negated literal (opNot), which records the present values the
// nested atom rejects.
type atom struct {
	field string
	op    atomOp
	value any
	set   []any
	re    *regexp.Regexp
	inner *atom
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

// exactOpposite returns the atom that denotes a's complement on a *present*
// field, for the operators whose opposite is exactly complementary. ok is
// false for the typed operators (comparisons, string tests, regexps): a value
// of the wrong type satisfies neither the atom nor its opposite operator, so
// the opposite atom alone would drop effects the runtime complement matches.
func (a atom) exactOpposite() (atom, bool) {
	out := a
	switch a.op {
	case opEq:
		out.op = opNe
	case opNe:
		out.op = opEq
	case opIn:
		out.op = opNotIn
	case opNotIn:
		out.op = opIn
	case opIsNone:
		out.op = opIsNotNone
	case opIsNotNone:
		out.op = opIsNone
	case opNot:
		if a.inner == nil {
			return atom{}, false
		}
		return *a.inner, true
	default:
		return atom{}, false
	}
	return out, true
}

// absent and present return the presence tests on a's field.
func (a atom) absent() atom { return atom{field: a.field, op: opAbsent} }

func (a atom) present() atom { return atom{field: a.field, op: opPresent} }

// complement returns the disjunction of one-atom clauses that denotes exactly
// the runtime complement of a: every effect whose field is absent matches it,
// and so does every effect whose field is present with a value a rejects.
// Match.Matches is ground truth for what a negation accepts, so this must not
// drop the absent (or wrong-typed) side.
func (a atom) complement() [][]atom {
	switch a.op {
	case opAbsent:
		return [][]atom{{a.present()}}
	case opPresent:
		return [][]atom{{a.absent()}}
	}
	out := [][]atom{{a.absent()}}
	if opp, ok := a.exactOpposite(); ok {
		return append(out, []atom{opp})
	}
	inner := a
	return append(out, []atom{{field: a.field, op: opNot, inner: &inner}})
}

// holds evaluates the atom against a present field value. A value of the wrong
// type for a typed operator is false under both the atom and its opposite: the
// field simply does not satisfy the test. That case is not lost, because the
// explicit negated literal (opNot) records it.
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
	case opEndsWith:
		s, ok := v.(string)
		return ok && strings.HasSuffix(s, stringOf(a.value))
	case opContains:
		return a.contains(v)
	case opMatches:
		s, ok := v.(string)
		return ok && a.re != nil && a.re.MatchString(s)
	case opIsNone:
		return v == nil
	case opIsNotNone:
		return v != nil
	case opAbsent:
		// An absent field never reaches holds: the atom is decided by
		// Match.Matches, which sees the absence itself.
		return false
	case opPresent:
		return true
	case opNot:
		return a.inner != nil && !a.inner.holds(v)
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

// key is the structural identity of the atom, used for sorting, deduplication,
// and clause comparison. Every part is length-prefixed, so two atoms share a
// key only when they really are the same test: no rendering ambiguity can make
// distinct atoms look identical.
func (a atom) key() string {
	var b strings.Builder
	b.WriteString("a(")
	writePart(&b, a.field)
	writePart(&b, a.op.String())
	switch {
	case a.inner != nil:
		writePart(&b, a.inner.key())
	case a.set != nil:
		b.WriteString(strconv.Itoa(len(a.set)))
		b.WriteByte('[')
		for _, x := range a.set {
			writePart(&b, valueKey(x))
		}
		b.WriteByte(']')
	default:
		writePart(&b, valueKey(a.value))
	}
	return b.String()
}

// writePart appends one length-prefixed part, so no part's content can be read
// as a different part's.
func writePart(b *strings.Builder, part string) {
	b.WriteString(strconv.Itoa(len(part)))
	b.WriteByte(':')
	b.WriteString(part)
}

func (a atom) String() string {
	switch a.op {
	case opIsNone:
		return a.field + " is null"
	case opIsNotNone:
		return a.field + " is not null"
	case opAbsent:
		return a.field + " is absent"
	case opPresent:
		return a.field + " is present"
	case opNot:
		if a.inner == nil {
			return "not(" + a.field + ")"
		}
		return "not(" + a.inner.String() + ")"
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
// distributes the resulting conjunction back into disjunctive normal form. Each
// atom is complemented exactly (atom.complement), so the result denotes the
// same effects the runtime negation matches — absent fields and wrong-typed
// values included.
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
		alts := make([][]atom, 0, 2*len(clause))
		for _, a := range clause {
			alts = append(alts, a.complement()...)
		}
		if len(alts) > maxClauses {
			return dnf{unknown: true}
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
	if target.op == opPresent {
		// Every atom except the absent test needs the field to be present.
		return src.op != opAbsent
	}
	if target.op == opNot {
		if target.inner == nil {
			return false
		}
		// An atom that cannot be satisfied alongside the inner atom excludes
		// it exactly, so it entails the negated literal. Otherwise fall through:
		// a pinned source value decides the literal directly.
		if !atomsCompatible(src, *target.inner) {
			return true
		}
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
			// Excluding the nil value is what excludes the nil value; an
			// excluded set without nil still admits a present nil field.
			return setContains(src.set, nil)
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
	// Presence is decided: an absent field cannot satisfy an atom that needs a
	// value, and a present field is not absent.
	if a.op == opAbsent || b.op == opAbsent {
		return false
	}
	// An explicit negated literal conflicts with exactly the atoms that entail
	// its inner atom, and two negated literals can always coexist.
	if a.op == opNot {
		if a.inner == nil {
			return false
		}
		if b.op == opNot {
			return true
		}
		return !atomImplies(b, *a.inner)
	}
	if b.op == opNot {
		if b.inner == nil {
			return false
		}
		return !atomImplies(a, *b.inner)
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
// as comparable when their exact values agree and falling back to deep equality
// for everything else.
func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, y := numberOf(a), numberOf(b); x.ok && y.ok {
		c, ok := compareNumber(x, y)
		return ok && c == 0
	}
	return reflect.DeepEqual(a, b)
}

// compareValues orders two values numerically or lexicographically. ok is false
// when the pair is not comparable, which makes every comparison operator false.
func compareValues(a, b any) (int, bool) {
	if x, y := numberOf(a), numberOf(b); x.ok || y.ok {
		return compareNumber(x, y)
	}
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

// numberKind classifies a numeric value: signed, unsigned, or floating point.
type numberKind uint8

const (
	numSigned numberKind = iota
	numUnsigned
	numFloat
)

// number is one numeric value kept in its own representation. Rounding every
// number to float64 would make neighbouring large integers compare equal —
// 2^64-2 and 2^64-1 both land on the same float64 — so integral values keep
// their exact magnitude instead.
type number struct {
	kind numberKind
	i    int64
	u    uint64
	f    float64
	ok   bool
}

func numberOf(v any) number {
	switch n := v.(type) {
	case int:
		return number{kind: numSigned, i: int64(n), ok: true}
	case int8:
		return number{kind: numSigned, i: int64(n), ok: true}
	case int16:
		return number{kind: numSigned, i: int64(n), ok: true}
	case int32:
		return number{kind: numSigned, i: int64(n), ok: true}
	case int64:
		return number{kind: numSigned, i: n, ok: true}
	case uint:
		return number{kind: numUnsigned, u: uint64(n), ok: true}
	case uint8:
		return number{kind: numUnsigned, u: uint64(n), ok: true}
	case uint16:
		return number{kind: numUnsigned, u: uint64(n), ok: true}
	case uint32:
		return number{kind: numUnsigned, u: uint64(n), ok: true}
	case uint64:
		return number{kind: numUnsigned, u: n, ok: true}
	case float32:
		return number{kind: numFloat, f: float64(n), ok: true}
	case float64:
		return number{kind: numFloat, f: n, ok: true}
	}
	return number{}
}

// compareNumber orders two classified numbers exactly, including mixed
// signed/unsigned and integer/float pairs. ok is false when either operand is
// not a number, and for a NaN, which is unordered against everything.
func compareNumber(x, y number) (int, bool) {
	switch {
	case !x.ok || !y.ok:
		return 0, false
	case x.kind == numFloat && y.kind == numFloat:
		if math.IsNaN(x.f) || math.IsNaN(y.f) {
			return 0, false
		}
		return cmp.Compare(x.f, y.f), true
	case x.kind == numFloat:
		return compareFloatInteger(x.f, y)
	case y.kind == numFloat:
		c, ok := compareFloatInteger(y.f, x)
		return -c, ok
	case x.kind == numSigned && y.kind == numSigned:
		return cmp.Compare(x.i, y.i), true
	case x.kind == numUnsigned && y.kind == numUnsigned:
		return cmp.Compare(x.u, y.u), true
	case x.kind == numSigned:
		if x.i < 0 {
			return -1, true
		}
		return cmp.Compare(uint64(x.i), y.u), true
	default:
		if y.i < 0 {
			return 1, true
		}
		return cmp.Compare(x.u, uint64(y.i)), true
	}
}

// compareFloatInteger compares the float f against the integral number n
// without rounding n: a large integer is never reported equal to a neighbouring
// float value. ok is false for a NaN operand.
func compareFloatInteger(f float64, n number) (int, bool) {
	switch {
	case math.IsNaN(f):
		return 0, false
	case math.IsInf(f, 1):
		return 1, true
	case math.IsInf(f, -1):
		return -1, true
	}
	if n.kind == numSigned {
		// 2^63 bounds the float64 range an int64 can be exact inside.
		switch {
		case f >= 1<<63:
			return 1, true
		case f < -(1 << 63):
			return -1, true
		}
		whole := math.Floor(f)
		if c := cmp.Compare(int64(whole), n.i); c != 0 {
			return c, true
		}
		if f != whole {
			// The integer equals the float's floor, so it is below it.
			return -1, true
		}
		return 0, true
	}
	// 2^64 bounds the float64 range a uint64 can be exact inside.
	switch {
	case f < 0:
		return -1, true
	case f >= 1<<64:
		return 1, true
	}
	whole := math.Floor(f)
	if c := cmp.Compare(uint64(whole), n.u); c != 0 {
		return c, true
	}
	if f != whole {
		return -1, true
	}
	return 0, true
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

// valueKey is the structural identity of a constant value. The encoding is
// unambiguous — every part is tagged and length-prefixed — so two values the
// runtime distinguishes never share a key. A rendering like %v cannot promise
// that: []string{"a b", "c"} and []string{"a", "b c"} both print as "[a b c]",
// which would let two distinct equality atoms look identical to the proofs.
func valueKey(v any) string {
	var b strings.Builder
	writeValueKey(&b, v)
	return b.String()
}

func writeValueKey(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
		return
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return
	case string:
		b.WriteString("str:")
		writePart(b, x)
		return
	}
	if n := numberOf(v); n.ok {
		switch n.kind {
		case numSigned:
			b.WriteString("num:")
			writePart(b, strconv.FormatInt(n.i, 10))
		case numUnsigned:
			b.WriteString("num:")
			writePart(b, strconv.FormatUint(n.u, 10))
		case numFloat:
			b.WriteString("flt:")
			writePart(b, strconv.FormatFloat(n.f, 'x', -1, 64))
		}
		return
	}
	if elems, ok := toAnySlice(v); ok {
		b.WriteString("list:")
		writePart(b, reflect.TypeOf(v).String())
		b.WriteString(strconv.Itoa(len(elems)))
		b.WriteByte('[')
		for _, e := range elems {
			writePart(b, valueKey(e))
		}
		b.WriteByte(']')
		return
	}
	if entries, ok := mapEntries(v); ok {
		b.WriteString("map:")
		writePart(b, reflect.TypeOf(v).String())
		b.WriteString(strconv.Itoa(len(entries)))
		b.WriteByte('{')
		for _, e := range entries {
			writePart(b, e)
		}
		b.WriteByte('}')
		return
	}
	// Outside the constant domain (functions, channels, structs carrying
	// unexported state) identity falls back to a type-tagged Go-syntax
	// rendering, which is only best effort.
	b.WriteString("opaque:")
	writePart(b, fmt.Sprintf("%T", v))
	writePart(b, fmt.Sprintf("%#v", v))
}

// mapEntries renders a map as sorted "key=value" entry keys. ok is false when v
// is not a map.
func mapEntries(v any) ([]string, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return nil, false
	}
	keys := rv.MapKeys()
	entries := make([]string, 0, len(keys))
	for _, k := range keys {
		var b strings.Builder
		writeValueKey(&b, k.Interface())
		b.WriteByte('=')
		writeValueKey(&b, rv.MapIndex(k).Interface())
		entries = append(entries, b.String())
	}
	sort.Strings(entries)
	return entries, true
}

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

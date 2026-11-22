// Package permissionplan compiles one authority declaration into a permission
// plan: the set of monitor assignments that enforce the declaration, each
// naming which monitor enforces a clause and when.
//
// It is a leaf package: the standard library and internal/effectpolicy only —
// no configuration loading, no dispatch wiring, no model calls.
//
// # Two monitors, one declaration
//
// yaah enforces a dispatch's declared authority in two lanes that today share
// nothing but the config they were lowered from:
//
//   - the jail monitor, which confines the run before the action: a container
//     dispatch lowers world-relative grants to writable roots and refuses a
//     write outside them while the body runs (internal/dispatch's
//     dispatchAuthority);
//   - the settlement monitor, which reviews the proposal at commit: a
//     proposal's whole-path post-images are captured and reviewed, and only
//     what the manifest can faithfully represent is settled (internal/dispatch's
//     GitDelta).
//
// A PermissionPlan is the artifact both lanes can cite. For each clause of one
// declaration it names the monitor and timing that enforce that clause, and
// Digest is a stable content digest of exactly that assignment — so the jail
// and the settlement step can be shown to enforce the same declaration rather
// than two independently lowered copies of it.
//
// # Fail-closed completeness
//
// The compiler refuses rather than emit a plan that leaves a declared clause
// unenforced. Compile validates every declared value, lowers each clause to the
// effect surface it governs, assigns it to a monitor, and then runs Coverage:
// a declared clause no complete monitor covers is an error naming that clause,
// never a silent drop. See Coverage for the exact contract.
//
// # What this package does not do
//
// It does not wire the lanes onto the plan, load configuration, resolve roles,
// or reason about a running process. Mapping yaah's grants, network posture,
// and settlement settings onto Authority is a later increment.
package permissionplan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/goatherdai/yaah/internal/effectpolicy"
)

// The effect vocabulary a plan speaks. A write clause governs the mutation
// kinds under its root; a network clause governs every network effect; a delta
// clause governs every settlement-delta effect. Paths are world-relative,
// slash-separated, and carried in the "path" field.
const (
	// WriteKind is the effect kind for a file or directory mutation.
	WriteKind = "write"
	// DeleteKind is the effect kind for a removal, which a write clause
	// governs exactly as it governs a write.
	DeleteKind = "delete"
	// NetworkKind is the effect kind for egress.
	NetworkKind = "network"
	// DeltaKind is the effect kind for one path in a proposal's settlement
	// delta.
	DeltaKind = "delta"
	// PathField is the effect field carrying the world-relative path a
	// filesystem or delta effect touched.
	PathField = "path"
)

// Monitor identifies the enforcement lane that owns a clause.
type Monitor string

// The monitors. Each acts at exactly one timing; an assignment that pairs a
// monitor with any other timing is not complete.
const (
	// MonitorUnknown is the zero value: no monitor. An assignment that names it
	// enforces nothing, so it never covers a clause.
	MonitorUnknown Monitor = ""
	// MonitorJail is the pre-action confinement monitor: the world is bounded
	// before the body runs, and an effect outside the assigned surface is
	// refused while it runs.
	MonitorJail Monitor = "jail"
	// MonitorSettlement is the commit-time delta monitor: the body's post-images
	// are reviewed and applied only as far as the assigned surface allows.
	MonitorSettlement Monitor = "settlement"
)

// String renders the monitor, spelling the zero value "unknown".
func (m Monitor) String() string {
	if m == MonitorUnknown {
		return "unknown"
	}
	return string(m)
}

// Valid reports whether m names a known monitor.
func (m Monitor) Valid() bool {
	switch m {
	case MonitorJail, MonitorSettlement:
		return true
	case MonitorUnknown:
		return false
	}
	return false
}

// enforceable reports whether m acts at timing t.
func (m Monitor) enforceable(t Timing) bool {
	switch m {
	case MonitorJail:
		return t == TimingPreAction
	case MonitorSettlement:
		return t == TimingCommit
	case MonitorUnknown:
		return false
	}
	return false
}

// Timing is when a monitor enforces an assignment.
type Timing string

// The timings.
const (
	// TimingUnknown is the zero value: no timing, so no enforcement.
	TimingUnknown Timing = ""
	// TimingPreAction is before the effect: the monitor establishes the bound
	// the body runs under.
	TimingPreAction Timing = "pre_action"
	// TimingCommit is at settlement: the monitor reviews the delta the body
	// produced before it reaches the parent world.
	TimingCommit Timing = "commit"
)

// String renders the timing, spelling the zero value "unknown".
func (t Timing) String() string {
	if t == TimingUnknown {
		return "unknown"
	}
	return string(t)
}

// Valid reports whether t names a known timing.
func (t Timing) Valid() bool {
	switch t {
	case TimingPreAction, TimingCommit:
		return true
	case TimingUnknown:
		return false
	}
	return false
}

// Fallback is the posture for a declared clause its monitor cannot enforce: a
// world that could not be confined, or a lane that degraded to advisory.
//
// The zero value is FallbackRefuse, so a clause is only ever accepted without
// enforcement by an explicit FallbackEnforce declaration. The compiler never
// selects Enforce on its own: a degraded lane is an operator decision that the
// plan then records (and the digest then covers), not something a declaration
// gets by omission.
type Fallback string

// The fallback postures.
const (
	// FallbackRefuse is the zero value and the fail-closed posture: if the
	// assigned monitor cannot enforce its clause, the run is refused.
	FallbackRefuse Fallback = ""
	// FallbackEnforce accepts the clause on an unenforced lane, so the
	// declaration proceeds with the degradation recorded on the assignment.
	FallbackEnforce Fallback = "enforce"
)

// String renders the fallback, spelling the zero value "refuse".
func (f Fallback) String() string {
	switch f {
	case FallbackRefuse:
		return "refuse"
	case FallbackEnforce:
		return "enforce"
	}
	return fmt.Sprintf("fallback(%q)", string(f))
}

// valid reports whether f is a posture the compiler can lower.
func (f Fallback) valid() bool {
	switch f {
	case FallbackRefuse, FallbackEnforce:
		return true
	}
	return false
}

// Assignment is one clause of a declaration together with the monitor that
// enforces it and when.
type Assignment struct {
	// Label names the declared clause, for example write("src", readwrite).
	// It is stable and is part of the plan's digest.
	Label string
	// Clause is the effect surface the clause governs.
	Clause effectpolicy.Match
	// Monitor is the lane that enforces the clause and Timing is when it acts.
	Monitor Monitor
	Timing  Timing
	// Fallback is the posture if Monitor cannot enforce the clause at run time.
	Fallback Fallback
}

// complete reports whether a names a known monitor acting at that monitor's own
// timing. An incomplete assignment enforces nothing and never covers a clause.
func (a Assignment) complete() bool { return a.Monitor.enforceable(a.Timing) }

// String renders the assignment stably, for logs and test expectations.
func (a Assignment) String() string {
	return fmt.Sprintf("clause=%s monitor=%s timing=%s fallback=%s surface=%s",
		strconv.Quote(a.Label), a.Monitor, a.Timing, a.Fallback, a.Clause.Canonical())
}

// canonical is the assignment's line in the plan's canonical serialization.
func (a Assignment) canonical() string {
	return "assignment " + a.String()
}

// clause is one declared clause as the compiler saw it: the label the
// declaration gave it and the effect surface it governs. It survives into the
// plan even when no assignment enforces it, which is what lets Coverage name
// the gap instead of the clause vanishing.
type clause struct {
	label string
	match effectpolicy.Match
}

// String returns the clause's label.
func (c clause) String() string { return c.label }

// PermissionPlan is an immutable compiled authority declaration: the clauses the
// declaration makes and the monitor assignments that enforce them, in
// declaration order. The zero plan declares nothing, which is trivially
// covered.
type PermissionPlan struct {
	clauses     []clause
	assignments []Assignment
	fallback    Fallback
	// networkHosts is the broker allowlist the declaration qualified its egress
	// posture with, in canonical set form (distinct, sorted). It is empty for
	// every posture that is not the broker, and it is part of the plan's digest
	// because it is part of what the declaration promised to enforce.
	networkHosts []string
}

// declare records one clause of the declaration.
func (p *PermissionPlan) declare(c clause) { p.clauses = append(p.clauses, c) }

// assign records one monitor assignment.
func (p *PermissionPlan) assign(a Assignment) { p.assignments = append(p.assignments, a) }

// Assignments returns the plan's assignments in declaration order. The result is
// a copy, so a caller cannot mutate the plan.
func (p PermissionPlan) Assignments() []Assignment { return slices.Clone(p.assignments) }

// Fallback returns the declaration's posture for a clause its monitor cannot
// enforce.
func (p PermissionPlan) Fallback() Fallback { return p.fallback }

// Coverage reports whether every declared clause is covered by a complete
// monitor assignment.
//
// An assignment is complete when it names a known monitor acting at that
// monitor's own timing — the jail before the action, the settlement monitor at
// commit — and it covers a clause when the declared clause's effect surface is a
// subset of the assignment's. The subset test is effectpolicy's, so it is
// conservative: a coverage the matcher cannot prove is reported as a gap,
// never assumed.
//
// Coverage returns nil when the plan keeps its promise, and otherwise an error
// wrapping ErrClauseUncovered that names the first uncovered clause. Compile
// refuses to return a plan whose Coverage fails, so a plan that leaves a clause
// unenforced cannot be built through the compiler: a gap is always an error,
// never a silent drop.
func (p PermissionPlan) Coverage() error {
	for _, c := range p.clauses {
		if !p.covers(c) {
			return fmt.Errorf("%w: clause %s is not covered by any monitor", ErrClauseUncovered, c.label)
		}
	}
	return nil
}

// covers reports whether some complete assignment covers c.
func (p PermissionPlan) covers(c clause) bool {
	for _, a := range p.assignments {
		if !a.complete() {
			continue
		}
		if c.match.SubsetOf(a.Clause) {
			return true
		}
	}
	return false
}

// Digest returns the plan's stable content digest: the hex-encoded SHA-256 of
// the plan's canonical serialization.
//
// The serialization is the sorted set of the plan's canonical lines — one per
// declared clause, one per assignment, one for the declaration's fallback
// posture, and one for the broker's allowlist when the declaration has one.
// Clause order is therefore not part of a plan's identity, while any difference
// in a clause, its surface, the monitor assigned to it, that monitor's timing,
// the fallback, or the set of hosts the broker admits changes the digest. The
// same declaration always compiles to the same digest, which is what makes a
// plan citable: the jail and the settlement step can each record the digest of
// the declaration they enforce, and a reader can tell whether they enforced the
// same one.
func (p PermissionPlan) Digest() string {
	lines := p.canonicalLines()
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// canonicalLines is the plan's canonical serialization, one line per fact,
// sorted so that declaration order cannot change it.
func (p PermissionPlan) canonicalLines() []string {
	lines := make([]string, 0, len(p.clauses)+len(p.assignments)+2)
	lines = append(lines, "fallback="+p.fallback.String())
	if len(p.networkHosts) > 0 {
		// One line for the whole allowlist: it is a set, so the entries are
		// quoted and joined rather than sorted in among the other lines, where
		// a declaration difference would be spread across many lines.
		quoted := make([]string, 0, len(p.networkHosts))
		for _, h := range p.networkHosts {
			quoted = append(quoted, strconv.Quote(h))
		}
		lines = append(lines, "network_hosts=["+strings.Join(quoted, " ")+"]")
	}
	for _, c := range p.clauses {
		lines = append(lines, "declared clause="+strconv.Quote(c.label)+" surface="+c.match.Canonical().String())
	}
	for _, a := range p.assignments {
		lines = append(lines, a.canonical())
	}
	sort.Strings(lines)
	return lines
}

// String renders the plan's clauses, assignments, and fallback posture.
func (p PermissionPlan) String() string {
	clauses := make([]string, 0, len(p.clauses))
	for _, c := range p.clauses {
		clauses = append(clauses, c.label)
	}
	assignments := make([]string, 0, len(p.assignments))
	for _, a := range p.assignments {
		assignments = append(assignments, "{"+a.String()+"}")
	}
	return "plan(fallback=" + p.fallback.String() + p.networkHostsString() +
		" clauses=[" + strings.Join(clauses, " ") +
		"] assignments=[" + strings.Join(assignments, " ") + "])"
}

// networkHostsString renders the broker allowlist for String, and renders
// nothing at all when the declaration admits no hosts, so a plan without a
// broker reads exactly as it did before the allowlist was part of the plan.
func (p PermissionPlan) networkHostsString() string {
	if len(p.networkHosts) == 0 {
		return ""
	}
	return " network_hosts=[" + strings.Join(p.networkHosts, " ") + "]"
}

// The compiler's refusals. They are sentinels so a caller can tell an invalid
// declaration from an unenforceable one with errors.Is; both wrap a message
// naming the offending field or clause. The Err prefix follows the
// exported-sentinel convention shared with the rest of the tree.
var (
	// ErrInvalidDeclaration reports a declared value the compiler cannot lower:
	// an unknown write access, a root that is not a world-relative directory,
	// an unknown network posture, delta policy, containment, or fallback.
	ErrInvalidDeclaration = errors.New("invalid authority declaration")
	// ErrClauseUncovered reports a well-formed declared clause that no complete
	// monitor enforces. It is the fail-closed refusal: the declaration promised
	// something no monitor in this plan enforces.
	ErrClauseUncovered = errors.New("clause is not covered by any monitor")
)

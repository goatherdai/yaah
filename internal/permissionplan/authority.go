package permissionplan

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/goatherdai/yaah/internal/effectpolicy"
)

// Authority is one authority declaration, in the smallest shape that makes a
// permission plan meaningful: the filesystem write clauses, the egress posture,
// the settlement posture for a proposal's delta, and how the declaration expects
// to be confined. It is deliberately not yaah's configuration — a later
// increment maps agents.subagent.grants, its network posture, and the
// settlement settings onto it.
//
// A zero Authority declares nothing and compiles to an empty plan: an absent
// clause is not an uncovered one.
type Authority struct {
	// Writes is the ordered set of world-relative write clauses.
	Writes []WriteClause
	// Network is the declared egress posture; NetworkUnset declares none.
	Network NetworkPosture
	// NetworkHosts is the broker allowlist that qualifies Network, and it is
	// only meaningful when Network is NetworkBroker: the allowlist is part of
	// what "broker" means, so two declarations whose brokers admit different
	// hosts enforce different egress and must not share a digest. The caller
	// supplies hosts already normalized the way its broker matches them — this
	// package is a leaf and does not know that vocabulary — and the compiler
	// only makes the set order-insensitive. It must be empty for every other
	// posture.
	NetworkHosts []string
	// Delta is the declared settlement posture; DeltaUnset declares none.
	Delta DeltaPolicy
	// Containment is how the declaration expects to be enforced. Only the
	// write and network clauses depend on it: the jail monitor establishes
	// them, so a declaration that claims no jail has nothing to enforce them
	// with.
	Containment Containment
	// Fallback is the posture for a clause the assigned monitor cannot enforce,
	// and it is carried onto every assignment the compiler emits. The zero
	// value refuses.
	Fallback Fallback
}

// WriteClause is one declared filesystem authority: a world-relative root and
// the access granted under it. It governs the mutation kinds — write and delete
// — whose PathField lies under the root.
type WriteClause struct {
	// Root is the directory the clause governs, relative to the dispatch world
	// root and slash-separated. An empty root governs the whole world.
	Root string
	// Access is the access granted under Root.
	Access Access
}

// Access is the access a write clause grants under its root.
type Access string

// The access values, matching the grants vocabulary yaah's config uses.
const (
	// AccessUnset is the zero value: no access declared, so the clause cannot
	// be enforced and the compiler refuses it.
	AccessUnset Access = ""
	// AccessReadWrite makes the root writable.
	AccessReadWrite Access = "readwrite"
	// AccessReadOnly declares the root but refuses writes under it.
	AccessReadOnly Access = "readonly"
)

// String renders the access, spelling the zero value "unknown".
func (a Access) String() string {
	if a == AccessUnset {
		return "unknown"
	}
	return string(a)
}

// valid reports whether a names an access the compiler can lower.
func (a Access) valid() bool {
	switch a {
	case AccessReadWrite, AccessReadOnly:
		return true
	case AccessUnset:
		return false
	}
	return false
}

// NetworkPosture is a declared egress posture.
type NetworkPosture string

// The network postures, matching yaah's agents.subagent.network vocabulary.
const (
	// NetworkUnset is the zero value: the declaration says nothing about
	// egress, so it declares no network clause.
	NetworkUnset NetworkPosture = ""
	// NetworkAllowAll permits unrestricted egress (host networking).
	NetworkAllowAll NetworkPosture = "allow_all"
	// NetworkDenyAll refuses egress, leaving the loopback interface only.
	NetworkDenyAll NetworkPosture = "deny_all"
	// NetworkBroker routes egress through an allowlist broker.
	NetworkBroker NetworkPosture = "broker"
)

// String renders the posture, spelling the zero value "unset".
func (n NetworkPosture) String() string {
	if n == NetworkUnset {
		return "unset"
	}
	return string(n)
}

// valid reports whether n names a posture the compiler can lower.
func (n NetworkPosture) valid() bool {
	switch n {
	case NetworkAllowAll, NetworkDenyAll, NetworkBroker:
		return true
	case NetworkUnset:
		return false
	}
	return false
}

// DeltaPolicy is a declared settlement posture for a proposal's delta.
type DeltaPolicy string

// The delta policies. Both are enforced by the settlement monitor at commit;
// the policy says whether the review step is interactive, not whether the
// clause is monitored.
const (
	// DeltaUnset is the zero value: the declaration says nothing about
	// settlement, so it declares no delta clause.
	DeltaUnset DeltaPolicy = ""
	// DeltaReview requires the delta to be reviewed before it settles.
	DeltaReview DeltaPolicy = "review"
	// DeltaApply settles the delta after the settlement checks without an
	// interactive review.
	DeltaApply DeltaPolicy = "apply"
)

// String renders the policy, spelling the zero value "unset".
func (d DeltaPolicy) String() string {
	if d == DeltaUnset {
		return "unset"
	}
	return string(d)
}

// valid reports whether d names a policy the compiler can lower.
func (d DeltaPolicy) valid() bool {
	switch d {
	case DeltaReview, DeltaApply:
		return true
	case DeltaUnset:
		return false
	}
	return false
}

// Containment is how a declaration expects to be enforced.
type Containment string

// The containment values. yaah's configured "paddock" value is ContainmentJail;
// mapping the two is left to the increment that wires a dispatch onto a plan.
const (
	// ContainmentUnset claims no jail. It is not read as a jail by default: a
	// declaration that wants its write and network clauses enforced must claim
	// one, so an omission cannot silently downgrade to an advisory run.
	ContainmentUnset Containment = ""
	// ContainmentJail confines the run before the action — the world's writable
	// roots and its egress posture are established up front.
	ContainmentJail Containment = "jail"
	// ContainmentAdvisory runs on host paths with no confinement, so there is no
	// jail monitor to enforce a write or network clause.
	ContainmentAdvisory Containment = "advisory"
)

// String renders the containment, spelling the zero value "unset".
func (c Containment) String() string {
	if c == ContainmentUnset {
		return "unset"
	}
	return string(c)
}

// valid reports whether c names a containment the compiler can lower.
func (c Containment) valid() bool {
	switch c {
	case ContainmentJail, ContainmentAdvisory:
		return true
	case ContainmentUnset:
		return false
	}
	return false
}

// Compile lowers one authority declaration to a permission plan: one declared
// clause per field of the declaration, each assigned to the monitor that
// enforces it and the timing at which it does.
//
// Write and network clauses are assigned to the jail monitor at pre-action:
// the confined world's writable roots and its egress posture are established
// before the body runs. A declaration that claims no jail — advisory, or one
// that never said — has no monitor for those clauses; they are declared anyway
// and Coverage reports them, so Compile refuses. The delta clause is assigned to
// the settlement monitor at commit, and that monitor lives in the parent world,
// so a discussion of containment does not reach it.
//
// Compile returns an error wrapping ErrInvalidDeclaration for a value it cannot
// lower, and one wrapping ErrClauseUncovered for a well-formed clause nothing
// enforces. It never returns a partial plan.
func Compile(a Authority) (PermissionPlan, error) {
	if err := a.validate(); err != nil {
		return PermissionPlan{}, err
	}
	confined := a.Containment == ContainmentJail
	// An unenforced clause is only accepted when the declaration says so: with
	// FallbackEnforce the assignment is still emitted, so the degraded lane is
	// visible in the plan, its String, and its digest.
	acceptsUnenforced := a.Fallback == FallbackEnforce

	p := PermissionPlan{fallback: a.Fallback}
	for _, w := range a.Writes {
		c, err := lowerWrite(w)
		if err != nil {
			return PermissionPlan{}, err
		}
		p.declare(c)
		if confined || acceptsUnenforced {
			p.assign(Assignment{
				Label:    c.label,
				Clause:   c.match,
				Monitor:  MonitorJail,
				Timing:   TimingPreAction,
				Fallback: a.Fallback,
			})
		}
	}
	if a.Network != NetworkUnset {
		c := clause{
			label: "network(" + a.Network.String() + ")",
			match: effectpolicy.Exact(NetworkKind),
		}
		p.declare(c)
		// The broker's allowlist qualifies that clause, so the plan carries it
		// and the digest covers it: changing one admitted host changes the
		// declaration's identity. validate has already refused hosts under any
		// other posture, so this is empty for every other mode.
		p.networkHosts = canonicalHosts(a.NetworkHosts)
		if confined || acceptsUnenforced {
			p.assign(Assignment{
				Label:    c.label,
				Clause:   c.match,
				Monitor:  MonitorJail,
				Timing:   TimingPreAction,
				Fallback: a.Fallback,
			})
		}
	}
	if a.Delta != DeltaUnset {
		c := clause{
			label: "delta(" + a.Delta.String() + ")",
			match: effectpolicy.Exact(DeltaKind),
		}
		p.declare(c)
		p.assign(Assignment{
			Label:    c.label,
			Clause:   c.match,
			Monitor:  MonitorSettlement,
			Timing:   TimingCommit,
			Fallback: a.Fallback,
		})
	}
	if err := p.Coverage(); err != nil {
		return PermissionPlan{}, err
	}
	return p, nil
}

// validate refuses a declaration-level value the compiler cannot lower.
func (a Authority) validate() error {
	if !a.Fallback.valid() {
		return fmt.Errorf("%w: fallback %q is not one of: refuse, enforce",
			ErrInvalidDeclaration, string(a.Fallback))
	}
	if a.Containment != ContainmentUnset && !a.Containment.valid() {
		return fmt.Errorf("%w: containment %q is not one of: jail, advisory",
			ErrInvalidDeclaration, string(a.Containment))
	}
	if a.Network != NetworkUnset && !a.Network.valid() {
		return fmt.Errorf("%w: network posture %q is not one of: allow_all, deny_all, broker",
			ErrInvalidDeclaration, string(a.Network))
	}
	if len(a.NetworkHosts) > 0 && a.Network != NetworkBroker {
		return fmt.Errorf("%w: network hosts are declared under network posture %q; an allowlist is only meaningful for %q",
			ErrInvalidDeclaration, a.Network.String(), string(NetworkBroker))
	}
	if a.Delta != DeltaUnset && !a.Delta.valid() {
		return fmt.Errorf("%w: delta policy %q is not one of: review, apply",
			ErrInvalidDeclaration, string(a.Delta))
	}
	return nil
}

// canonicalHosts returns the broker allowlist in the one form the plan's digest
// covers: the distinct host names, sorted, so the same allowlist in a different
// order — or with a repeated entry — is the same declaration. Matching
// normalization (trimming, lower-casing, dropping a trailing dot) belongs to the
// caller that knows its broker's vocabulary; an empty entry is not a host, so it
// is dropped exactly as the broker drops it.
func canonicalHosts(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h == "" {
			continue
		}
		out = append(out, h)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// lowerWrite validates one write clause and lowers it to its clause: the label
// naming it and the effect surface it governs.
func lowerWrite(w WriteClause) (clause, error) {
	if !w.Access.valid() {
		return clause{}, fmt.Errorf("%w: write clause %s: access %q is not one of: readwrite, readonly",
			ErrInvalidDeclaration, writeLabel(w.Root, w.Access), string(w.Access))
	}
	root, err := cleanRoot(w.Root)
	if err != nil {
		return clause{}, fmt.Errorf("%w: write clause %s: %w",
			ErrInvalidDeclaration, writeLabel(w.Root, w.Access), err)
	}
	surface := effectpolicy.Exact(WriteKind).Or(effectpolicy.Exact(DeleteKind))
	if root != "" {
		surface = surface.And(effectpolicy.Field(PathField, effectpolicy.StartsWith, root+"/"))
	}
	return clause{label: writeLabel(root, w.Access), match: surface}, nil
}

// writeLabel names a write clause. The root is quoted so a root containing a
// separator, a space, or a comma cannot make two labels read alike.
func writeLabel(root string, access Access) string {
	return "write(" + strconv.Quote(root) + ", " + access.String() + ")"
}

// cleanRoot normalizes a declared root to the world-relative, slash-separated
// directory prefix the clause governs. An empty root is the whole world.
func cleanRoot(root string) (string, error) {
	r := strings.TrimSpace(root)
	if r == "" {
		return "", nil
	}
	if strings.ContainsRune(r, '\\') {
		return "", fmt.Errorf("root %q must be slash-separated", root)
	}
	if path.IsAbs(r) {
		return "", fmt.Errorf("root %q must be world-relative, not absolute", root)
	}
	r = path.Clean(r)
	switch {
	case r == ".":
		return "", fmt.Errorf(`root %q names the world root; use "" for whole-world authority`, root)
	case r == ".." || strings.HasPrefix(r, "../"):
		return "", fmt.Errorf("root %q escapes the dispatch world", root)
	}
	return r, nil
}

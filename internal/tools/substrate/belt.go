// Package substrate holds the tool substrate: the policy and authority
// types the leaf tools are written against, with no dependency on any tool
// implementation. internal/tools depends on this package; this package
// depends on nothing in internal/tools.
package substrate

import (
	"slices"
	"strings"
	"sync"
)

// Belt is the set of tools a session may call.
//
// The belt is a construction constraint, not a cleanup pass: the registry
// consults it on every registration, so a tool outside the belt is never
// installed in the first place. There is no window in which it exists and
// is removed later, and a late registration (the runtime-wired tools
// register after the leaf tools) is refused like any other.
//
// Every tool the model does not use still ships its schema on every
// request, so a belt that names only the tools a lane needs is a prompt
// saving on every call (yaah-w8x).
type Belt struct {
	allowAll bool
	allowed  map[string]bool
	refused  []string
	known    map[string]bool

	// mu guards the three mutation surfaces above. The belt was born a
	// construction-time object, but the lazy toolbelt made Admit a
	// runtime operation: the agent loop executes a batch's tool calls
	// concurrently, so two mounts in one batch write the belt from two
	// goroutines — and an unsynchronized map write is a fatal, not a
	// data race. Every method takes the lock; none of them re-enters.
	mu sync.Mutex
}

// NewBelt builds a belt from the configured allowlist. An empty list
// allows every tool — the substrate primitive's default. The session
// seam does not rely on that: cmd/yaah resolves a curated default
// (orchestratorBeltNames) or an explicit config list before calling
// this, so the belt it hands the registry always names its surface.
func NewBelt(names []string) *Belt {
	if len(names) == 0 {
		return &Belt{allowAll: true}
	}
	b := &Belt{allowed: make(map[string]bool, len(names)), known: make(map[string]bool)}
	for _, name := range names {
		n := strings.TrimSpace(name)
		if n == "" {
			continue
		}
		b.allowed[n] = true
	}
	if len(b.allowed) == 0 {
		b.allowAll = true
	}
	return b
}

// Allows reports whether the belt admits a tool name.
func (b *Belt) Allows(name string) bool {
	if b == nil || b.allowAll {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allowed[name]
}

// Admit adds tool names to the allowlist after construction. Its use is
// the runtime-wired feature lane whose names could not be known when the
// belt was built: an MCP server's tools register only as the server
// connects, and configuring the server is itself the opt-in, so refusing
// its tools at the belt would silently drop a configured feature. Admit is
// a widening operation by design — unlike everything else in this file —
// and lives at the registration site, not in config resolution: the
// caller vouches that the named tool carries its own gate.
// An allow-all belt ignores it — everything is admitted already.
func (b *Belt) Admit(names ...string) {
	if b == nil || b.allowAll {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.allowed == nil {
		b.allowed = make(map[string]bool, len(names))
	}
	trimmed := make([]string, 0, len(names))
	for _, name := range names {
		n := strings.TrimSpace(name)
		if n == "" {
			continue
		}
		trimmed = append(trimmed, n)
		b.allowed[n] = true
		// A prior registration attempt may have refused the name (a
		// retried server); admitting it now means that refusal is
		// stale, and leaving it in Refused() would keep the prompt
		// naming a tool the session now holds. Re-widening retracts.
		if i := slices.Index(b.refused, n); i >= 0 {
			b.refused = slices.Delete(b.refused, i, i+1)
		}
	}
	// The admitted name is known from the moment it is admitted, like
	// a refused one is known from the moment it is refused: if the
	// registration never follows (a server that died between Admit and
	// Register, a future caller breaking the contract), the name must
	// not read as a typo in Unknown() — it is a real tool this session
	// expected and did not get. Know the trimmed name — the same one
	// allowed — or a padded caller would allow "tool" and know " tool".
	// Inlined (b.known writes) rather than calling Know, which would
	// re-take the lock this method holds.
	if b.known == nil {
		b.known = make(map[string]bool, len(trimmed))
	}
	for _, n := range trimmed {
		b.known[n] = true
	}
}

// Know records a tool name the session knows about, whether or not it was
// installed: a tool that exists but was left out for another reason (a
// disabled lane, an absent database, an isolated workspace) is not a typo
// in the allowlist. The registry calls it for every tool it accepts.
func (b *Belt) Know(names ...string) {
	if b == nil || b.allowAll {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.knowLocked(names...)
}

// knowLocked is Know with the lock already held (Admit inlines it via its
// own map writes; Refuse calls it under its own lock section).
func (b *Belt) knowLocked(names ...string) {
	if b.known == nil {
		b.known = make(map[string]bool, len(names))
	}
	for _, name := range names {
		b.known[name] = true
	}
}

// Knows reports whether the belt's accounting has recorded the name:
// a tool that registered under it, or one a gate explicitly marked
// known. Paired with Allows it separates a name the belt dropped but
// that exists (refused at registration, therefore known) from a name
// that never matched any tool — a typo, neither allowed nor known.
func (b *Belt) Knows(name string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.known[name]
}

// Refuse records a tool the belt turned away. The registry calls it when
// a registration is refused; the session prompt and record read the list
// back so the model is told what is missing and the transcript can say
// "the model couldn't" apart from "the model didn't".
func (b *Belt) Refuse(name string) {
	if b == nil || b.allowAll {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !slices.Contains(b.refused, name) {
		b.refused = append(b.refused, name)
	}
	// A refused tool still exists — it is known, so an allowlist that
	// names it is not a typo.
	b.knowLocked(name)
}

// Refused returns the tool names the belt turned away, sorted.
func (b *Belt) Refused() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := slices.Clone(b.refused)
	slices.Sort(out)
	return out
}

// Unknown returns the allowlist names that match no tool the session
// knows about — a typo, which would silently leave the session without a
// tool it asked for. A tool that exists but was not installed for another
// reason (the retained lane is off, the database is absent, the workspace
// is isolated) is known, so it never reads as a typo.
func (b *Belt) Unknown() []string {
	if b == nil || b.allowAll {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var unknown []string
	for name := range b.allowed {
		if !b.known[name] {
			unknown = append(unknown, name)
		}
	}
	slices.Sort(unknown)
	return unknown
}

// Allowed returns the belt's allowlist, sorted.
func (b *Belt) Allowed() []string {
	if b == nil || b.allowAll {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.allowed))
	for name := range b.allowed {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

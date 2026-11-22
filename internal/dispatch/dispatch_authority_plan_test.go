package dispatch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/permissionplan"
)

// TestAuthorityForDispatch pins the config -> declaration mapping. The plan is
// the gate and the identity for the jail lane, so every axis the mapping can
// see is enumerated here rather than discovered by a green end-to-end run.
func TestAuthorityForDispatch(t *testing.T) {
	t.Run("grants become world-relative write clauses", func(t *testing.T) {
		rt := RetainedRuntime{
			Containment: "paddock",
			Network:     "deny_all",
			Grants: []config.GrantConfig{
				{Path: "src", Access: "readwrite"},
				{Path: "docs/api", Access: "readonly"},
			},
		}
		got, err := authorityForDispatch(rt, "developer")
		if err != nil {
			t.Fatalf("authorityForDispatch: %v", err)
		}
		want := []permissionplan.WriteClause{
			{Root: "src", Access: permissionplan.AccessReadWrite},
			{Root: "docs/api", Access: permissionplan.AccessReadOnly},
		}
		if !reflect.DeepEqual(got.Writes, want) {
			t.Errorf("writes = %+v, want %+v", got.Writes, want)
		}
		if got.Network != permissionplan.NetworkDenyAll {
			t.Errorf("network = %q, want deny_all", got.Network)
		}
		if got.Delta != permissionplan.DeltaApply {
			t.Errorf("delta = %q, want apply: the orchestrator invoking select/apply is the review", got.Delta)
		}
		if got.Containment != permissionplan.ContainmentJail || got.Fallback != permissionplan.FallbackRefuse {
			t.Errorf("containment/fallback = %q/%q, want jail/refuse", got.Containment, got.Fallback)
		}
	})

	t.Run("the delta clause is enforced by the settlement monitor", func(t *testing.T) {
		plan, err := compileDispatchPlan(RetainedRuntime{Containment: "paddock"}, "developer")
		if err != nil {
			t.Fatalf("compileDispatchPlan: %v", err)
		}
		found := false
		for _, a := range plan.Assignments() {
			if a.Monitor != permissionplan.MonitorSettlement {
				continue
			}
			found = true
			if a.Timing != permissionplan.TimingCommit {
				t.Errorf("delta assignment timing = %q, want commit", a.Timing)
			}
			if want := "delta(" + permissionplan.DeltaApply.String() + ")"; a.Label != want {
				t.Errorf("delta assignment label = %q, want %q", a.Label, want)
			}
		}
		if !found {
			t.Fatal("a declared delta posture must compile to a settlement assignment: one declaration, two monitors")
		}
	})

	t.Run("role grants override the session set", func(t *testing.T) {
		rt := RetainedRuntime{
			Containment: "jail",
			Grants:      []config.GrantConfig{{Path: "src", Access: "readwrite"}},
			GrantsForRole: func(role string) []config.GrantConfig {
				if role == "reviewer" {
					return []config.GrantConfig{{Path: "docs", Access: "readonly"}}
				}
				return nil
			},
		}
		got, err := authorityForDispatch(rt, "reviewer")
		if err != nil {
			t.Fatalf("authorityForDispatch: %v", err)
		}
		want := []permissionplan.WriteClause{{Root: "docs", Access: permissionplan.AccessReadOnly}}
		if !reflect.DeepEqual(got.Writes, want) {
			t.Errorf("writes = %+v, want the role override %+v", got.Writes, want)
		}
	})

	t.Run("no grants declares no write clause", func(t *testing.T) {
		got, err := authorityForDispatch(RetainedRuntime{Containment: "auto"}, "developer")
		if err != nil {
			t.Fatalf("authorityForDispatch: %v", err)
		}
		if len(got.Writes) != 0 {
			t.Errorf("writes = %+v, want none", got.Writes)
		}
	})

	network := []struct {
		mode string
		want permissionplan.NetworkPosture
	}{
		{mode: "", want: permissionplan.NetworkUnset},
		{mode: "allow_all", want: permissionplan.NetworkAllowAll},
		{mode: "deny_all", want: permissionplan.NetworkDenyAll},
		{mode: "broker", want: permissionplan.NetworkBroker},
		{mode: " DENY_ALL ", want: permissionplan.NetworkDenyAll},
	}
	for _, tc := range network {
		t.Run("network "+tc.mode, func(t *testing.T) {
			got, err := authorityForDispatch(RetainedRuntime{Containment: "paddock", Network: tc.mode}, "developer")
			if err != nil {
				t.Fatalf("authorityForDispatch: %v", err)
			}
			if got.Network != tc.want {
				t.Errorf("network = %q, want %q", got.Network, tc.want)
			}
		})
	}

	// The containment axis decides whether a jail monitor exists, so both of its
	// halves are part of the contract: the placement, and the fallback that
	// makes an advisory lane's clauses compile instead of refusing them.
	containment := []struct {
		setting string
		wantC   permissionplan.Containment
		wantF   permissionplan.Fallback
	}{
		{setting: "paddock", wantC: permissionplan.ContainmentJail, wantF: permissionplan.FallbackRefuse},
		{setting: "jail", wantC: permissionplan.ContainmentJail, wantF: permissionplan.FallbackRefuse},
		{setting: "auto", wantC: permissionplan.ContainmentJail, wantF: permissionplan.FallbackRefuse},
		{setting: "", wantC: permissionplan.ContainmentJail, wantF: permissionplan.FallbackRefuse},
		{setting: " Jail ", wantC: permissionplan.ContainmentJail, wantF: permissionplan.FallbackRefuse},
		{setting: "off", wantC: permissionplan.ContainmentAdvisory, wantF: permissionplan.FallbackEnforce},
		{setting: "none", wantC: permissionplan.ContainmentAdvisory, wantF: permissionplan.FallbackEnforce},
		{setting: "advisory", wantC: permissionplan.ContainmentAdvisory, wantF: permissionplan.FallbackEnforce},
	}
	for _, tc := range containment {
		t.Run("containment "+tc.setting, func(t *testing.T) {
			got, err := authorityForDispatch(RetainedRuntime{Containment: tc.setting}, "developer")
			if err != nil {
				t.Fatalf("authorityForDispatch: %v", err)
			}
			if got.Containment != tc.wantC || got.Fallback != tc.wantF {
				t.Errorf("containment/fallback = %q/%q, want %q/%q",
					got.Containment, got.Fallback, tc.wantC, tc.wantF)
			}
		})
	}

	t.Run("unknown access is refused", func(t *testing.T) {
		rt := RetainedRuntime{Containment: "paddock", Grants: []config.GrantConfig{{Path: "src", Access: "write"}}}
		if _, err := authorityForDispatch(rt, "developer"); err == nil {
			t.Fatal("an unknown grant access must be refused")
		} else if !strings.Contains(err.Error(), "unknown access") {
			t.Fatalf("error = %v, want it to name the access", err)
		}
	})

	t.Run("unknown network is refused", func(t *testing.T) {
		rt := RetainedRuntime{Containment: "paddock", Network: "borker"}
		if _, err := authorityForDispatch(rt, "developer"); err == nil {
			t.Fatal("an unknown network mode must be refused")
		} else if !strings.Contains(err.Error(), "agents.subagent.network") {
			t.Fatalf("error = %v, want it to name the config key", err)
		}
	})

	t.Run("unknown containment is refused", func(t *testing.T) {
		rt := RetainedRuntime{Containment: "sandbox-ish"}
		if _, err := authorityForDispatch(rt, "developer"); err == nil {
			t.Fatal("an unknown containment must be refused, not degraded")
		} else if !strings.Contains(err.Error(), "agents.subagent.containment") {
			t.Fatalf("error = %v, want it to name the config key", err)
		}
	})
}

// TestResolveDispatchAuthority_Oracle is the byte-identical oracle for the
// refactor: for every config in the matrix it recomputes the mount shape from
// the retained lowering primitives (resolveContainerGrants and
// lowerContainerNetwork, unchanged by this increment) and requires
// resolveDispatchAuthority to produce exactly that — no reshaped roots, no
// reshaped mask, no reshaped network — while additionally carrying a compiled
// plan digest.
func TestResolveDispatchAuthority_Oracle(t *testing.T) {
	grantSets := []struct {
		name   string
		grants []config.GrantConfig
	}{
		{name: "no grants"},
		{name: "readwrite", grants: []config.GrantConfig{{Path: "src", Access: "readwrite"}}},
		{name: "readonly", grants: []config.GrantConfig{{Path: "docs", Access: "readonly"}}},
		{name: "mixed", grants: []config.GrantConfig{
			{Path: "src", Access: "readwrite"},
			{Path: "docs/api", Access: "readonly"},
		}},
	}
	networkSets := []struct {
		name  string
		mode  string
		hosts []string
	}{
		{name: "default network"},
		{name: "allow_all", mode: "allow_all"},
		{name: "deny_all", mode: "deny_all"},
		{name: "broker", mode: "broker", hosts: []string{" API.Example.com. "}},
	}
	containments := []string{"", "auto", "paddock", "jail", "off", "none", "advisory"}

	for _, gs := range grantSets {
		for _, ns := range networkSets {
			for _, containment := range containments {
				name := gs.name + "/" + ns.name + "/containment=" + containment
				t.Run(name, func(t *testing.T) {
					rt := RetainedRuntime{
						Network:         ns.mode,
						NetworkHosts:    ns.hosts,
						Containment:     containment,
						ContainerWorlds: true,
						Grants:          gs.grants,
						DispatchWorlds:  &DispatchWorlds{Config: sessionTestConfig()},
					}

					got, err := resolveDispatchAuthority(rt, "developer")
					if err != nil {
						t.Fatalf("resolveDispatchAuthority: %v", err)
					}

					wantRoots, wantMask, wantNetwork, wantHosts := oracleLowering(t, gs.grants, ns.mode, ns.hosts)
					if !reflect.DeepEqual(got.writableRoots, wantRoots) {
						t.Errorf("writable roots = %v, want %v", got.writableRoots, wantRoots)
					}
					if got.maskWorkspace != wantMask {
						t.Errorf("mask workspace = %v, want %v", got.maskWorkspace, wantMask)
					}
					if got.network != wantNetwork {
						t.Errorf("network = %q, want %q", got.network, wantNetwork)
					}
					if !reflect.DeepEqual(got.brokerHosts, wantHosts) {
						t.Errorf("broker hosts = %v, want %v", got.brokerHosts, wantHosts)
					}

					// The declaration compiled: the gate passed and the identity
					// is citable.
					if digest := got.planIdentity(); len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
						t.Fatalf("plan digest = %q, want 64 hex characters", digest)
					}

					// Determinism: the same declaration always compiles to the
					// same digest.
					again, err := resolveDispatchAuthority(rt, "developer")
					if err != nil {
						t.Fatalf("resolveDispatchAuthority (second): %v", err)
					}
					if again.planIdentity() != got.planIdentity() {
						t.Errorf("digest changed between identical resolutions: %q != %q",
							again.planIdentity(), got.planIdentity())
					}
				})
			}
		}
	}
}

// oracleLowering recomputes the pre-refactor mount shape from the lowering
// primitives, independently of resolveDispatchAuthority.
func oracleLowering(t *testing.T, grants []config.GrantConfig, mode string, hosts []string) (roots []string, mask bool, network string, brokerHosts []string) {
	t.Helper()
	if len(grants) > 0 {
		resolved, err := resolveContainerGrants(grants, "/workspace")
		if err != nil {
			t.Fatalf("resolveContainerGrants: %v", err)
		}
		roots = resolved.Writable
		mask = len(resolved.Writable) == 0
	}
	network, brokerHosts, err := lowerContainerNetwork(mode, hosts)
	if err != nil {
		t.Fatalf("lowerContainerNetwork: %v", err)
	}
	return roots, mask, network, brokerHosts
}

// TestResolveDispatchAuthority_StillRefuses pins the configs that are errors
// today: the plan gate must not turn a refused grant into an accepted world.
func TestResolveDispatchAuthority_StillRefuses(t *testing.T) {
	cases := []struct {
		name string
		rt   RetainedRuntime
		want string
	}{
		{
			name: "absolute grant",
			rt:   RetainedRuntime{Grants: []config.GrantConfig{{Path: "/etc", Access: "readwrite"}}},
			want: "world-relative",
		},
		{
			name: "escaping grant",
			rt:   RetainedRuntime{Grants: []config.GrantConfig{{Path: "../etc", Access: "readwrite"}}},
			want: "escapes",
		},
		{
			name: "world-root grant",
			rt:   RetainedRuntime{Grants: []config.GrantConfig{{Path: ".", Access: "readwrite"}}},
			want: "world root",
		},
		{
			name: "unknown grant access",
			rt:   RetainedRuntime{Grants: []config.GrantConfig{{Path: "src", Access: "write"}}},
			want: "unknown access",
		},
		{
			name: "unknown network",
			rt:   RetainedRuntime{Network: "borker"},
			want: "agents.subagent.network",
		},
		{
			name: "unknown containment",
			rt:   RetainedRuntime{Containment: "sandbox-ish"},
			want: "agents.subagent.containment",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.rt.ContainerWorlds = true
			tc.rt.DispatchWorlds = &DispatchWorlds{Config: sessionTestConfig()}
			_, err := resolveDispatchAuthority(tc.rt, "developer")
			if err == nil {
				t.Fatal("the config must still be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDispatchPlanGate_FailClosed proves the coverage gate is live in the jail
// lane: a declaration that claims write authority without a jail to enforce it
// does not compile, so no dispatch world can be built from one. The mapping
// above never emits this combination (an advisory placement sets
// FallbackEnforce), which is exactly why the gate is asserted directly — the
// refusal must come from the compiler, not from the mapping happening to be
// careful.
func TestDispatchPlanGate_FailClosed(t *testing.T) {
	declared := permissionplan.Authority{
		Writes: []permissionplan.WriteClause{{Root: "src", Access: permissionplan.AccessReadWrite}},
	}

	uncovered := []struct {
		name string
		a    permissionplan.Authority
	}{
		{name: "write clause, containment unset, refuse", a: declared},
		{
			name: "write clause, advisory, refuse",
			a:    withContainment(declared, permissionplan.ContainmentAdvisory, permissionplan.FallbackRefuse),
		},
		{
			name: "network clause, containment unset",
			a:    permissionplan.Authority{Network: permissionplan.NetworkBroker},
		},
		{
			name: "network clause, advisory, refuse",
			a: permissionplan.Authority{
				Network:     permissionplan.NetworkBroker,
				Containment: permissionplan.ContainmentAdvisory,
				Fallback:    permissionplan.FallbackRefuse,
			},
		},
	}
	for _, tc := range uncovered {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			plan, err := permissionplan.Compile(tc.a)
			if err == nil {
				t.Fatalf("Compile accepted an unenforceable declaration: %s", plan)
			}
			if !errors.Is(err, permissionplan.ErrClauseUncovered) {
				t.Fatalf("error = %v, want ErrClauseUncovered", err)
			}
		})
	}

	for _, tc := range []struct {
		name string
		a    permissionplan.Authority
	}{
		{name: "jail", a: withContainment(declared, permissionplan.ContainmentJail, permissionplan.FallbackRefuse)},
		{name: "advisory accepting enforcement", a: withContainment(declared, permissionplan.ContainmentAdvisory, permissionplan.FallbackEnforce)},
	} {
		t.Run("compiled/"+tc.name, func(t *testing.T) {
			plan, err := permissionplan.Compile(tc.a)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if err := plan.Coverage(); err != nil {
				t.Fatalf("Coverage: %v", err)
			}
			if plan.Digest() == "" {
				t.Fatal("a compiled plan must have a digest")
			}
		})
	}
}

func withContainment(a permissionplan.Authority, c permissionplan.Containment, f permissionplan.Fallback) permissionplan.Authority {
	a.Containment, a.Fallback = c, f
	return a
}

// TestSettlementCitesJailPlanDigest is the "one declaration, two lanes"
// invariant: the digest the jail lane compiles for a config is the digest the
// container seal persists, so a proposal sealed by that world carries the same
// declaration identity into settlement. The two lanes share a declaration only
// if their digests agree; a mismatch here would mean one of them lowered its own
// copy of the config.
func TestSettlementCitesJailPlanDigest(t *testing.T) {
	f := newDeltaFixture(t)
	rt := RetainedRuntime{
		Containment:     "paddock",
		Network:         "deny_all",
		Grants:          []config.GrantConfig{{Path: "src", Access: "readwrite"}},
		ContainerWorlds: true,
		DispatchWorlds:  &DispatchWorlds{Config: sessionTestConfig()},
	}
	jail, err := resolveDispatchAuthority(rt, "developer")
	if err != nil {
		t.Fatalf("resolveDispatchAuthority: %v", err)
	}
	plan, err := compileDispatchPlan(rt, "developer")
	if err != nil {
		t.Fatalf("compileDispatchPlan: %v", err)
	}
	if jail.planIdentity() != plan.Digest() {
		t.Fatalf("the world's plan digest %q is not the compiled declaration %q",
			jail.planIdentity(), plan.Digest())
	}

	// Seal exactly as the container lane does: the world's authority digest goes
	// into the manifest's metadata beside it.
	if _, _, err := persistGitDelta(context.Background(), f.repo, "proposal:two-lanes",
		rehydrateDelta(), "", "", "", f.sessionID, f.rootID, "host",
		f.basisState, f.basisDigest, jail.planIdentity()); err != nil {
		t.Fatalf("persistGitDelta: %v", err)
	}
	reg, warnings := LoadProposalRegistry(f.repo, f.fx.mgr, f.rootID, f.sessionID)
	if len(warnings) != 0 || len(reg.all()) != 1 {
		t.Fatalf("proposals = %d, warnings %v; want one", len(reg.all()), warnings)
	}
	if got := reg.all()[0].authorityPlanDigest; got != jail.planIdentity() {
		t.Fatalf("settlement cites %q, want the jail lane's %q", got, jail.planIdentity())
	}
}

// TestCompileDispatchPlan_DigestIdentity pins the digest as a declaration
// identity: stable for one declaration, different for a different one.
func TestCompileDispatchPlan_DigestIdentity(t *testing.T) {
	base := RetainedRuntime{
		Containment: "paddock",
		Network:     "deny_all",
		Grants:      []config.GrantConfig{{Path: "src", Access: "readwrite"}},
	}
	first, err := compileDispatchPlan(base, "developer")
	if err != nil {
		t.Fatalf("compileDispatchPlan: %v", err)
	}
	second, err := compileDispatchPlan(base, "developer")
	if err != nil {
		t.Fatalf("compileDispatchPlan (second): %v", err)
	}
	if first.Digest() != second.Digest() {
		t.Fatalf("digest is not deterministic: %q != %q", first.Digest(), second.Digest())
	}

	wider := base
	wider.Grants = []config.GrantConfig{{Path: "src", Access: "readwrite"}, {Path: "docs", Access: "readonly"}}
	other, err := compileDispatchPlan(wider, "developer")
	if err != nil {
		t.Fatalf("compileDispatchPlan (wider): %v", err)
	}
	if other.Digest() == first.Digest() {
		t.Fatal("two different declarations must not share a digest")
	}
}

// TestAuthorityFactPayloadShape pins the authority fact's key set, so the trace
// consumer and the settlement lane can rely on it (worldFactPayload has the
// same guard).
func TestAuthorityFactPayloadShape(t *testing.T) {
	got := authorityFactPayload(dispatchAuthority{
		writableRoots: []string{"/workspace/src"},
		network:       "broker",
		planDigest:    "abc123",
	})
	want := map[string]bool{"plan_digest": true, "network_posture": true, "shaped": true, "writable_roots": true}
	if len(got) != len(want) {
		t.Fatalf("payload keys = %v, want %v", got, want)
	}
	for k := range got {
		if !want[k] {
			t.Errorf("unexpected authority-fact key %q", k)
		}
	}
	if got["plan_digest"] != "abc123" || got["network_posture"] != "broker" || got["shaped"] != true || got["writable_roots"] != 1 {
		t.Errorf("payload = %v", got)
	}

	unshaped := authorityFactPayload(dispatchAuthority{network: "host", planDigest: "def456"})
	if unshaped["shaped"] != false || unshaped["writable_roots"] != 0 {
		t.Errorf("unshaped payload = %v", unshaped)
	}
}

// TestBackslashGrantPath_CompilesAndMounts is the Windows-authored grant
// spelling: config validation accepts `src\foo`, so it must compile into a plan
// and lower into exactly the mount its forward-slash spelling does. Without the
// canonical grant path seam the plan refused the backslash and the dispatch
// failed before the world started.
func TestBackslashGrantPath_CompilesAndMounts(t *testing.T) {
	rt := RetainedRuntime{
		Containment:     "paddock",
		Network:         "deny_all",
		Grants:          []config.GrantConfig{{Path: `src\foo`, Access: "readwrite"}},
		ContainerWorlds: true,
		DispatchWorlds:  &DispatchWorlds{Config: sessionTestConfig()},
	}
	slash := rt
	slash.Grants = []config.GrantConfig{{Path: "src/foo", Access: "readwrite"}}

	// The declaration: one clause, in the canonical spelling.
	auth, err := authorityForDispatch(rt, "developer")
	if err != nil {
		t.Fatalf("authorityForDispatch(%q): %v", `src\foo`, err)
	}
	wantWrites := []permissionplan.WriteClause{{Root: "src/foo", Access: permissionplan.AccessReadWrite}}
	if !reflect.DeepEqual(auth.Writes, wantWrites) {
		t.Fatalf("writes = %+v, want the canonical spelling %+v", auth.Writes, wantWrites)
	}

	// The plan compiles, and it is the same plan the forward-slash spelling
	// compiles to.
	plan, err := compileDispatchPlan(rt, "developer")
	if err != nil {
		t.Fatalf("compileDispatchPlan(%q): %v", `src\foo`, err)
	}
	same, err := compileDispatchPlan(slash, "developer")
	if err != nil {
		t.Fatalf("compileDispatchPlan(%q): %v", "src/foo", err)
	}
	if plan.Digest() != same.Digest() {
		t.Fatalf("backslash digest %s != forward-slash digest %s: the spellings must name one declaration",
			plan.Digest(), same.Digest())
	}

	// The mount agrees with the declaration.
	got, err := resolveDispatchAuthority(rt, "developer")
	if err != nil {
		t.Fatalf("resolveDispatchAuthority(%q): %v", `src\foo`, err)
	}
	want, err := resolveDispatchAuthority(slash, "developer")
	if err != nil {
		t.Fatalf("resolveDispatchAuthority(%q): %v", "src/foo", err)
	}
	if !reflect.DeepEqual(got.writableRoots, []string{"/workspace/src/foo"}) {
		t.Fatalf("writable roots = %v, want [/workspace/src/foo]", got.writableRoots)
	}
	if !reflect.DeepEqual(got.writableRoots, want.writableRoots) || got.planIdentity() != want.planIdentity() {
		t.Fatalf("backslash lowering = %v/%s, forward-slash = %v/%s; want one declaration",
			got.writableRoots, got.planIdentity(), want.writableRoots, want.planIdentity())
	}
}

// TestCanonicalGrantPath pins the seam itself: every spelling config validation
// accepts for one directory lowers to one path, and the spellings it refuses
// still refuse.
func TestCanonicalGrantPath(t *testing.T) {
	same := []string{"src/foo", "src\\foo", " src\\foo ", "src//foo", "src/./foo", `src\foo\bar\..`}
	for _, raw := range same {
		if got := canonicalGrantPath(raw); got != "src/foo" {
			t.Errorf("canonicalGrantPath(%q) = %q, want src/foo", raw, got)
		}
	}
	if _, err := resolveContainerGrants([]config.GrantConfig{{Path: `..\..\etc`, Access: "readwrite"}}, "/workspace"); err == nil {
		t.Fatal(`a backslash grant that escapes the world must still refuse`)
	}
}

// TestAuthorityForDispatch_BrokerHosts pins the broker allowlist as part of the
// declaration: the admitted host set is what the Croft backend enforces, so two
// dispatches with different allowlists must not cite one plan digest.
func TestAuthorityForDispatch_BrokerHosts(t *testing.T) {
	rt := RetainedRuntime{
		Containment:  "paddock",
		Network:      "broker",
		NetworkHosts: []string{" API.Example.com. ", "docs.example.com"},
	}
	digest := func(t *testing.T, rt RetainedRuntime) string {
		t.Helper()
		plan, err := compileDispatchPlan(rt, "developer")
		if err != nil {
			t.Fatalf("compileDispatchPlan: %v", err)
		}
		return plan.Digest()
	}

	auth, err := authorityForDispatch(rt, "developer")
	if err != nil {
		t.Fatalf("authorityForDispatch: %v", err)
	}
	wantHosts := []string{"api.example.com", "docs.example.com"}
	if !reflect.DeepEqual(auth.NetworkHosts, wantHosts) {
		t.Fatalf("network hosts = %v, want the provisioning normalization %v", auth.NetworkHosts, wantHosts)
	}

	base := digest(t, rt)

	t.Run("changing one allowed host changes the digest", func(t *testing.T) {
		other := rt
		other.NetworkHosts = []string{"api.example.com", "other.example.com"}
		if got := digest(t, other); got == base {
			t.Fatalf("two allowlists share the digest %s", got)
		}
	})

	t.Run("the allowlist is a set: order and repeats do not change it", func(t *testing.T) {
		for _, hosts := range [][]string{
			{"docs.example.com", "api.example.com"},
			{"docs.example.com", "API.EXAMPLE.COM.", "api.example.com"},
		} {
			permuted := rt
			permuted.NetworkHosts = hosts
			if got := digest(t, permuted); got != base {
				t.Fatalf("allowlist %v digest = %s, want %s", hosts, got, base)
			}
		}
	})

	t.Run("hosts outside the broker posture do not reach the plan", func(t *testing.T) {
		for _, mode := range []string{"", "allow_all", "deny_all"} {
			withHosts := RetainedRuntime{
				Containment:  "paddock",
				Network:      mode,
				NetworkHosts: []string{"api.example.com"},
			}
			got, err := authorityForDispatch(withHosts, "developer")
			if err != nil {
				t.Fatalf("authorityForDispatch(%q): %v", mode, err)
			}
			if len(got.NetworkHosts) != 0 {
				t.Fatalf("mode %q carried hosts %v; the declaration claims an allowlist no broker enforces",
					mode, got.NetworkHosts)
			}
			without := withHosts
			without.NetworkHosts = nil
			if digest(t, withHosts) != digest(t, without) {
				t.Fatalf("mode %q: a dropped allowlist perturbed the digest", mode)
			}
		}
	})
}

// TestWorktreeAuthorityForDispatch pins the worktree world's declaration: the
// same grants, egress posture, and settlement posture the container lane reads,
// restated with the containment posture a worktree dispatch actually enforces —
// advisory, because that world establishes no jail, plus FallbackEnforce so the
// declaration's clauses still compile instead of being refused on a lane with
// no jail monitor.
func TestWorktreeAuthorityForDispatch(t *testing.T) {
	rt := RetainedRuntime{
		Containment:  "paddock",
		Network:      "broker",
		NetworkHosts: []string{" API.Example.com. "},
		Grants: []config.GrantConfig{
			{Path: "src", Access: "readwrite"},
			{Path: "docs/api", Access: "readonly"},
		},
	}
	got, err := worktreeAuthorityForDispatch(rt, "developer")
	if err != nil {
		t.Fatalf("worktreeAuthorityForDispatch: %v", err)
	}

	// Every authority axis except containment is the container lane's, read by
	// the same mapping, so the two worlds cite one declaration.
	container, err := authorityForDispatch(rt, "developer")
	if err != nil {
		t.Fatalf("authorityForDispatch: %v", err)
	}
	if !reflect.DeepEqual(got.Writes, container.Writes) {
		t.Errorf("writes = %+v, want the container lane's %+v", got.Writes, container.Writes)
	}
	if got.Network != container.Network || !reflect.DeepEqual(got.NetworkHosts, container.NetworkHosts) {
		t.Errorf("network/allowlist = %q/%v, want %q/%v",
			got.Network, got.NetworkHosts, container.Network, container.NetworkHosts)
	}
	if got.Delta != container.Delta {
		t.Errorf("delta = %q, want the container lane's %q: one settlement posture", got.Delta, container.Delta)
	}

	// Containment is the one axis restated. "paddock" would otherwise claim a
	// jail this world never established.
	if got.Containment != permissionplan.ContainmentAdvisory || got.Fallback != permissionplan.FallbackEnforce {
		t.Errorf("containment/fallback = %q/%q, want advisory/enforce", got.Containment, got.Fallback)
	}

	// Whatever the configured containment, the worktree declaration is advisory
	// and accepts its clauses: the posture is a property of the world, not of the
	// config key the container lane maps.
	for _, setting := range []string{"", "auto", "paddock", "jail", "off", "none", "advisory"} {
		t.Run("containment "+setting, func(t *testing.T) {
			withGrant := RetainedRuntime{
				Containment: setting,
				Grants:      []config.GrantConfig{{Path: "src", Access: "readwrite"}},
			}
			got, err := worktreeAuthorityForDispatch(withGrant, "developer")
			if err != nil {
				t.Fatalf("worktreeAuthorityForDispatch: %v", err)
			}
			if got.Containment != permissionplan.ContainmentAdvisory || got.Fallback != permissionplan.FallbackEnforce {
				t.Fatalf("containment/fallback = %q/%q, want advisory/enforce", got.Containment, got.Fallback)
			}
			// The gate still runs: the grant is declared, assigned, and covered.
			if _, err := compileWorktreePlan(withGrant, "developer"); err != nil {
				t.Fatalf("compileWorktreePlan: %v", err)
			}
		})
	}

	t.Run("an uncompilable declaration is refused, not cited", func(t *testing.T) {
		if _, err := resolveWorktreePlanDigest(RetainedRuntime{Network: "borker"}, "developer"); err == nil {
			t.Fatal("an unknown network mode must refuse the worktree declaration")
		} else if !strings.Contains(err.Error(), "agents.subagent.network") {
			t.Fatalf("error = %v, want it to name the config key", err)
		}
		badGrant := RetainedRuntime{Grants: []config.GrantConfig{{Path: "src", Access: "write"}}}
		if _, err := resolveWorktreePlanDigest(badGrant, "developer"); err == nil {
			t.Fatal("an unknown grant access must refuse the worktree declaration")
		}
	})
}

// TestWorktreePlanDigest_AgreesWithContainerWhereTheWorldAgrees pins the one
// deliberate difference between the two settlement lanes' digests. The lanes
// compile one declaration, so their digests agree exactly where both worlds
// enforce the same containment posture — containment off/none/advisory, which
// the container lane also lowers to advisory/enforce. Everywhere else the
// container declares a jail and the worktree cannot, so the digests differ, and
// they must: for that config the two worlds enforce different declarations.
func TestWorktreePlanDigest_AgreesWithContainerWhereTheWorldAgrees(t *testing.T) {
	grantSets := []struct {
		name   string
		grants []config.GrantConfig
	}{
		{name: "no grants"},
		{name: "readwrite", grants: []config.GrantConfig{{Path: "src", Access: "readwrite"}}},
		{name: "mixed", grants: []config.GrantConfig{
			{Path: "src", Access: "readwrite"},
			{Path: "docs/api", Access: "readonly"},
		}},
	}
	networkSets := []struct {
		name  string
		mode  string
		hosts []string
	}{
		{name: "default network"},
		{name: "allow_all", mode: "allow_all"},
		{name: "deny_all", mode: "deny_all"},
		{name: "broker", mode: "broker", hosts: []string{" API.Example.com. "}},
	}
	// agree reports whether the two worlds enforce the same declaration for the
	// containment setting: the container lane declares advisory only when the
	// config does.
	containments := []struct {
		setting string
		agree   bool
	}{
		{setting: "", agree: false},
		{setting: "auto", agree: false},
		{setting: "paddock", agree: false},
		{setting: "jail", agree: false},
		{setting: "off", agree: true},
		{setting: "none", agree: true},
		{setting: "advisory", agree: true},
	}

	for _, gs := range grantSets {
		for _, ns := range networkSets {
			for _, tc := range containments {
				name := gs.name + "/" + ns.name + "/containment=" + tc.setting
				t.Run(name, func(t *testing.T) {
					rt := RetainedRuntime{
						Network:      ns.mode,
						NetworkHosts: ns.hosts,
						Containment:  tc.setting,
						Grants:       gs.grants,
					}
					container, err := resolveDispatchAuthority(rt, "developer")
					if err != nil {
						t.Fatalf("resolveDispatchAuthority: %v", err)
					}
					worktree, err := resolveWorktreePlanDigest(rt, "developer")
					if err != nil {
						t.Fatalf("resolveWorktreePlanDigest: %v", err)
					}
					if len(worktree) != 64 || strings.Trim(worktree, "0123456789abcdef") != "" {
						t.Fatalf("worktree plan digest = %q, want 64 hex characters", worktree)
					}
					if tc.agree && worktree != container.planIdentity() {
						t.Fatalf("digests differ where both worlds enforce one declaration: worktree %s != container %s",
							worktree, container.planIdentity())
					}
					if !tc.agree && worktree == container.planIdentity() {
						t.Fatalf("digests agree for containment %q, but the worlds enforce different declarations: %s",
							tc.setting, worktree)
					}

					// Determinism: a restatement of one declaration is one digest.
					again, err := resolveWorktreePlanDigest(rt, "developer")
					if err != nil {
						t.Fatalf("resolveWorktreePlanDigest (second): %v", err)
					}
					if again != worktree {
						t.Fatalf("digest changed between identical resolutions: %q != %q", again, worktree)
					}
				})
			}
		}
	}
}

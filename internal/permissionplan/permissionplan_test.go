package permissionplan

import (
	"errors"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/effectpolicy"
)

// renderAssignments reduces a plan's assignments to one stable line each, so a
// table can state the expected monitor, timing, clause, and fallback without
// spelling out the effect surface every time.
func renderAssignments(p PermissionPlan) []string {
	out := make([]string, 0, len(p.Assignments()))
	for _, a := range p.Assignments() {
		out = append(out, a.Monitor.String()+"/"+a.Timing.String()+" "+a.Label+" "+a.Fallback.String())
	}
	return out
}

// jailedAuthority is the reference declaration most tests build on: a confined
// world with one read-write root, loopback egress, and a reviewed delta.
func jailedAuthority() Authority {
	return Authority{
		Writes:      []WriteClause{{Root: "src", Access: AccessReadWrite}},
		Network:     NetworkDenyAll,
		Delta:       DeltaReview,
		Containment: ContainmentJail,
	}
}

func TestCompileAssignments(t *testing.T) {
	cases := []struct {
		name string
		a    Authority
		want []string
	}{
		{
			name: "jailed world: writes and egress before the action, delta at commit",
			a:    jailedAuthority(),
			want: []string{
				`jail/pre_action write("src", readwrite) refuse`,
				`jail/pre_action network(deny_all) refuse`,
				`settlement/commit delta(review) refuse`,
			},
		},
		{
			name: "whole-world write root",
			a: Authority{
				Writes:      []WriteClause{{Access: AccessReadWrite}},
				Containment: ContainmentJail,
			},
			want: []string{`jail/pre_action write("", readwrite) refuse`},
		},
		{
			name: "read-only root still needs the jail",
			a: Authority{
				Writes:      []WriteClause{{Root: "docs", Access: AccessReadOnly}},
				Containment: ContainmentJail,
			},
			want: []string{`jail/pre_action write("docs", readonly) refuse`},
		},
		{
			name: "nested root is cleaned",
			a: Authority{
				Writes:      []WriteClause{{Root: "src/./internal//", Access: AccessReadWrite}},
				Containment: ContainmentJail,
			},
			want: []string{`jail/pre_action write("src/internal", readwrite) refuse`},
		},
		{
			name: "brokered egress",
			a: Authority{
				Network:     NetworkBroker,
				Containment: ContainmentJail,
			},
			want: []string{"jail/pre_action network(broker) refuse"},
		},
		{
			name: "delta alone needs no containment: settlement acts in the parent world",
			a:    Authority{Delta: DeltaApply},
			want: []string{"settlement/commit delta(apply) refuse"},
		},
		{
			name: "an absent clause is not an uncovered clause",
			a:    Authority{},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.a)
			if err != nil {
				t.Fatalf("Compile() error = %v; want nil", err)
			}
			if err := p.Coverage(); err != nil {
				t.Fatalf("Coverage() = %v; want nil", err)
			}
			got := renderAssignments(p)
			if len(got) != len(tc.want) {
				t.Fatalf("assignments = %v; want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("assignment %d = %q; want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCoverageNamesTheUncoveredClause(t *testing.T) {
	cases := []struct {
		name  string
		a     Authority
		label string
	}{
		{
			name: "advisory containment cannot enforce a write clause",
			a: Authority{
				Writes:      []WriteClause{{Root: "src", Access: AccessReadWrite}},
				Containment: ContainmentAdvisory,
			},
			label: `write("src", readwrite)`,
		},
		{
			name:  "an unclaimed containment is not a jail",
			a:     Authority{Writes: []WriteClause{{Root: "src", Access: AccessReadWrite}}},
			label: `write("src", readwrite)`,
		},
		{
			name: "advisory containment cannot enforce an egress posture either",
			a: Authority{
				Network:     NetworkDenyAll,
				Containment: ContainmentAdvisory,
			},
			label: "network(deny_all)",
		},
		{
			name: "the first uncovered clause is the one named",
			a: Authority{
				Writes: []WriteClause{
					{Root: "src", Access: AccessReadWrite},
					{Root: "docs", Access: AccessReadOnly},
				},
				Containment: ContainmentAdvisory,
			},
			label: `write("src", readwrite)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.a)
			if err == nil {
				t.Fatalf("Compile() = %s, nil; want an uncovered-clause error", p)
			}
			if !errors.Is(err, ErrClauseUncovered) {
				t.Fatalf("Compile() error = %v; want ErrClauseUncovered", err)
			}
			if errors.Is(err, ErrInvalidDeclaration) {
				t.Fatalf("Compile() error = %v; must not read as an invalid declaration", err)
			}
			if !strings.Contains(err.Error(), tc.label) {
				t.Fatalf("Compile() error = %q; want it to name clause %s", err, tc.label)
			}
			if !strings.Contains(p.String(), "plan(fallback=refuse") {
				t.Fatalf("refused Compile returned %s; want the zero plan", p)
			}
		})
	}
}

func TestFallbackEnforceAcceptsTheUnenforcedLane(t *testing.T) {
	t.Run("the assignment is emitted and records the degradation", func(t *testing.T) {
		a := Authority{
			Writes:      []WriteClause{{Root: "src", Access: AccessReadWrite}},
			Network:     NetworkDenyAll,
			Containment: ContainmentAdvisory,
			Fallback:    FallbackEnforce,
		}
		p, err := Compile(a)
		if err != nil {
			t.Fatalf("Compile() error = %v; want nil", err)
		}
		if got := p.Fallback(); got != FallbackEnforce {
			t.Fatalf("Fallback() = %s; want enforce", got)
		}
		want := []string{
			`jail/pre_action write("src", readwrite) enforce`,
			`jail/pre_action network(deny_all) enforce`,
		}
		got := renderAssignments(p)
		if len(got) != len(want) {
			t.Fatalf("assignments = %v; want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("assignment %d = %q; want %q", i, got[i], want[i])
			}
		}
		if !strings.Contains(p.String(), "fallback=enforce") {
			t.Fatalf("String() = %s; want it to record the enforced fallback", p)
		}
	})

	t.Run("refuse is the default and the same declaration then fails", func(t *testing.T) {
		a := Authority{
			Writes:      []WriteClause{{Root: "src", Access: AccessReadWrite}},
			Containment: ContainmentAdvisory,
		}
		if _, err := Compile(a); !errors.Is(err, ErrClauseUncovered) {
			t.Fatalf("Compile() error = %v; want ErrClauseUncovered", err)
		}
		a.Fallback = FallbackEnforce
		if _, err := Compile(a); err != nil {
			t.Fatalf("Compile() error = %v; want nil once the fallback enforces", err)
		}
	})
}

func TestCompileRefusesAnInvalidDeclaration(t *testing.T) {
	cases := []struct {
		name string
		a    Authority
		want string
	}{
		{
			name: "access unset",
			a:    Authority{Writes: []WriteClause{{Root: "src"}}, Containment: ContainmentJail},
			want: "unknown",
		},
		{
			name: "access unknown",
			a:    Authority{Writes: []WriteClause{{Root: "src", Access: Access("rw")}}, Containment: ContainmentJail},
			want: `access "rw"`,
		},
		{
			name: "absolute root",
			a:    Authority{Writes: []WriteClause{{Root: "/etc", Access: AccessReadWrite}}, Containment: ContainmentJail},
			want: "world-relative",
		},
		{
			name: "root naming the world root",
			a:    Authority{Writes: []WriteClause{{Root: ".", Access: AccessReadWrite}}, Containment: ContainmentJail},
			want: "names the world root",
		},
		{
			name: "escaping root",
			a:    Authority{Writes: []WriteClause{{Root: "../etc", Access: AccessReadWrite}}, Containment: ContainmentJail},
			want: "escapes the dispatch world",
		},
		{
			name: "backend-separated root",
			a:    Authority{Writes: []WriteClause{{Root: `src\internal`, Access: AccessReadWrite}}, Containment: ContainmentJail},
			want: "slash-separated",
		},
		{
			name: "network posture unknown",
			a:    Authority{Network: NetworkPosture("open"), Containment: ContainmentJail},
			want: `network posture "open"`,
		},
		{
			name: "delta policy unknown",
			a:    Authority{Delta: DeltaPolicy("auto")},
			want: `delta policy "auto"`,
		},
		{
			name: "containment unknown",
			a:    Authority{Containment: Containment("paddock")},
			want: `containment "paddock"`,
		},
		{
			name: "fallback unknown",
			a:    Authority{Fallback: Fallback("allow"), Containment: ContainmentJail},
			want: `fallback "allow"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Compile(tc.a)
			if err == nil {
				t.Fatalf("Compile() = %s, nil; want an invalid-declaration error", p)
			}
			if !errors.Is(err, ErrInvalidDeclaration) {
				t.Fatalf("Compile() error = %v; want ErrInvalidDeclaration", err)
			}
			if errors.Is(err, ErrClauseUncovered) {
				t.Fatalf("Compile() error = %v; must not read as a coverage gap", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Compile() error = %q; want it to mention %q", err, tc.want)
			}
			if p.Digest() != (PermissionPlan{}).Digest() {
				t.Fatalf("refused Compile returned a plan with digest %s; want the zero plan", p.Digest())
			}
		})
	}
}

func TestCoverageRejectsIncompleteAssignments(t *testing.T) {
	c, err := lowerWrite(WriteClause{Root: "src", Access: AccessReadWrite})
	if err != nil {
		t.Fatalf("lowerWrite() error = %v; want nil", err)
	}
	narrow, err := lowerWrite(WriteClause{Root: "src/internal", Access: AccessReadWrite})
	if err != nil {
		t.Fatalf("lowerWrite() error = %v; want nil", err)
	}

	cases := []struct {
		name string
		a    []Assignment
		want bool // covered
	}{
		{
			name: "complete assignment covers",
			a:    []Assignment{{Label: c.label, Clause: c.match, Monitor: MonitorJail, Timing: TimingPreAction}},
			want: true,
		},
		{
			name: "no assignment at all",
			a:    nil,
			want: false,
		},
		{
			name: "unknown monitor enforces nothing",
			a:    []Assignment{{Label: c.label, Clause: c.match}},
			want: false,
		},
		{
			name: "a known monitor at the wrong timing is not complete",
			a:    []Assignment{{Label: c.label, Clause: c.match, Monitor: MonitorJail, Timing: TimingCommit}},
			want: false,
		},
		{
			name: "the settlement monitor acts at commit, not before the action",
			a:    []Assignment{{Label: c.label, Clause: c.match, Monitor: MonitorSettlement, Timing: TimingPreAction}},
			want: false,
		},
		{
			name: "a narrower assignment does not cover a wider clause",
			a:    []Assignment{{Label: narrow.label, Clause: narrow.match, Monitor: MonitorJail, Timing: TimingPreAction}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PermissionPlan{fallback: FallbackRefuse}
			p.declare(clause{label: c.label, match: c.match})
			for _, a := range tc.a {
				p.assign(a)
			}
			err := p.Coverage()
			if tc.want && err != nil {
				t.Fatalf("Coverage() = %v; want nil", err)
			}
			if !tc.want {
				if !errors.Is(err, ErrClauseUncovered) {
					t.Fatalf("Coverage() = %v; want ErrClauseUncovered", err)
				}
				if !strings.Contains(err.Error(), c.label) {
					t.Fatalf("Coverage() = %q; want it to name clause %s", err, c.label)
				}
			}
		})
	}
}

func TestDigest(t *testing.T) {
	mustCompile := func(t *testing.T, a Authority) PermissionPlan {
		t.Helper()
		p, err := Compile(a)
		if err != nil {
			t.Fatalf("Compile() error = %v; want nil", err)
		}
		return p
	}

	base := jailedAuthority()
	reordered := jailedAuthority()
	reordered.Writes = []WriteClause{
		{Root: "docs", Access: AccessReadOnly},
		{Root: "src", Access: AccessReadWrite},
	}
	baseWithDocs := jailedAuthority()
	baseWithDocs.Writes = append([]WriteClause{{Root: "docs", Access: AccessReadOnly}}, base.Writes...)

	flipped := jailedAuthority()
	flipped.Writes[0].Access = AccessReadOnly

	applied := jailedAuthority()
	applied.Delta = DeltaApply

	// A root spelled with a trailing separator names the same directory, so it
	// must compile to the same plan.
	spelled := jailedAuthority()
	spelled.Writes[0].Root = "src/./"

	advisory := jailedAuthority()
	advisory.Containment = ContainmentAdvisory
	advisory.Fallback = FallbackEnforce

	advisoryRefused := jailedAuthority()
	advisoryRefused.Containment = ContainmentAdvisory

	cases := []struct {
		name  string
		a     Authority
		same  *Authority
		equal bool
	}{
		{
			name:  "the same declaration is the same plan",
			a:     base,
			same:  &base,
			equal: true,
		},
		{
			name:  "clause order is not part of a plan's identity",
			a:     baseWithDocs,
			same:  &reordered,
			equal: true,
		},
		{
			name:  "a different access is a different plan",
			a:     base,
			same:  &flipped,
			equal: false,
		},
		{
			name:  "a root's spelling is normalized",
			a:     base,
			same:  &spelled,
			equal: true,
		},
		{
			name:  "a different delta policy is a different plan",
			a:     base,
			same:  &applied,
			equal: false,
		},
		{
			name:  "an accepted degraded lane is a different plan",
			a:     base,
			same:  &advisory,
			equal: false,
		},
		{
			name:  "a refused declaration has no plan to differ from",
			a:     base,
			same:  &advisoryRefused,
			equal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustCompile(t, tc.a).Digest()
			if len(got) != 64 || strings.Trim(got, "0123456789abcdef") != "" {
				t.Fatalf("Digest() = %q; want 64 lowercase hex characters", got)
			}
			if again := mustCompile(t, tc.a).Digest(); again != got {
				t.Fatalf("Digest() is not stable: %s then %s", got, again)
			}
			if tc.same == nil {
				return
			}
			other, err := Compile(*tc.same)
			if err != nil {
				// The refused declaration produces no plan; only the zero plan
				// is left to compare against, which is the point of the case.
				other = PermissionPlan{}
			}
			if gotEqual := got == other.Digest(); gotEqual != tc.equal {
				t.Fatalf("digests equal = %v; want %v (%s vs %s)", gotEqual, tc.equal, got, other.Digest())
			}
		})
	}

	t.Run("the zero plan has a stable digest", func(t *testing.T) {
		if a, b := (PermissionPlan{}).Digest(), (PermissionPlan{}).Digest(); a != b {
			t.Fatalf("zero-plan digests differ: %s vs %s", a, b)
		}
	})
}

func TestStringRoundTrip(t *testing.T) {
	p, err := Compile(jailedAuthority())
	if err != nil {
		t.Fatalf("Compile() error = %v; want nil", err)
	}
	s := p.String()

	wants := []string{
		"plan(fallback=refuse",
		`write("src", readwrite)`,
		"network(deny_all)",
		"delta(review)",
		"monitor=jail",
		"monitor=settlement",
		"timing=pre_action",
		"timing=commit",
		"fallback=refuse",
	}
	for _, want := range wants {
		if !strings.Contains(s, want) {
			t.Fatalf("String() = %s; want it to mention %q", s, want)
		}
	}
	if strings.Contains(s, "unknown") {
		t.Fatalf("String() = %s; a compiled plan names no unknown monitor or timing", s)
	}
	if again := p.String(); again != s {
		t.Fatalf("String() is not stable: %q then %q", s, again)
	}

	other, err := Compile(Authority{Delta: DeltaApply})
	if err != nil {
		t.Fatalf("Compile() error = %v; want nil", err)
	}
	if other.String() == s {
		t.Fatalf("different declarations render alike: %s", s)
	}
	if zero := (PermissionPlan{}).String(); !strings.Contains(zero, "clauses=[] assignments=[]") {
		t.Fatalf("zero plan String() = %s; want empty clause and assignment lists", zero)
	}
}

func TestWriteClauseSurface(t *testing.T) {
	p, err := Compile(Authority{
		Writes:      []WriteClause{{Root: "src", Access: AccessReadWrite}},
		Containment: ContainmentJail,
	})
	if err != nil {
		t.Fatalf("Compile() error = %v; want nil", err)
	}
	surface := p.Assignments()[0].Clause

	cases := []struct {
		name string
		e    effectpolicy.Effect
		want bool
	}{
		{
			name: "a write under the root",
			e:    effectpolicy.Effect{Kind: WriteKind, Fields: map[string]any{PathField: "src/main.go"}},
			want: true,
		},
		{
			name: "a delete under the root is the same authority",
			e:    effectpolicy.Effect{Kind: DeleteKind, Fields: map[string]any{PathField: "src/old.go"}},
			want: true,
		},
		{
			name: "a write outside the root",
			e:    effectpolicy.Effect{Kind: WriteKind, Fields: map[string]any{PathField: "docs/readme.md"}},
			want: false,
		},
		{
			name: "a sibling whose name only shares the prefix",
			e:    effectpolicy.Effect{Kind: WriteKind, Fields: map[string]any{PathField: "srcx/main.go"}},
			want: false,
		},
		{
			name: "a non-mutating effect",
			e:    effectpolicy.Effect{Kind: "bash", Fields: map[string]any{PathField: "src/main.go"}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := surface.Matches(tc.e); got != tc.want {
				t.Fatalf("%s.Matches(%s) = %v; want %v", surface, tc.e, got, tc.want)
			}
		})
	}

	t.Run("an empty root governs the whole world", func(t *testing.T) {
		whole, err := Compile(Authority{
			Writes:      []WriteClause{{Access: AccessReadWrite}},
			Containment: ContainmentJail,
		})
		if err != nil {
			t.Fatalf("Compile() error = %v; want nil", err)
		}
		surface := whole.Assignments()[0].Clause
		for _, p := range []string{"anything.txt", "src/main.go", PathField} {
			e := effectpolicy.Effect{Kind: WriteKind, Fields: map[string]any{PathField: p}}
			if !surface.Matches(e) {
				t.Fatalf("%s.Matches(%s) = false; want true", surface, e)
			}
		}
	})
}

func TestEnumRenderings(t *testing.T) {
	t.Run("monitor", func(t *testing.T) {
		cases := []struct {
			m        Monitor
			want     string
			wantVald bool
		}{
			{MonitorUnknown, "unknown", false},
			{MonitorJail, "jail", true},
			{MonitorSettlement, "settlement", true},
			{Monitor("container"), "container", false},
		}
		for _, tc := range cases {
			t.Run(tc.want, func(t *testing.T) {
				if got := tc.m.String(); got != tc.want {
					t.Fatalf("%q.String() = %q; want %q", string(tc.m), got, tc.want)
				}
				if got := tc.m.Valid(); got != tc.wantVald {
					t.Fatalf("%q.Valid() = %v; want %v", string(tc.m), got, tc.wantVald)
				}
			})
		}
	})

	t.Run("timing", func(t *testing.T) {
		cases := []struct {
			when     Timing
			want     string
			wantVald bool
		}{
			{TimingUnknown, "unknown", false},
			{TimingPreAction, "pre_action", true},
			{TimingCommit, "commit", true},
			{Timing("during"), "during", false},
		}
		for _, tc := range cases {
			t.Run(tc.want, func(t *testing.T) {
				if got := tc.when.String(); got != tc.want {
					t.Fatalf("%q.String() = %q; want %q", string(tc.when), got, tc.want)
				}
				if got := tc.when.Valid(); got != tc.wantVald {
					t.Fatalf("%q.Valid() = %v; want %v", string(tc.when), got, tc.wantVald)
				}
			})
		}
	})

	t.Run("fallback", func(t *testing.T) {
		cases := []struct {
			f    Fallback
			want string
		}{
			{Fallback(""), "refuse"},
			{FallbackRefuse, "refuse"},
			{FallbackEnforce, "enforce"},
			{Fallback("allow"), `fallback("allow")`},
		}
		for _, tc := range cases {
			t.Run(tc.want, func(t *testing.T) {
				if got := tc.f.String(); got != tc.want {
					t.Fatalf("%q.String() = %q; want %q", string(tc.f), got, tc.want)
				}
			})
		}
	})

	t.Run("declaration vocabulary", func(t *testing.T) {
		cases := []struct {
			name string
			got  string
			want string
		}{
			{"unset access", AccessUnset.String(), "unknown"},
			{"readwrite", AccessReadWrite.String(), "readwrite"},
			{"unset network", NetworkUnset.String(), "unset"},
			{"broker", NetworkBroker.String(), "broker"},
			{"unset delta", DeltaUnset.String(), "unset"},
			{"review", DeltaReview.String(), "review"},
			{"unset containment", ContainmentUnset.String(), "unset"},
			{"jail", ContainmentJail.String(), "jail"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if tc.got != tc.want {
					t.Fatalf("%s = %q; want %q", tc.name, tc.got, tc.want)
				}
			})
		}
	})
}

func TestAssignmentsIsACopy(t *testing.T) {
	p, err := Compile(jailedAuthority())
	if err != nil {
		t.Fatalf("Compile() error = %v; want nil", err)
	}
	got := p.Assignments()
	if len(got) == 0 {
		t.Fatal("Assignments() is empty; the reference declaration has clauses")
	}
	got[0].Label = "tampered"
	got[0].Monitor = MonitorUnknown
	if again := p.Assignments(); again[0].Label == "tampered" || again[0].Monitor == MonitorUnknown {
		t.Fatalf("Assignments() shares its slice with the plan: %v", again)
	}
	if err := p.Coverage(); err != nil {
		t.Fatalf("Coverage() = %v; want nil after a caller mutated its copy", err)
	}
}

func TestZeroPlanCoversNothingAndRefusesNothing(t *testing.T) {
	zero := PermissionPlan{}
	if err := zero.Coverage(); err != nil {
		t.Fatalf("Coverage() = %v; want nil: a plan with no declared clause has no gap", err)
	}
	if got := zero.Assignments(); len(got) != 0 {
		t.Fatalf("Assignments() = %v; want none", got)
	}
	if got := zero.Fallback(); got != FallbackRefuse {
		t.Fatalf("Fallback() = %s; want refuse", got)
	}
}

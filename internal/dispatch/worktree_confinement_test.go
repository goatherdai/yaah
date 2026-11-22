package dispatch

// worktree_confinement_test.go pins the two halves of the worktree lane's
// containment citation: the digest names the jail the run actually established
// (not the one its config asked for), and every default settlement verb records
// the citation it consumed the sealed output under.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/permissionplan"
)

// The confinement reports a runner can hand the dispatch lane. They are written
// as bare records so a test states the posture, not the runner's plumbing.

// jailedConfinement is a run the runner actually jailed.
func jailedConfinement() *jobs.SubAgentConfinement {
	return &jobs.SubAgentConfinement{
		Requested: "auto",
		Resolved:  "paddock",
		Backend:   "seatbelt",
		Tier:      "native-syscall-deny",
		Enforced:  true,
	}
}

// advisoryConfinement is a run that resolved to host paths.
func advisoryConfinement() *jobs.SubAgentConfinement {
	return &jobs.SubAgentConfinement{Requested: "off", Resolved: "advisory"}
}

// refusedConfinement is a run that demanded a jail the host could not
// establish, so it refused rather than running unconfined.
func refusedConfinement() *jobs.SubAgentConfinement {
	return &jobs.SubAgentConfinement{Requested: "paddock", Resolved: "paddock"}
}

// degradedConfinement is a run that resolved to a jail, then lost it at exec
// time: the body ran unconfined, so the citation must say advisory.
func degradedConfinement() *jobs.SubAgentConfinement {
	return &jobs.SubAgentConfinement{
		Requested: "auto",
		Resolved:  "paddock",
		Backend:   "landlock",
		Enforced:  false,
		Degraded:  "landlock: ruleset not accepted",
	}
}

// allConfinements is every posture a runner can report, plus the nil that means
// "nothing witnessed this run".
func allConfinements() []*jobs.SubAgentConfinement {
	return []*jobs.SubAgentConfinement{
		nil,
		jailedConfinement(),
		advisoryConfinement(),
		refusedConfinement(),
		degradedConfinement(),
	}
}

// TestWorktreeAuthorityForDispatch_ReadsTheEstablishedPosture pins the mapping
// from the run's confinement to the declaration it cites.
func TestWorktreeAuthorityForDispatch_ReadsTheEstablishedPosture(t *testing.T) {
	rt := RetainedRuntime{Containment: "auto", Grants: []config.GrantConfig{{Path: "src", Access: "readwrite"}}}

	cases := []struct {
		name     string
		report   *jobs.SubAgentConfinement
		want     permissionplan.Containment
		fallback permissionplan.Fallback
	}{
		{name: "jailed", report: jailedConfinement(), want: permissionplan.ContainmentJail, fallback: permissionplan.FallbackRefuse},
		{name: "refused", report: refusedConfinement(), want: permissionplan.ContainmentJail, fallback: permissionplan.FallbackRefuse},
		{name: "advisory", report: advisoryConfinement(), want: permissionplan.ContainmentAdvisory, fallback: permissionplan.FallbackEnforce},
		{name: "degraded", report: degradedConfinement(), want: permissionplan.ContainmentAdvisory, fallback: permissionplan.FallbackEnforce},
		// Nothing witnessed the run: the declared posture, fail-closed.
		{name: "unreported", report: nil, want: permissionplan.ContainmentJail, fallback: permissionplan.FallbackRefuse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := worktreeAuthorityForDispatch(rt, "developer", tc.report)
			if err != nil {
				t.Fatalf("worktreeAuthorityForDispatch: %v", err)
			}
			if got.Containment != tc.want || got.Fallback != tc.fallback {
				t.Fatalf("containment/fallback = %q/%q, want %q/%q",
					got.Containment, got.Fallback, tc.want, tc.fallback)
			}
		})
	}

	// The deliberate refusal is not a downgrade: a jail the host could not
	// establish cites the same declaration a jailed run cites.
	refused, err := resolveWorktreePlanDigest(rt, "developer", refusedConfinement())
	if err != nil {
		t.Fatalf("resolveWorktreePlanDigest(refused): %v", err)
	}
	jailed, err := resolveWorktreePlanDigest(rt, "developer", jailedConfinement())
	if err != nil {
		t.Fatalf("resolveWorktreePlanDigest(jailed): %v", err)
	}
	if refused != jailed {
		t.Fatalf("a refused jail cites %q, a jailed run %q: both are jail/refuse", refused, jailed)
	}
}

// TestWorktreePlanDigest_CitesTheJailTheRunEstablished pins the reported
// difference between a jailed and an advisory worktree run, and pins that the
// jailed digest is the container lane's own jail declaration rather than a
// worktree-specific restatement of it.
func TestWorktreePlanDigest_CitesTheJailTheRunEstablished(t *testing.T) {
	rt := RetainedRuntime{
		Containment:  "auto",
		Network:      "allow_all",
		Grants:       []config.GrantConfig{{Path: "src", Access: "readwrite"}},
		NetworkHosts: nil,
	}

	jailed, err := resolveWorktreePlanDigest(rt, "developer", jailedConfinement())
	if err != nil {
		t.Fatalf("resolveWorktreePlanDigest(jailed): %v", err)
	}
	advisory, err := resolveWorktreePlanDigest(rt, "developer", advisoryConfinement())
	if err != nil {
		t.Fatalf("resolveWorktreePlanDigest(advisory): %v", err)
	}
	if jailed == advisory {
		t.Fatalf("a jailed run and an advisory run cite one digest (%s): the plan describes no confinement", jailed)
	}

	// The jail posture is the declaration the container lane compiles for this
	// config — containment auto is a jail there — so the two lanes' digests agree
	// exactly because both worlds were jailed.
	container, err := resolveDispatchAuthority(rt, "developer")
	if err != nil {
		t.Fatalf("resolveDispatchAuthority: %v", err)
	}
	if jailed != container.planIdentity() {
		t.Errorf("jailed worktree digest = %q, want the container lane's jail declaration %q", jailed, container.planIdentity())
	}

	// The advisory posture is the advisory/enforce declaration, not a re-hash.
	advisoryPlan, err := compileAuthorityPlan(permissionplan.Authority{
		Writes:      []permissionplan.WriteClause{{Root: "src", Access: permissionplan.AccessReadWrite}},
		Network:     permissionplan.NetworkAllowAll,
		Delta:       permissionplan.DeltaApply,
		Containment: permissionplan.ContainmentAdvisory,
		Fallback:    permissionplan.FallbackEnforce,
	})
	if err != nil {
		t.Fatalf("compileAuthorityPlan(advisory): %v", err)
	}
	if advisory != advisoryPlan.Digest() {
		t.Errorf("advisory worktree digest = %q, want %q", advisory, advisoryPlan.Digest())
	}
}

// TestRetained_WorktreeProposalCitesEstablishedConfinement pins the citation end
// to end: the sealed metadata carries the digest of the jail the runner reported
// for that run, so the proposal (and a rehydration of it) names the enforcement
// the run had.
func TestRetained_WorktreeProposalCitesEstablishedConfinement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report *jobs.SubAgentConfinement
		want   *jobs.SubAgentConfinement
	}{
		{name: "jailed", report: jailedConfinement()},
		{name: "advisory", report: advisoryConfinement()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, repo, rootID := newSessionFixtures(t)
			report := *tc.report
			runner := func(ctx context.Context, _ string, params SubAgentParams) (string, error) {
				writeTestFile(t, params.Workdir, "work.txt", "work\n")
				// The runner's own report: the jail this body ran under.
				jobs.CaptureSubAgentConfinement(ctx, report)
				return "done", nil
			}
			rt := RetainedRuntime{
				Runner:       runner,
				RepoPath:     repo,
				ScopeManager: fx.mgr,
				RootScopeID:  rootID,
				WorktreeRoot: t.TempDir(),
				SessionID:    "sess-confinement-citation",
				Proposals:    NewProposalRegistry(),
				Network:      "allow_all",
				Containment:  "auto",
			}
			want, err := resolveWorktreePlanDigest(rt, "developer", tc.report)
			if err != nil {
				t.Fatalf("resolveWorktreePlanDigest: %v", err)
			}
			other := monitoredRefusal(t, rt, report)

			res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer"}, 10*time.Second, false)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			env := decodeProposalEnvelope(t, res)
			if env.Status != "proposal" {
				t.Fatalf("dispatch status = %q (%s)", env.Status, env.Error)
			}
			live, err := rt.Proposals.get(env.ProposalID)
			if err != nil {
				t.Fatalf("live proposal: %v", err)
			}
			if live.authorityPlanDigest != want {
				t.Errorf("live proposal cites %q, want the run's established posture %q", live.authorityPlanDigest, want)
			}
			if live.authorityPlanDigest == other {
				t.Errorf("proposal cites the other posture's digest (%q); the run's confinement did not reach the seal", other)
			}
			meta, _, merr := proposalMetaFromOutput(live.output)
			if merr != nil {
				t.Fatalf("sealed metadata: %v", merr)
			}
			if meta.AuthorityPlanDigest != want {
				t.Errorf("sealed metadata cites %q, want %q", meta.AuthorityPlanDigest, want)
			}
		})
	}
}

// monitoredRefusal returns the digest the opposite posture would cite, so a test
// can prove the seal did not accidentally cite it.
func monitoredRefusal(t *testing.T, rt RetainedRuntime, report jobs.SubAgentConfinement) string {
	t.Helper()
	flipped := report
	if report.Jailed() {
		flipped = *advisoryConfinement()
	} else {
		flipped = *jailedConfinement()
	}
	digest, err := resolveWorktreePlanDigest(rt, "developer", &flipped)
	if err != nil {
		t.Fatalf("resolveWorktreePlanDigest(flipped): %v", err)
	}
	return digest
}

// TestWorktreeSettleDefault_RecordsPlanDigest pins the settlement fact for the
// default worktree verbs. Each of select, path-disjoint apply, release, and
// discard consumes the sealed output, and each must record which declaration
// governed the world that produced it — including a proposal rehydrated from its
// sealed metadata, which is the case a restart produces.
func TestWorktreeSettleDefault_RecordsPlanDigest(t *testing.T) {
	for _, action := range []string{"select", "apply", "release", "discard"} {
		t.Run(action, func(t *testing.T) {
			fx, repo, rootID := newSessionFixtures(t)
			runner := &sessionRunner{}
			runner.sideEffect = func(call int, _ string) {
				writeTestFile(t, runner.workdirFor(t, call), "out.txt", "work\n")
			}
			rt := RetainedRuntime{
				Runner:       runner.run(),
				RepoPath:     repo,
				ScopeManager: fx.mgr,
				RootScopeID:  rootID,
				WorktreeRoot: t.TempDir(),
				SessionID:    "sess-settle-fact",
				Proposals:    NewProposalRegistry(),
			}
			res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "worker"}, 10*time.Second, false)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			env := decodeProposalEnvelope(t, res)
			if env.Status != "proposal" {
				t.Fatalf("dispatch status = %q (%s)", env.Status, env.Error)
			}
			live, err := rt.Proposals.get(env.ProposalID)
			if err != nil {
				t.Fatalf("live proposal: %v", err)
			}
			if live.authorityPlanDigest == "" {
				t.Fatal("the sealed proposal cites no declaration; the test cannot prove it is carried")
			}
			if _, err := rt.Proposals.settleRetainedProposal(context.Background(), env.ProposalID, action); err != nil {
				t.Fatalf("settle %s: %v", action, err)
			}
			assertSettleFactDigest(t, fx, rootID, action, live.authorityPlanDigest)
		})
	}

	t.Run("rehydrated", func(t *testing.T) {
		fx, repo, rootID := newSessionFixtures(t)
		runner := &sessionRunner{}
		runner.sideEffect = func(call int, _ string) {
			writeTestFile(t, runner.workdirFor(t, call), "out.txt", "work\n")
		}
		rt := RetainedRuntime{
			Runner:       runner.run(),
			RepoPath:     repo,
			ScopeManager: fx.mgr,
			RootScopeID:  rootID,
			WorktreeRoot: t.TempDir(),
			SessionID:    "sess-settle-rehydrated",
			Proposals:    NewProposalRegistry(),
		}
		res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "worker"}, 10*time.Second, false)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		env := decodeProposalEnvelope(t, res)

		// A restart: the sealed metadata is the durable record, and rehydration
		// must carry the citation into the recovered proposal.
		reg, warnings := loadWorktreeProposalsForTest(fx, repo, rootID, rt.SessionID)
		p, err := reg.get(env.ProposalID)
		if err != nil {
			t.Fatalf("rehydrated proposal missing: %v (warnings: %v)", err, warnings)
		}
		if p.authorityPlanDigest == "" {
			t.Fatal("rehydrated proposal cites no declaration: the seal's digest did not survive rehydration")
		}
		if _, serr := reg.settleRetainedProposal(context.Background(), env.ProposalID, "discard"); serr != nil {
			t.Fatalf("settle rehydrated discard: %v", serr)
		}
		assertSettleFactDigest(t, fx, rootID, "discard", p.authorityPlanDigest)
	})
}

// loadWorktreeProposalsForTest runs the worktree half of LoadProposalRegistry
// against the live scope tree, so a test exercises the same rehydration path a
// restart does without standing up recovery.
func loadWorktreeProposalsForTest(fx *sessionFixtures, repo, rootID, sessionID string) (*ProposalRegistry, []string) {
	reg := NewProposalRegistry()
	return reg, loadWorktreeProposals(reg, fx.mgr, repo, rootID, sessionID)
}

// assertSettleFactDigest fails unless the parent scope holds a settled fact for
// action carrying the want digest.
func assertSettleFactDigest(t *testing.T, fx *sessionFixtures, rootID, action, want string) {
	t.Helper()
	parent, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatal("root scope missing")
	}
	facts := findRetainedFacts(t, fx.store, parent.OwnerID(), SchemaProposalSettled)
	matched := false
	for _, fact := range facts {
		var payload struct {
			Action              string `json:"action"`
			World               string `json:"world"`
			AuthorityPlanDigest string `json:"authority_plan_digest"`
		}
		data, _ := json.Marshal(fact.Body.Payload)
		if json.Unmarshal(data, &payload) != nil || payload.Action != action {
			continue
		}
		matched = true
		if payload.World != string(worldWorktree) {
			t.Errorf("%s settle fact world = %q, want %q", action, payload.World, worldWorktree)
		}
		if payload.AuthorityPlanDigest != want {
			t.Errorf("%s settle fact authority_plan_digest = %q, want the seal's %q",
				action, payload.AuthorityPlanDigest, want)
		}
	}
	if !matched {
		t.Fatalf("no settle fact recorded for %s: %+v", action, facts)
	}
}

// worldWorktree is the envelope world name the settle fact records for this lane.
const worldWorktree = "worktree"

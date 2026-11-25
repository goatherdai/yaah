package tools

// delegate_supervise_test.go pins the tool half of revert by construction
// (design §4.2, yaah-ugtl): under supervision a dispatch that lane routing
// would send to the shared workspace is upgraded to the report lane, the
// per-call `supervise` override wins over the session posture, and the
// runner learns the answer from the dispatch context.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// decodeStatusLane parses the completion envelope's dispatch identity.
func decodeStatusLane(t *testing.T, result string) (status, lane, world string) {
	t.Helper()
	var env struct {
		Status string `json:"status"`
		Lane   string `json:"lane"`
		World  string `json:"world"`
	}
	if err := json.Unmarshal([]byte(result), &env); err != nil {
		t.Fatalf("completion envelope is not JSON: %v\n%s", err, result)
	}
	return env.Status, env.Lane, env.World
}

// TestExecute_SupervisedUpgradesSharedLaneToReport pins the lane upgrade: a
// role whose lane routing says "shared" AND whose tools can change the tree
// gets the isolated report lane when the dispatch is supervised — an isolated
// worktree, a plain report, and the worktree torn down — because that is what
// makes the work revertable by construction. The unsupervised twin keeps the
// shared lane (pinned by TestExecute_LaneForRoleRouting, unchanged).
func TestExecute_SupervisedUpgradesSharedLaneToReport(t *testing.T) {
	runner := &sessionRunner{responses: []runnerResponse{{result: "shared report"}}}
	rt := newRetainedTestEnv(t, runner)
	tt := &DelegateTool{
		RoleNames:            []string{"counter"},
		Runner:               runner.run(),
		Retained:             &rt,
		LaneForRole:          func(string) DelegateLane { return LaneShared },
		RoleMutatesWorkspace: func(string) bool { return true },
		Supervised:           true,
	}

	res, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	status, lane, _ := decodeStatusLane(t, res)
	if status != "report" || lane != string(LaneReport) {
		t.Errorf("supervised shared-lane dispatch = status %q lane %q, want the report lane", status, lane)
	}
	if got := rt.Proposals.Summaries(); got != "No open proposals." {
		t.Errorf("the upgrade must not register a proposal, got %q", got)
	}
	wd := runner.workdirFor(t, 0)
	if wd == "" || wd == rt.RepoPath {
		t.Fatalf("the upgrade must isolate the dispatch in its own world, got workdir %q", wd)
	}
	if _, serr := os.Stat(wd); !os.IsNotExist(serr) {
		t.Errorf("the report world must be torn down after the dispatch (stat err=%v)", serr)
	}
}

// TestExecute_SupervisedReadOnlyRoleKeepsSharedLane pins the other half of the
// upgrade's condition (yaah-ugtl review: the upgrade bought nothing for a
// read-only role). Lane routing only hands back the shared lane for a role with
// no workspace-mutating tool, so upgrading it forked a world at HEAD, hid the
// session's live tree from a dispatch that only reads it, and tore the world
// down again — a revert has nothing to undo when nothing can be written. A
// read-only supervised dispatch keeps the shared lane; a mutating one still
// upgrades (the test above).
func TestExecute_SupervisedReadOnlyRoleKeepsSharedLane(t *testing.T) {
	runner := &sessionRunner{responses: []runnerResponse{{result: "shared report"}}}
	rt := newRetainedTestEnv(t, runner)
	tt := &DelegateTool{
		RoleNames:            []string{"counter"},
		Runner:               runner.run(),
		Retained:             &rt,
		LaneForRole:          func(string) DelegateLane { return LaneShared },
		RoleMutatesWorkspace: func(string) bool { return false },
		Supervised:           true,
	}

	res, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	status, lane, world := decodeStatusLane(t, res)
	if status != "shared" || lane != string(LaneShared) {
		t.Errorf("read-only supervised dispatch = status %q lane %q, want the shared lane kept", status, lane)
	}
	if world != "shared" {
		t.Errorf("world = %q, want shared — no world may be provisioned for a dispatch that cannot write", world)
	}
	if wd := runner.workdirFor(t, 0); wd != "" {
		t.Errorf("a read-only dispatch must not be provisioned a world, got %q", wd)
	}
}

// TestExecute_SupervisedSharedLaneWithoutIsolation pins the other branch of
// supervisedLane (review finding L4): a supervised dispatch whose host has no
// isolated lane available (no retained runtime — a container session, §9)
// STAYS on the shared lane. The lane must not pretend to isolate, and the
// decision moves to the runner's start gate: with require_reversible off the
// dispatch runs, reporting Reversible false honestly.
func TestExecute_SupervisedSharedLaneWithoutIsolation(t *testing.T) {
	runner := &sessionRunner{responses: []runnerResponse{{result: "shared result"}}}
	tt := &DelegateTool{
		RoleNames:  []string{"counter"},
		Runner:     runner.run(),
		Retained:   nil, // no retained runtime: isolated worlds unavailable
		Supervised: true,
	}

	res, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter"}`)
	if err != nil {
		t.Fatalf("Execute: %v, want the dispatch to run on the shared lane", err)
	}
	status, lane, _ := decodeStatusLane(t, res)
	if status == "report" || lane == string(LaneReport) {
		t.Errorf("supervised dispatch without isolation = status %q lane %q, want the shared lane kept", status, lane)
	}
}

// TestExecute_SuperviseFalseKeepsRoutedLane pins the opt-out: `supervise:
// false` leaves the lane the role's routing chose, so an opted-out dispatch
// on a supervised session runs exactly where it always ran.
func TestExecute_SuperviseFalseKeepsRoutedLane(t *testing.T) {
	runner := &sessionRunner{responses: []runnerResponse{{result: "shared report"}}}
	rt := newRetainedTestEnv(t, runner)
	tt := &DelegateTool{
		RoleNames:   []string{"counter"},
		Runner:      runner.run(),
		Retained:    &rt,
		LaneForRole: func(string) DelegateLane { return LaneShared },
		Supervised:  true,
	}

	res, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter","supervise":false}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	status, lane, _ := decodeStatusLane(t, res)
	if status != "shared" || lane != string(LaneShared) {
		t.Errorf("supervise:false = status %q lane %q, want the routed shared lane", status, lane)
	}
	if wd := runner.workdirFor(t, 0); wd != "" {
		t.Errorf("an opted-out dispatch must not be provisioned a world, got %q", wd)
	}
}

// TestExecute_SuperviseTrueNeedsSessionSupervision pins the resolved
// ambiguity (the design names the override but not this combination): a
// single dispatch can choose an isolated, revertable lane but cannot start
// the host-side supervision that would watch it, so `supervise: true` on an
// unsupervised session is refused instead of silently honored.
func TestExecute_SuperviseTrueNeedsSessionSupervision(t *testing.T) {
	runner := &sessionRunner{responses: []runnerResponse{{result: "ok"}}}
	rt := newRetainedTestEnv(t, runner)
	tt := &DelegateTool{
		RoleNames:   []string{"counter"},
		Runner:      runner.run(),
		Retained:    &rt,
		LaneForRole: func(string) DelegateLane { return LaneShared },
	}

	_, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter","supervise":true}`)
	if err == nil {
		t.Fatal("supervise:true on an unsupervised session must be refused")
	}
	if !strings.Contains(err.Error(), "supervise: true") || !strings.Contains(err.Error(), "supervision enabled for the session") {
		t.Errorf("refusal must name the override and the missing session switch, got: %v", err)
	}
	runner.mu.Lock()
	dispatches := len(runner.prompts)
	runner.mu.Unlock()
	if dispatches != 0 {
		t.Errorf("the refused dispatch must not reach the runner, saw %d dispatch(es)", dispatches)
	}
}

// TestExecute_SuperviseMarkerReachesRunner pins the seam the runner's start
// gate reads: a supervised dispatch carries the marker on its context, and an
// opted-out one does not. The runner-side behavior it enables is pinned in
// internal/agent/runner/supervised_start_test.go.
func TestExecute_SuperviseMarkerReachesRunner(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want bool
	}{
		{"session supervision on", `{"description":"d","prompt":"p","role":"counter"}`, true},
		{"opted out", `{"description":"d","prompt":"p","role":"counter","supervise":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			tt := &DelegateTool{
				RoleNames: []string{"counter"},
				Runner: func(ctx context.Context, _ string, _ SubAgentParams) (string, error) {
					seen = SupervisedDispatch(ctx)
					return "ok", nil
				},
				LaneForRole: func(string) DelegateLane { return LaneShared },
				Supervised:  true,
			}
			if _, err := tt.Execute(context.Background(), tc.args); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if seen != tc.want {
				t.Errorf("SupervisedDispatch on the dispatch context = %v, want %v", seen, tc.want)
			}
		})
	}
}

// TestExecute_SuperviseMarkerCarriesTheResolvedBit pins the seam in BOTH
// directions (yaah-ugtl review M1). A nested dispatch reuses its parent's
// context, so a marker written only when supervised let the parent's answer
// stand in for the child's: a depth-2 dispatch that opted out ran supervised
// anyway, and one whose tool resolved to unsupervised could not opt out of a
// gate it never asked for.
func TestExecute_SuperviseMarkerCarriesTheResolvedBit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         string
		toolSuper    bool
		parentMarked bool
		want         bool
	}{
		{
			// The depth-2 leak: the parent's marker is on this context, and
			// this dispatch says no.
			name: "opted out under a supervised parent", args: `{"description":"d","prompt":"p","role":"counter","supervise":false}`,
			toolSuper: true, parentMarked: true, want: false,
		},
		{
			name: "inherits the resolved posture under a supervised parent", args: `{"description":"d","prompt":"p","role":"counter"}`,
			toolSuper: true, parentMarked: true, want: true,
		},
		{
			name: "unsupervised tool clears a parent mark", args: `{"description":"d","prompt":"p","role":"counter"}`,
			toolSuper: false, parentMarked: true, want: false,
		},
		{
			name: "supervised tool marks an unmarked context", args: `{"description":"d","prompt":"p","role":"counter","supervise":true}`,
			toolSuper: true, parentMarked: false, want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen, ran bool
			tt := &DelegateTool{
				RoleNames:   []string{"counter"},
				LaneForRole: func(string) DelegateLane { return LaneShared },
				Supervised:  tc.toolSuper,
				Runner: func(ctx context.Context, _ string, _ SubAgentParams) (string, error) {
					ran = true
					seen = SupervisedDispatch(ctx)
					return "ok", nil
				},
			}
			ctx := context.Background()
			if tc.parentMarked {
				ctx = WithSupervisedDispatch(ctx, true)
			}
			if _, err := tt.Execute(ctx, tc.args); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !ran {
				t.Fatal("the dispatch never reached the runner")
			}
			if seen != tc.want {
				t.Errorf("SupervisedDispatch on the dispatch context = %v, want %v", seen, tc.want)
			}
		})
	}
}

// TestBackgroundContextSeederRoundTripsSupervision pins the background seam
// (yaah-ugtl review M1): a job derives cancel from the session root, so the
// supervision answer is re-attached by the seeder — in both directions, so a
// deferred supervised dispatch is still supervised and a deferred opted-out one
// cannot inherit a mark from the job context it started on.
func TestBackgroundContextSeederRoundTripsSupervision(t *testing.T) {
	for _, supervised := range []bool{true, false} {
		name := "unsupervised"
		if supervised {
			name = "supervised"
		}
		t.Run(name, func(t *testing.T) {
			seeder := backgroundContextSeeder(context.Background(), "widget", ConflictLaneShared, supervised)
			// The job context carries the OPPOSITE answer, so a seeder that
			// wrote only on true would fail the false case ...
			jobCtx := WithSupervisedDispatch(context.Background(), !supervised)
			if got := SupervisedDispatch(seeder(jobCtx)); got != supervised {
				t.Errorf("the seeder left the job marked %v, want the resolved %v", got, supervised)
			}
			// ... and one that never wrote at all would fail both.
			if got := SupervisedDispatch(seeder(context.Background())); got != supervised {
				t.Errorf("on an unmarked job context the seeder left %v, want %v", got, supervised)
			}
		})
	}
}

package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/jobs"
)

// guardDenial is the denial a fake child reports through the runner sink,
// standing in for the refusals the real runner captures (yaah-938).
var guardDenial = jobs.SubAgentDenial{
	Tool:    "bash",
	Command: "git status",
	Reason:  "command \"git status\" is not in this role's allow_commands allowlist",
	Rule:    "role_shell_allowlist",
}

// denialChild is a runner that reported one guard refusal during its run.
func denialChild(denial jobs.SubAgentDenial) TaskRunner {
	return func(ctx context.Context, _ string, _ SubAgentParams) (string, error) {
		jobs.CaptureSubAgentDenials(ctx, []jobs.SubAgentDenial{denial})
		return "the report", nil
	}
}

// TestRunSharedLaneReportsGuardDenials proves the envelope an orchestrator
// reads names a refusal the child's guard made: the shared lane's warnings
// carry it, and the model-facing envelope renders it (yaah-938).
func TestRunSharedLaneReportsGuardDenials(t *testing.T) {
	res, err := Run(context.Background(), DispatchRequest{
		Lane:   LaneShared,
		Params: SubAgentParams{Role: "worker"},
		Prompt: "do the work",
		Runner: denialChild(guardDenial),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := `supervisor guard denied bash (git status) by role_shell_allowlist: command "git status" is not in this role's allow_commands allowlist`
	if len(res.Warnings) != 1 || res.Warnings[0] != want {
		t.Fatalf("warnings = %q, want the one guard refusal", res.Warnings)
	}
	env := res.Envelope()
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "supervisor guard denied bash (git status)") {
		t.Errorf("envelope warnings = %q, want the guard refusal", env.Warnings)
	}
	if !strings.Contains(env.Warning, "supervisor guard denied bash") {
		t.Errorf("envelope warning = %q, want the first refusal in the scalar field too", env.Warning)
	}
}

// TestRetainedReportLaneCarriesGuardDenials proves the worktree report lane
// carries the refusals too: a report that could only gather part of what it was
// asked for must not read as an unqualified success.
func TestRetainedReportLaneCarriesGuardDenials(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	rt := RetainedRuntime{
		Runner:       denialChild(guardDenial),
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    NewProposalRegistry(),
	}

	res, err := startRetainedRun(context.Background(), context.Background(), rt,
		"review the thing", SubAgentParams{Role: "reviewer"}, 5*time.Second, 5*time.Second, true)
	if err != nil {
		t.Fatalf("startRetainedRun(report): %v", err)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %q, want the one guard refusal", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "supervisor guard denied bash (git status)") {
		t.Errorf("warning = %q, want it to name the tool and command", res.Warnings[0])
	}
}

// TestRetainedProposalLaneCarriesGuardDenials proves a sealed proposal's
// envelope names the refusals as well: the orchestrator settles what it reads.
func TestRetainedProposalLaneCarriesGuardDenials(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	rt := RetainedRuntime{
		Runner:       denialChild(guardDenial),
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    NewProposalRegistry(),
	}

	res, err := startRetainedRun(context.Background(), context.Background(), rt,
		"implement the thing", SubAgentParams{Role: "developer"}, 5*time.Second, 5*time.Second, false)
	if err != nil {
		t.Fatalf("startRetainedRun(proposal): %v", err)
	}
	if res.Proposal == nil {
		t.Fatal("proposal lane must return a sealed proposal")
	}
	found := false
	for _, w := range res.Proposal.Warnings {
		if strings.Contains(w, "supervisor guard denied bash (git status)") {
			found = true
		}
	}
	if !found {
		t.Errorf("proposal envelope warnings = %q, want the guard refusal", res.Proposal.Warnings)
	}
}

// TestDenialWarnings_Rendering pins the wording: a call with no command omits
// the parenthetical, a global-guard refusal omits the rule, and a long list is
// bounded so a retrying model cannot flood the envelope.
func TestDenialWarnings_Rendering(t *testing.T) {
	if got := denialWarnings(nil); got != nil {
		t.Errorf("no denials must render no warnings, got %q", got)
	}

	global := denialWarnings([]jobs.SubAgentDenial{{Tool: "bash", Reason: "Blocked destructive operation."}})
	if len(global) != 1 || global[0] != "supervisor guard denied bash: Blocked destructive operation." {
		t.Errorf("global refusal = %q, want it named without a command or rule", global)
	}

	many := make([]jobs.SubAgentDenial, 0, denialWarningBudget+2)
	for i := 0; i < denialWarningBudget+2; i++ {
		many = append(many, jobs.SubAgentDenial{Tool: "bash", Command: "cmd" + string(rune('a'+i)), Reason: "denied"})
	}
	got := denialWarnings(many)
	if len(got) != denialWarningBudget+1 {
		t.Fatalf("rendered %d warnings for %d denials, want %d plus the count line", len(got), len(many), denialWarningBudget)
	}
	if !strings.Contains(got[denialWarningBudget], "refused 2 more call(s)") {
		t.Errorf("last warning = %q, want the remainder count", got[denialWarningBudget])
	}
}

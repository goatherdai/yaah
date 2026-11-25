package runner

// supervised_nested_test.go pins the NESTED half of revert by construction
// (design §4.2, §9; yaah-ugtl review findings M1-nested and M2): a grandchild
// dispatch does not run inside its parent's isolated world, so under
// supervisor.require_reversible it is refused at the delegate boundary instead
// of being sent to the session tree the parent's revert never touches.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
)

// TestNestedDispatchWritesOutsideTheParentWorld pins the FACT the nested
// refusal rests on, rather than the policy it prefers: a dispatch running in an
// isolated worktree world contains its OWN file tools there, and does not
// contain a grandchild's.
//
// The parent registry roots the path validator at the dispatch's worktree, so a
// write to the session tree is refused. The nested dispatch — built the way
// makeTaskRunner builds it, from the parent's captured options with the nested
// call's empty Workdir — keeps the SESSION validator (rooted at the session,
// with no work dir), so the same write lands on the session tree. Reverting the
// parent's scope rolls back its worktree and never the session tree, which is
// why require_reversible cannot honestly accept the grandchild.
//
// If a future change routes nested dispatches into the parent's world, this test
// fails — which is the signal that the refusal below can be revisited.
func TestNestedDispatchWritesOutsideTheParentWorld(t *testing.T) {
	initTestRoles(t)

	repo := t.TempDir()
	wt := filepath.Join(repo, ".worktrees", "parent")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("make the dispatch worktree: %v", err)
	}

	parentOpts := taskRunnerOpts{
		pathValidator: tools.NewPathValidator(repo, false, nil),
	}
	parentReg, _, err := buildSubAgentRegistry(parentOpts,
		subagent.RoleProfile{Tools: []string{"read", "write", "delegate"}}, "developer", 1, wt)
	if err != nil {
		t.Fatalf("parent registry: %v", err)
	}
	if parentReg.Get("delegate") == nil {
		t.Fatal("a depth-1 dispatch that may delegate must hold the delegate tool")
	}

	// The grandchild's options, as makeTaskRunner derives them: dispatchOpts
	// over the parent's captured options, and the nested call's Workdir empty
	// (no dispatch runtime sets one for a nested call).
	grandOpts := dispatchOpts(parentOpts, context.Background())
	grandReg, _, err := buildSubAgentRegistry(grandOpts,
		subagent.RoleProfile{Tools: []string{"read", "write"}}, "counter", 0, "")
	if err != nil {
		t.Fatalf("grandchild registry: %v", err)
	}

	target := filepath.Join(repo, "escaped.txt")
	args := fmt.Sprintf(`{"filePath":%q,"content":"grandchild"}`, target)

	if _, perr := parentReg.Get("write").Execute(context.Background(), args); perr == nil {
		t.Fatal("the parent dispatch's write outside its own world must be refused; the fact this test pins no longer holds")
	}
	if _, gerr := grandReg.Get("write").Execute(context.Background(), args); gerr != nil {
		t.Fatalf("grandchild write outside the parent world = %v; if nested dispatches are now confined, revisit the refusal", gerr)
	}
	if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "grandchild" {
		t.Fatalf("grandchild write did not land on the session tree (content=%q err=%v)", string(b), rerr)
	}
}

// TestNestedDelegateRefusedUnderRequireReversible pins the refusal itself: the
// nested delegate tool the runner registers carries Nested and the session's
// require_reversible, so a grandchild dispatch is refused at the tool boundary
// — naming the sentinel — instead of being provisioned on the session tree.
func TestNestedDelegateRefusedUnderRequireReversible(t *testing.T) {
	initTestRoles(t)

	reg, _, err := buildSubAgentRegistry(taskRunnerOpts{supervised: true, requireReversible: true},
		subagent.RoleProfile{Tools: []string{"read", "delegate"}}, "developer", 1, "")
	if err != nil {
		t.Fatalf("buildSubAgentRegistry: %v", err)
	}
	nested, ok := reg.Get("delegate").(*tools.DelegateTool)
	if !ok {
		t.Fatalf("the registry's delegate tool is %T, want *tools.DelegateTool", reg.Get("delegate"))
	}
	if !nested.Nested {
		t.Error("a delegate tool registered inside a dispatch must be marked Nested")
	}
	if !nested.RequireReversible {
		t.Error("the session's require_reversible must reach the nested delegate tool")
	}

	_, execErr := nested.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter"}`)
	if !errors.Is(execErr, supervisionapi.ErrSupervisedNotReversible) {
		t.Fatalf("nested dispatch under require_reversible = %v, want ErrSupervisedNotReversible", execErr)
	}
}

// TestNestedDelegateAllowedWithoutRequireReversible pins the other direction:
// with the key off a nested dispatch runs exactly as it did before this
// refusal existed, so the unsupervised path is untouched.
func TestNestedDelegateAllowedWithoutRequireReversible(t *testing.T) {
	initTestRoles(t)

	var ran bool
	reg, _, err := buildSubAgentRegistry(taskRunnerOpts{supervised: true},
		subagent.RoleProfile{Tools: []string{"read", "delegate"}}, "developer", 1, "")
	if err != nil {
		t.Fatalf("buildSubAgentRegistry: %v", err)
	}
	nested, ok := reg.Get("delegate").(*tools.DelegateTool)
	if !ok {
		t.Fatalf("the registry's delegate tool is %T, want *tools.DelegateTool", reg.Get("delegate"))
	}
	nested.Runner = func(context.Context, string, tools.SubAgentParams) (string, error) {
		ran = true
		return "ok", nil
	}

	if _, execErr := nested.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter"}`); execErr != nil {
		t.Fatalf("nested dispatch without require_reversible = %v, want it to run", execErr)
	}
	if !ran {
		t.Error("the nested dispatch never reached the runner")
	}
}

// TestNewDelegateToolCarriesRequireReversible pins the wiring in one place: the
// config value handed to the runner reaches BOTH halves it gates — the tool
// field the per-call opt-out refusal reads, and the runner option the start gate
// reads. A config key that arrives at only one of them is a half-enforced key.
func TestNewDelegateToolCarriesRequireReversible(t *testing.T) {
	initTestRoles(t)

	mgr, store, root := newStartGateFixture(t, "sess-ugtl-wiring", nil)
	provider := &countingProvider{}
	tool := NewDelegateTool(DelegateToolOpts{
		Provider:          provider,
		ProviderName:      "test",
		SystemPrompt:      "BASE",
		ModelName:         "test-model",
		SessionID:         "sess-ugtl-wiring",
		ScopeManager:      mgr,
		RootScopeID:       root.ID(),
		TraceStore:        store,
		Supervised:        true,
		RequireReversible: true,
	})
	if !tool.RequireReversible {
		t.Fatal("supervisor.require_reversible must reach the delegate tool's own field")
	}

	// The runner half: the same value must reach taskRunnerOpts, which the start
	// gate consults. A dispatch on a pure-causal root owns no isolated sandbox,
	// so it is refused — proving the value crossed into the runner.
	_, err := tool.Runner(tools.WithSupervisedDispatch(context.Background(), true), "fix the widget",
		tools.SubAgentParams{Role: "developer"})
	if !errors.Is(err, ErrSupervisedNotReversible) {
		t.Fatalf("runner verdict = %v, want ErrSupervisedNotReversible (did require_reversible reach the runner?)", err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Errorf("the refused dispatch reached the model %d time(s); it must fail at start", got)
	}
}

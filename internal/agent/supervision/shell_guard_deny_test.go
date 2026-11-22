package supervision

import (
	"context"
	"path/filepath"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
)

// newGuardDenyService builds a service with the blocking shell guard
// enabled over a real trace store, and one forked sub-agent scope the
// deny fact can land on.
func newGuardDenyService(t *testing.T, allow []string) (*Service, *kerneltrace.SQLiteTraceStore) {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	bus := kerneltrace.NewEffectBus(64)
	t.Cleanup(bus.Close)
	mgr := goatherd.NewScopeManager(store)
	if _, err := mgr.Create("sub-worker", nil); err != nil {
		t.Fatalf("create scope: %v", err)
	}
	svc := NewService(mgr, bus, NewRegistry(), AdvisoryRules{ShellGuardAllowCommands: allow}, true, false, "")
	return svc, store
}

// denyFacts reads the shell-guard deny facts recorded on one owner path.
func denyFacts(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner string) []kerneltrace.Record {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, 100, kerneltrace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace for %s: %v", owner, err)
	}
	var out []kerneltrace.Record
	for _, id := range slice.FactIDs() {
		if rec, ok := slice.FactsByID[id].(kerneltrace.Record); ok && rec.Envelope.SchemaRef == SchemaShellGuardDeny {
			out = append(out, rec)
		}
	}
	return out
}

// TestGuardCheckDenyRecordsCommandAndReason pins the deny's legibility
// on both surfaces: the status firing counts the refusals for the rule,
// scope, and kind with the MOST RECENT refused command and the reason
// beside it — one line however many distinct commands were refused, so a
// session that keeps inventing commands cannot grow status without bound
// — and the trace keeps one fact per refusal on the refused scope's own
// owner path.
func TestGuardCheckDenyRecordsCommandAndReason(t *testing.T) {
	svc, store := newGuardDenyService(t, nil)
	check := svc.GuardCheck()

	const cmd = "git push --force origin main"
	reason := check("sub-worker", "bash", `{"command":"`+cmd+`"}`)
	if reason == "" {
		t.Fatal("the shell guard must deny a bash command outside the allowlist")
	}

	firings := svc.Firings()
	if len(firings) != 1 {
		t.Fatalf("firings = %+v, want exactly the guard's deny", firings)
	}
	f := firings[0]
	if f.Rule != shellGuardRuleName || f.Scope != "scope:sub-worker" || f.Kind != "deny" || f.Count != 1 {
		t.Errorf("firing = %+v, want destructive_shell_guard/scope:sub-worker/deny/1", f)
	}
	if f.Command != cmd {
		t.Errorf("firing command = %q, want the refused command %q", f.Command, cmd)
	}
	if f.Reason != reason {
		t.Errorf("firing reason = %q, want the reason the model was given %q", f.Reason, reason)
	}

	// The same refusal twice is still one line, counting two.
	check("sub-worker", "bash", `{"command":"`+cmd+`"}`)
	firings = svc.Firings()
	if len(firings) != 1 || firings[0].Count != 2 {
		t.Fatalf("firings after a repeat = %+v, want one line counting 2", firings)
	}

	// A different refused command folds into the same line — rule,
	// scope, and kind aggregate — and becomes the detail it shows, so
	// the count is complete and the line still names the latest refusal.
	const other = "ls -la"
	if r := check("sub-worker", "bash", `{"command":"`+other+`"}`); r == "" {
		t.Fatal("a second unlisted command must be denied too")
	}
	firings = svc.Firings()
	if len(firings) != 1 {
		t.Fatalf("firings = %+v, want one aggregate line per rule, scope, and kind", firings)
	}
	if f := firings[0]; f.Count != 3 || f.Command != other {
		t.Errorf("firing = %+v, want count 3 with the latest command %q", f, other)
	}

	// The durable half: one fact per refusal, on the refused scope's
	// owner path, naming the rule, the tool, the command, and the why.
	// The writes are off the dispatch path, so the test joins them first.
	svc.awaitDenyFacts()
	facts := denyFacts(t, store, "sub-worker")
	if len(facts) != 3 {
		t.Fatalf("deny facts = %d, want one per refusal (3)", len(facts))
	}
	// Writes are async, so order is not part of the contract: assert the
	// multiset of refused commands and the shared fields, not which fact
	// landed first.
	cmds := map[string]int{}
	for _, rec := range facts {
		if rec.Envelope.Mode != kerneltrace.Capture {
			t.Errorf("deny fact mode = %v, want Capture (an outcome, not a declaration)", rec.Envelope.Mode)
		}
		if rec.View != nil && rec.View.KindLabel != ShellGuardDenyKind {
			t.Errorf("deny fact kind = %q, want %q", rec.View.KindLabel, ShellGuardDenyKind)
		}
		if got := rec.Body.Payload["rule"]; got != shellGuardRuleName {
			t.Errorf("deny fact rule = %v, want %q", got, shellGuardRuleName)
		}
		if got := rec.Body.Payload["tool"]; got != "bash" {
			t.Errorf("deny fact tool = %v, want %q", got, "bash")
		}
		if got := rec.Body.Payload["reason"]; got != reason {
			t.Errorf("deny fact reason = %v, want %q", got, reason)
		}
		gotCmd, _ := rec.Body.Payload["cmd"].(string)
		cmds[gotCmd]++
	}
	if cmds[cmd] != 2 || cmds[other] != 1 {
		t.Errorf("deny fact commands = %v, want %q x2 and %q x1", cmds, cmd, other)
	}
}

// TestGuardCheckDenyFactWithoutScope pins the unaddressable-scope path:
// a check for an owner that never forked still denies and still counts,
// it simply has no owner path to record the fact on.
func TestGuardCheckDenyFactWithoutScope(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	bus := kerneltrace.NewEffectBus(64)
	t.Cleanup(bus.Close)
	svc := NewService(goatherd.NewScopeManager(store), bus, NewRegistry(), AdvisoryRules{}, true, false, "")

	if reason := svc.GuardCheck()("never-forked", "bash", `{"command":"ls"}`); reason == "" {
		t.Fatal("the guard must deny regardless of whether the scope exists")
	}
	if firings := svc.Firings(); len(firings) != 1 || firings[0].Command != "ls" {
		t.Errorf("firings = %+v, want the deny counted with its command", firings)
	}
	if facts := denyFacts(t, store, "never-forked"); len(facts) != 0 {
		t.Errorf("deny facts = %d, want none without an addressable scope", len(facts))
	}
}

// TestGuardCheckAllowRecordsNoDenyDetail is the negative half: a
// command the role's allowlist admits leaves no firing and no fact —
// the legibility belongs to refusals only.
func TestGuardCheckAllowRecordsNoDenyDetail(t *testing.T) {
	svc, store := newGuardDenyService(t, []string{"git status*"})
	if reason := svc.GuardCheck()("sub-worker", "bash", `{"command":"git status -s"}`); reason != "" {
		t.Fatalf("an allowlisted command must pass, got reason %q", reason)
	}
	if firings := svc.Firings(); len(firings) != 0 {
		t.Errorf("firings = %+v, want none for an allowed command", firings)
	}
	if facts := denyFacts(t, store, "sub-worker"); len(facts) != 0 {
		t.Errorf("deny facts = %d, want none for an allowed command", len(facts))
	}
}

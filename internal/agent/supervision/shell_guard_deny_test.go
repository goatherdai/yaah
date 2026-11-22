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
// on both surfaces: the status firing names the refused command and the
// reason beside the count, aggregated per (command, reason) so a repeat
// adds to one line and a different command gets its own, and the trace
// keeps one fact per refusal on the refused scope's own owner path.
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

	// The same refusal twice is one line counting two — the status stays
	// a summary, and the count still owes its detail.
	check("sub-worker", "bash", `{"command":"`+cmd+`"}`)
	firings = svc.Firings()
	if len(firings) != 1 || firings[0].Count != 2 {
		t.Fatalf("firings after a repeat = %+v, want one line counting 2", firings)
	}

	// A different command is its own line: the aggregate never hides
	// what was refused behind a count.
	const other = "ls -la"
	if r := check("sub-worker", "bash", `{"command":"`+other+`"}`); r == "" {
		t.Fatal("a second unlisted command must be denied too")
	}
	firings = svc.Firings()
	if len(firings) != 2 {
		t.Fatalf("firings = %+v, want one line per refused command", firings)
	}
	seen := map[string]int{}
	for _, f := range firings {
		seen[f.Command] = f.Count
	}
	if seen[cmd] != 2 || seen[other] != 1 {
		t.Errorf("firing counts by command = %v, want %q=2 and %q=1", seen, cmd, other)
	}

	// The durable half: one fact per refusal, on the refused scope's
	// owner path, naming the rule, the tool, the command, and the why.
	facts := denyFacts(t, store, "sub-worker")
	if len(facts) != 3 {
		t.Fatalf("deny facts = %d, want one per refusal (3)", len(facts))
	}
	first := facts[0]
	if first.Envelope.Mode != kerneltrace.Capture {
		t.Errorf("deny fact mode = %v, want Capture (an outcome, not a declaration)", first.Envelope.Mode)
	}
	if first.View != nil && first.View.KindLabel != ShellGuardDenyKind {
		t.Errorf("deny fact kind = %q, want %q", first.View.KindLabel, ShellGuardDenyKind)
	}
	want := map[string]string{
		"rule":   shellGuardRuleName,
		"tool":   "bash",
		"cmd":    cmd,
		"reason": reason,
	}
	for key, wantValue := range want {
		if got := first.Body.Payload[key]; got != wantValue {
			t.Errorf("deny fact %s = %v, want %q", key, got, wantValue)
		}
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

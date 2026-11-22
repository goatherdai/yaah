package supervision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	goatherd "github.com/goatherdai/goatherd-runtime"
)

// denyArgsJSON builds the shell tool's argument payload for one command,
// quoted the way the tool receives it.
func denyArgsJSON(cmd string) string {
	payload, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// TestFiringsBoundedByRuleScopeKind pins the fix for unbounded session
// state: firings aggregate per rule, scope, and kind, so a run of distinct
// refusals — the shape a model produces when it keeps inventing commands —
// stays ONE status line that counts them all. Keying by the refused command
// grew the map once per command, so status grew with the model's output.
func TestFiringsBoundedByRuleScopeKind(t *testing.T) {
	svc, _ := newGuardDenyService(t, nil)
	check := svc.GuardCheck()

	const refusals = 500
	for i := 0; i < refusals; i++ {
		cmd := fmt.Sprintf("rm -rf /tmp/probe-%d", i)
		if reason := check("sub-worker", "bash", denyArgsJSON(cmd)); reason == "" {
			t.Fatalf("refusal %d must be denied", i)
		}
	}

	firings := svc.Firings()
	if len(firings) != 1 {
		t.Fatalf("firings = %d lines for %d distinct refusals, want 1", len(firings), refusals)
	}
	if f := firings[0]; f.Count != refusals {
		t.Errorf("firing count = %d, want %d (the count is the aggregate)", f.Count, refusals)
	}
	// The detail is the most recent refusal, not the first.
	if got, want := firings[0].Command, fmt.Sprintf("rm -rf /tmp/probe-%d", refusals-1); got != want {
		t.Errorf("firing command = %q, want the latest refusal %q", got, want)
	}
	svc.awaitDenyFacts()
}

// TestFiringKeysCapped pins the backstop: past maxFiringKeys distinct keys —
// which a session forking unbounded scopes can still produce — the surplus
// folds into one per-rule-and-kind overflow entry, so the map is bounded and
// the total count is still complete.
func TestFiringKeysCapped(t *testing.T) {
	svc, _ := newGuardDenyService(t, nil)
	check := svc.GuardCheck()

	const owners = maxFiringKeys + 40
	for i := 0; i < owners; i++ {
		// Distinct owners, no forked scopes: every refusal is counted and
		// none reaches the trace.
		check(fmt.Sprintf("owner-%d", i), "bash", denyArgsJSON("rm -rf /tmp/x"))
	}

	firings := svc.Firings()
	if len(firings) > maxFiringKeys+1 {
		t.Fatalf("firings = %d keys, want at most %d plus the overflow entry", len(firings), maxFiringKeys)
	}
	total := 0
	for _, f := range firings {
		total += f.Count
	}
	if total != owners {
		t.Errorf("total count = %d, want %d — the cap must not lose refusals", total, owners)
	}
}

// TestDenyDetailTruncated pins the byte bound on the deny detail: a command
// is model-authored text and can be megabytes long, so both the status
// firing and the durable fact keep at most maxDenyDetailBytes of it.
func TestDenyDetailTruncated(t *testing.T) {
	svc, store := newGuardDenyService(t, nil)
	check := svc.GuardCheck()

	oversized := "rm -rf /tmp/" + strings.Repeat("a", 4*maxDenyDetailBytes)
	if reason := check("sub-worker", "bash", denyArgsJSON(oversized)); reason == "" {
		t.Fatal("an oversized command must still be denied")
	}

	firings := svc.Firings()
	if len(firings) != 1 {
		t.Fatalf("firings = %+v, want one deny", firings)
	}
	got := firings[0].Command
	if len(got) > maxDenyDetailBytes+len("…") {
		t.Errorf("firing command is %d bytes, want at most %d", len(got), maxDenyDetailBytes+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("firing command = %q, want the truncation marked", got[len(got)-8:])
	}
	if !strings.HasPrefix(got, "rm -rf /tmp/") {
		t.Errorf("firing command = %q, want the stored prefix kept", got)
	}

	svc.awaitDenyFacts()
	facts := denyFacts(t, store, "sub-worker")
	if len(facts) != 1 {
		t.Fatalf("deny facts = %d, want one", len(facts))
	}
	factCmd, _ := facts[0].Body.Payload["cmd"].(string)
	if len(factCmd) > maxDenyDetailBytes+len("…") {
		t.Errorf("fact cmd is %d bytes, want at most %d", len(factCmd), maxDenyDetailBytes+len("…"))
	}
	if factCmd != got {
		t.Errorf("fact cmd = %q, want the same bounded detail the status carries (%q)", factCmd, got)
	}
}

// TestGuardCheckDoesNotWaitForStalledFactStore pins the ordering the deny
// path owes: the verdict is decided before the fact is written, so a trace
// store that stalls must not delay it. The substituted writer blocks until
// the test releases it; GuardCheck must still return promptly.
func TestGuardCheckDoesNotWaitForStalledFactStore(t *testing.T) {
	svc, _ := newGuardDenyService(t, nil)

	blocked := make(chan struct{})
	release := make(chan struct{})
	svc.denyFact = func(ctx context.Context, _ *goatherd.Scope, _ map[string]any) error {
		close(blocked)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	start := time.Now()
	reason := svc.GuardCheck()("sub-worker", "bash", denyArgsJSON("rm -rf /tmp/probe"))
	elapsed := time.Since(start)
	if reason == "" {
		t.Fatal("the guard must deny while its fact write is still in flight")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("GuardCheck took %s with a stalled fact store; the verdict must not wait on it", elapsed)
	}
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the deny fact write never started")
	}
	// The firing is already recorded: the count does not depend on the
	// durable half.
	if firings := svc.Firings(); len(firings) != 1 || firings[0].Count != 1 {
		t.Errorf("firings = %+v, want the deny counted before the fact lands", firings)
	}
	close(release)
	svc.awaitDenyFacts()
}

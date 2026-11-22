package supervision

import (
	"strings"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
)

// allowlistCheck wires a role's own allowlist over the service's global
// guard check, with the service's recorder — the shape the runner hands
// a sub-agent loop.
func allowlistCheck(svc *Service, allow []string) func(ownerID, toolName, argsJSON string) string {
	return WithShellAllowlist(svc.GuardCheck(), allow, svc.ShellAllowlistDenyRecorder())
}

// TestShellAllowlistDenyRecordsOnBothSurfaces pins the role-allowlist
// deny's legibility: a refusal the dispatching role's own allow_commands
// list makes is counted on the status surface under its own rule — with
// the refused command and the reason the model was given — and appends a
// SchemaShellGuardDeny fact on the refused scope's owner path whose
// payload names that same rule.
//
// The global guard admits the command here, so a firing or fact can only
// come from the role's list: the two authorities are told apart by rule
// name, never conflated.
func TestShellAllowlistDenyRecordsOnBothSurfaces(t *testing.T) {
	svc, store := newGuardDenyService(t, []string{"git status*"})
	check := allowlistCheck(svc, []string{"go test*"})

	const cmd = "git status -s" // admitted globally, outside this role's list
	reason := check("sub-worker", "bash", `{"command":"`+cmd+`"}`)
	if reason == "" {
		t.Fatal("a command outside the role's allowlist must be denied")
	}
	if !strings.Contains(reason, "not in this role's allow_commands allowlist") {
		t.Errorf("reason = %q, want the role-allowlist refusal unchanged", reason)
	}

	firings := svc.Firings()
	if len(firings) != 1 {
		t.Fatalf("firings = %+v, want exactly the role allowlist's deny", firings)
	}
	f := firings[0]
	if f.Rule != "role_shell_allowlist" {
		t.Errorf("firing rule = %q, want %q", f.Rule, shellAllowlistRuleName)
	}
	if f.Scope != "scope:sub-worker" {
		t.Errorf("firing scope = %q, want the canonical scope id scope:sub-worker", f.Scope)
	}
	if f.Kind != "deny" || f.Count != 1 {
		t.Errorf("firing kind/count = %q/%d, want deny/1", f.Kind, f.Count)
	}
	if f.Command != cmd {
		t.Errorf("firing command = %q, want the refused command %q", f.Command, cmd)
	}
	if f.Reason != reason {
		t.Errorf("firing reason = %q, want the reason the model was given %q", f.Reason, reason)
	}
	for _, other := range firings {
		if other.Rule == shellGuardRuleName {
			t.Errorf("role-allowlist deny counted as %q: the two authorities must not be conflated", shellGuardRuleName)
		}
	}

	// The fact write is off the dispatch path, so join it before reading.
	svc.awaitDenyFacts()
	facts := denyFacts(t, store, "sub-worker")
	if len(facts) != 1 {
		t.Fatalf("deny facts = %d, want one per refusal", len(facts))
	}
	fact := facts[0]
	if fact.Envelope.Mode != kerneltrace.Capture {
		t.Errorf("deny fact mode = %v, want Capture", fact.Envelope.Mode)
	}
	if fact.View != nil && fact.View.KindLabel != ShellGuardDenyKind {
		t.Errorf("deny fact kind = %q, want %q", fact.View.KindLabel, ShellGuardDenyKind)
	}
	want := map[string]string{
		"rule":   shellAllowlistRuleName,
		"tool":   "bash",
		"cmd":    cmd,
		"reason": reason,
	}
	for key, wantValue := range want {
		if got := fact.Body.Payload[key]; got != wantValue {
			t.Errorf("deny fact %s = %v, want %q", key, got, wantValue)
		}
	}
}

// TestShellAllowlistDenyWithoutCommandRecords pins the other deny
// branch: a shell call the plan cannot classify carries no command to
// match, and that refusal is legible too — it is exactly the fail-closed
// verdict an operator most needs to see.
func TestShellAllowlistDenyWithoutCommandRecords(t *testing.T) {
	svc, store := newGuardDenyService(t, nil)
	check := allowlistCheck(svc, []string{"go test*"})

	reason := check("sub-worker", "bash", `{}`)
	const want = "shell call carries no command to check against the role's allowlist"
	if reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}

	firings := svc.Firings()
	if len(firings) != 1 {
		t.Fatalf("firings = %+v, want the no-command refusal counted", firings)
	}
	if f := firings[0]; f.Rule != shellAllowlistRuleName || f.Kind != "deny" || f.Command != "" || f.Reason != want {
		t.Errorf("firing = %+v, want role_shell_allowlist/deny with an empty command and the refusal reason", f)
	}

	// The fact write is off the dispatch path, so join it before reading.
	svc.awaitDenyFacts()
	facts := denyFacts(t, store, "sub-worker")
	if len(facts) != 1 {
		t.Fatalf("deny facts = %d, want the no-command refusal recorded", len(facts))
	}
	if got := facts[0].Body.Payload["rule"]; got != shellAllowlistRuleName {
		t.Errorf("deny fact rule = %v, want %q", got, shellAllowlistRuleName)
	}
	if got := facts[0].Body.Payload["cmd"]; got != "" {
		t.Errorf("deny fact cmd = %v, want empty", got)
	}
}

// TestShellAllowlistAllowRecordsNothing is the negative half: an
// admitted command and a non-shell tool leave no firing and no fact —
// the legibility belongs to refusals only.
func TestShellAllowlistAllowRecordsNothing(t *testing.T) {
	svc, store := newGuardDenyService(t, nil)
	check := allowlistCheck(svc, []string{"go test*"})

	if reason := check("sub-worker", "bash", `{"command":"go test ./internal/agent/..."}`); reason != "" {
		t.Fatalf("an allowlisted command must pass, got reason %q", reason)
	}
	if reason := check("sub-worker", "read", `{"path":"/workspace/file"}`); reason != "" {
		t.Fatalf("a non-shell tool must pass through, got reason %q", reason)
	}
	if firings := svc.Firings(); len(firings) != 0 {
		t.Errorf("firings = %+v, want none for admitted calls", firings)
	}
	if facts := denyFacts(t, store, "sub-worker"); len(facts) != 0 {
		t.Errorf("deny facts = %d, want none for admitted calls", len(facts))
	}
}

// TestWithShellAllowlistWithoutRecorderIsUnchanged is the backward
// compatibility oracle: the pre-recorder call shape — no DenyRecorder
// argument — returns byte-identical verdicts and records nothing, so
// every existing caller keeps its exact behavior.
func TestWithShellAllowlistWithoutRecorderIsUnchanged(t *testing.T) {
	args := []string{
		`{"command":"git status -s"}`,
		`{}`,
		`{"command":"go test ./..."}`,
		`{"path":"/workspace/file"}`,
	}
	svc, store := newGuardDenyService(t, nil)

	// The plain wrapper first: nothing may be recorded through it.
	plain := WithShellAllowlist(svc.GuardCheck(), []string{"go test*"})
	plainReasons := make([]string, len(args))
	for i, a := range args {
		plainReasons[i] = plain("sub-worker", "bash", a)
	}
	if firings := svc.Firings(); len(firings) != 0 {
		t.Errorf("firings = %+v, want none without a recorder", firings)
	}
	if facts := denyFacts(t, store, "sub-worker"); len(facts) != 0 {
		t.Errorf("deny facts = %d, want none without a recorder", len(facts))
	}

	// The recorder-carrying wrapper must reach the same verdicts byte for
	// byte; only the recording differs.
	withRec := allowlistCheck(svc, []string{"go test*"})
	for i, a := range args {
		if got := withRec("sub-worker", "bash", a); got != plainReasons[i] {
			t.Errorf("args %s: with recorder %q, without %q — verdicts must not differ",
				a, got, plainReasons[i])
		}
	}
	// The recorder's fact writes run off the dispatch path; join them so
	// the test does not tear the store down under a live writer.
	svc.awaitDenyFacts()
}

// TestShellAllowlistDenyWithoutScope pins the unaddressable-scope path:
// a refusal for an owner that never forked is still counted — with the
// owner it names — and simply has no owner path for the fact.
func TestShellAllowlistDenyWithoutScope(t *testing.T) {
	svc, store := newGuardDenyService(t, nil)
	check := allowlistCheck(svc, []string{"go test*"})

	if reason := check("never-forked", "bash", `{"command":"git status"}`); reason == "" {
		t.Fatal("the role's allowlist must deny regardless of whether the scope exists")
	}
	firings := svc.Firings()
	if len(firings) != 1 || firings[0].Rule != shellAllowlistRuleName || firings[0].Scope != "never-forked" {
		t.Errorf("firings = %+v, want one role_shell_allowlist deny under the owner it addressed", firings)
	}
	if facts := denyFacts(t, store, "never-forked"); len(facts) != 0 {
		t.Errorf("deny facts = %d, want none without an addressable scope", len(facts))
	}
}

// TestShellAllowlistDenyRecorderNilService pins the disabled shape: no
// service is no recorder, which every caller reads as "record nothing".
func TestShellAllowlistDenyRecorderNilService(t *testing.T) {
	var svc *Service
	if rec := svc.ShellAllowlistDenyRecorder(); rec != nil {
		t.Error("a nil service must yield a nil recorder")
	}
}

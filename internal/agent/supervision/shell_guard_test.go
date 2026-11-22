package supervision

import (
	"fmt"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/effectpolicy"
	"github.com/goatherdai/yaah/internal/permissions"
)

// legacyCommandAllowed is the pre-plan allowlist probe, kept verbatim as the
// oracle the plan-based guard must agree with: patterns are matched in full
// against the raw command (permissions.GlobMatch), and an empty command
// matches nothing even against "*".
func legacyCommandAllowed(allow []string, command string) bool {
	if command == "" {
		return false
	}
	for _, pattern := range allow {
		if permissions.GlobMatch(pattern, command) {
			return true
		}
	}
	return false
}

// legacyShellDeny is the pre-plan shell guard's verdict, kept verbatim as the
// oracle: declare-fire classification plus the glob allowlist probe.
func legacyShellDeny(allow []string, kind, cmd string) bool {
	switch kind {
	case "bash", "powershell":
		return !legacyCommandAllowed(allow, cmd)
	case "background_process":
		if cmd == "" {
			return false
		}
		return !legacyCommandAllowed(allow, cmd)
	}
	return false
}

// planShellDeny is the plan-based guard's verdict through the real rule: the
// effect is classified exactly as GuardCheck's event carries it (the command
// under "cmd", absent when the call has none).
func planShellDeny(match func(kerneltrace.EffectEvent) bool, kind, cmd string) bool {
	payload := map[string]any{}
	if cmd != "" {
		payload["cmd"] = cmd
	}
	return match(kerneltrace.EffectEvent{
		TraceOwnerID: "sub-worker",
		Mode:         kerneltrace.Declaration,
		SchemaRef:    "yaah.tool." + kind + ".v1",
		KindLabel:    kind,
		Payload:      payload,
	})
}

// TestShellGuardPlan_MatchesGlobDecision is the security-critical invariant of
// the effectpolicy swap: swapping the allowlist mechanism from
// permissions.GlobMatch to effectpolicy.Plan.Check must not move a single
// verdict. For every (allowlist, kind, command) below the plan-based guard
// must deny exactly what the pre-plan guard denied — prefix and exact globs,
// "?" globs, bare-bracket and brace literals, an invalid pattern, an empty
// command, an empty allowlist, background_process's read-only actions, and
// non-shell kinds included.
func TestShellGuardPlan_MatchesGlobDecision(t *testing.T) {
	allowlists := []struct {
		name  string
		allow []string
	}{
		{name: "nil", allow: nil},
		{name: "empty", allow: []string{}},
		{name: "star", allow: []string{"*"}},
		{name: "prefix", allow: []string{"git status*"}},
		{name: "two prefixes", allow: []string{"git status*", "go test*"}},
		{name: "question", allow: []string{"file?.md"}},
		{name: "bracket literal", allow: []string{"git [st]*"}},
		{name: "brace literal", allow: []string{"{git,go} *"}},
		{name: "exact", allow: []string{"git status"}},
		{name: "regex metachars are literal", allow: []string{"git.push*"}},
		{name: "windows path", allow: []string{"dir C:/*"}},
		{name: "invalid pattern", allow: []string{"["}},
	}
	commands := []string{
		"",
		"git status",
		"git status -s",
		"echo git status",
		"go test ./...",
		"file1.md",
		"file12.md",
		"git stash",
		"git [st]ash",
		"{git,go} status",
		"gitXpush",
		`dir C:\Users\greg`,
		"rm -rf /tmp/probe",
	}
	kinds := []string{"bash", "powershell", "background_process", "write", "read", "edit"}

	for _, al := range allowlists {
		rule := shellGuardRule(shellGuardPlan(al.allow))
		for _, kind := range kinds {
			for _, cmd := range commands {
				t.Run(fmt.Sprintf("%s/%s/%q", al.name, kind, cmd), func(t *testing.T) {
					want := legacyShellDeny(al.allow, kind, cmd)
					got := planShellDeny(rule.Match, kind, cmd)
					if got != want {
						t.Fatalf("deny(%v, %q, %q) = %v, want %v (the pre-plan glob verdict)",
							al.allow, kind, cmd, got, want)
					}
				})
			}
		}
	}
}

// TestShellGuardPlan_EmptyAllowlistDeniesEveryShell pins the plan's own
// reading of the guard's historical default-deny: Nothing().Not() is All(), so
// the deny clause covers the whole shell surface — never the reverse, which a
// mistakenly un-negated empty allowlist would produce.
func TestShellGuardPlan_EmptyAllowlistDeniesEveryShell(t *testing.T) {
	plan := shellGuardPlan(nil)
	for _, kind := range []string{"bash", "powershell"} {
		for _, cmd := range []string{"git status", "ls"} {
			eff := effectpolicy.Effect{Kind: kind, Fields: map[string]any{"command": cmd}}
			if err := plan.Check(eff); err == nil {
				t.Errorf("%s with command %q must be denied by an empty allowlist", kind, cmd)
			}
		}
	}
	if err := plan.Check(effectpolicy.Effect{Kind: "background_process", Fields: map[string]any{"command": "ls"}}); err == nil {
		t.Error("background_process start must be denied by an empty allowlist")
	}
	// The deny clause covers the shell surface only: structured tools stay
	// outside the plan's concern either way.
	if err := plan.Check(effectpolicy.Effect{Kind: "write", Fields: map[string]any{"path": "/workspace/x"}}); err != nil {
		t.Errorf("a structured tool must not be touched by the shell plan: %v", err)
	}
}

// TestShellGuardPlan_AllowsOnlyTheAllowlist pins the positive direction of the
// same plan: a command the allowlist names is permitted, and a non-shell kind
// is outside the plan's concern entirely.
func TestShellGuardPlan_AllowsOnlyTheAllowlist(t *testing.T) {
	plan := shellGuardPlan([]string{"git status*", "go test*"})
	allowed := []effectpolicy.Effect{
		{Kind: "bash", Fields: map[string]any{"command": "git status -s"}},
		{Kind: "powershell", Fields: map[string]any{"command": "go test ./..."}},
		{Kind: "background_process", Fields: map[string]any{"command": "git status"}},
		{Kind: "write", Fields: map[string]any{"path": "/workspace/x"}},
		{Kind: "read"},
	}
	for _, eff := range allowed {
		if err := plan.Check(eff); err != nil {
			t.Errorf("plan must permit %s: %v", eff, err)
		}
	}
	denied := []effectpolicy.Effect{
		{Kind: "bash", Fields: map[string]any{"command": "echo git status"}},
		{Kind: "bash", Fields: map[string]any{"command": "rm -rf /tmp/x"}},
		{Kind: "bash"}, // no command field at all — unclassifiable
	}
	for _, eff := range denied {
		if err := plan.Check(eff); err == nil {
			t.Errorf("plan must deny %s", eff)
		}
	}
}

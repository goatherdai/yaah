package dispatch

import (
	"errors"
	"github.com/goatherdai/yaah/internal/tools/substrate"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/config"
)

func TestResolveDispatchWorld(t *testing.T) {
	t.Run("auto uses the worktree while the container lane is unshipped", func(t *testing.T) {
		rt := RetainedRuntime{World: "auto", Containment: "auto", Network: "allow_all"}
		res, err := resolveDispatchWorld(rt, "developer", false)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != "worktree" || res.Degraded {
			t.Fatalf("got %+v, want an undegraded worktree", res)
		}
	})

	t.Run("explicit container fails closed", func(t *testing.T) {
		rt := RetainedRuntime{World: "container"}
		if _, err := resolveDispatchWorld(rt, "developer", false); err == nil {
			t.Fatal("container requested on a build without the lane must fail")
		} else if !strings.Contains(err.Error(), "container") {
			t.Fatalf("error = %q, want it to name the world", err)
		}
	})

	t.Run("a report dispatch without a configured image keeps the worktree silently", func(t *testing.T) {
		rt := RetainedRuntime{World: "auto"}
		res, err := resolveDispatchWorld(rt, "reviewer", true)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != subagent.WorldWorktree || res.Degraded {
			t.Fatalf("got %+v, want an undegraded worktree", res)
		}
	})

	t.Run("explicit container names the missing image for a report dispatch", func(t *testing.T) {
		rt := RetainedRuntime{World: "container"}
		if _, err := resolveDispatchWorld(rt, "reviewer", true); err == nil {
			t.Fatal("container requested without a configured image must fail closed")
		} else if !strings.Contains(err.Error(), "workspace.containerd.image") {
			t.Fatalf("error = %q, want it to name the config key", err)
		}
	})

	t.Run("per-role override wins", func(t *testing.T) {
		rt := RetainedRuntime{
			World:        "auto",
			WorldForRole: func(role string) string { return "container" },
		}
		if _, err := resolveDispatchWorld(rt, "reviewer", true); err == nil {
			t.Fatal("the role override must be honoured (and fail closed)")
		}
	})

	t.Run("a wired dispatch setup lets an auto proposal use the container", func(t *testing.T) {
		rt := RetainedRuntime{
			World:           "auto",
			ContainerWorlds: true,
			DispatchWorlds:  &DispatchWorlds{},
			Containment:     "auto",
			Network:         "allow_all",
		}
		res, err := resolveDispatchWorld(rt, "developer", false)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != subagent.WorldContainer || res.Degraded {
			t.Fatalf("got %+v, want an undegraded container", res)
		}
	})

	t.Run("explicit container keeps a proposal on the container world", func(t *testing.T) {
		rt := RetainedRuntime{
			World:           "container",
			ContainerWorlds: true,
			DispatchWorlds:  &DispatchWorlds{},
			Containment:     "auto",
			Network:         "allow_all",
		}
		res, err := resolveDispatchWorld(rt, "developer", false)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != subagent.WorldContainer || res.Degraded {
			t.Fatalf("got %+v, want an undegraded container", res)
		}
	})

	t.Run("a shipped lane reports a missing setup as a degradation", func(t *testing.T) {
		rt := RetainedRuntime{World: "auto", ContainerWorlds: true, Containment: "auto", Network: "allow_all"}
		res, err := resolveDispatchWorld(rt, "developer", false)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != "worktree" || !res.Degraded {
			t.Fatalf("got %+v, want a degraded worktree", res)
		}
		if !strings.Contains(res.Reason, "settlement") {
			t.Fatalf("reason %q must name the blocker", res.Reason)
		}
	})

	t.Run("a restrictive egress posture chooses a container world", func(t *testing.T) {
		rt := RetainedRuntime{
			World:           "auto",
			ContainerWorlds: true,
			DispatchWorlds:  &DispatchWorlds{},
			Containment:     "auto",
			Network:         "deny_all",
		}
		res, err := resolveDispatchWorld(rt, "reviewer", true)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != subagent.WorldContainer || res.Degraded {
			t.Fatalf("got %+v, want an undegraded container", res)
		}

		explicit := rt
		explicit.World = "container"
		res, err = resolveDispatchWorld(explicit, "reviewer", true)
		if err != nil {
			t.Fatalf("explicit container with deny_all: %v", err)
		}
		if res.Resolved != subagent.WorldContainer {
			t.Fatalf("got %+v, want container", res)
		}
	})

	t.Run("restrictive grants choose a container world", func(t *testing.T) {
		rt := RetainedRuntime{
			World:           "auto",
			ContainerWorlds: true,
			DispatchWorlds:  &DispatchWorlds{},
			Containment:     "auto",
			Network:         "allow_all",
			Grants:          []config.GrantConfig{{Path: "src", Access: "readwrite"}},
		}
		res, err := resolveDispatchWorld(rt, "developer", false)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != subagent.WorldContainer || res.Degraded {
			t.Fatalf("got %+v, want an undegraded container", res)
		}
	})

	t.Run("a report dispatch uses a wired container world", func(t *testing.T) {
		rt := RetainedRuntime{
			World:           "auto",
			ContainerWorlds: true,
			DispatchWorlds:  &DispatchWorlds{},
			Containment:     "auto",
			Network:         "allow_all",
		}
		res, err := resolveDispatchWorld(rt, "reviewer", true)
		if err != nil {
			t.Fatalf("resolveDispatchWorld: %v", err)
		}
		if res.Resolved != subagent.WorldContainer || res.Degraded {
			t.Fatalf("got %+v, want an undegraded container", res)
		}
	})
}

// TestDispatchWorldPreflightFailure pins the preflight decision: only an
// explicit container request fails closed; auto steps down to the worktree with
// the failure named in the recorded reason.
func TestDispatchWorldPreflightFailure(t *testing.T) {
	cause := errors.New("open probe sandbox: daemon unreachable")

	t.Run("auto degrades to the worktree", func(t *testing.T) {
		res, err := dispatchWorldPreflightFailure(
			subagent.WorldResolution{Requested: subagent.WorldAuto, Resolved: subagent.WorldContainer},
			cause,
		)
		if err != nil {
			t.Fatalf("auto must not fail on a preflight failure: %v", err)
		}
		if res.Resolved != subagent.WorldWorktree || !res.Degraded {
			t.Fatalf("got %+v, want a degraded worktree", res)
		}
		if !strings.Contains(res.Reason, "daemon unreachable") {
			t.Errorf("reason %q must name the failure", res.Reason)
		}
	})

	t.Run("container fails closed", func(t *testing.T) {
		res, err := dispatchWorldPreflightFailure(
			subagent.WorldResolution{Requested: subagent.WorldContainer, Resolved: subagent.WorldContainer},
			cause,
		)
		if err == nil {
			t.Fatalf("an explicit container request must fail closed, got %+v", res)
		}
		if !errors.Is(err, cause) {
			t.Errorf("error = %v, want it to wrap the preflight failure", err)
		}
		if !strings.Contains(err.Error(), "container") {
			t.Errorf("error = %q, want it to name the world", err)
		}
	})
}

// TestWorldFactPayloadShape pins the trace fact's key set to the parity ledger.
func TestWorldFactPayloadShape(t *testing.T) {
	got := worldFactPayload(subagent.WorldResolution{
		Requested: subagent.WorldAuto,
		Resolved:  subagent.WorldWorktree,
		Degraded:  true,
		Reason:    "container unavailable",
	})
	want := map[string]bool{"requested": true, "resolved": true, "degraded": true, "reason": true}
	if len(got) != len(want) {
		t.Fatalf("payload keys = %v, want %v", got, want)
	}
	for k := range got {
		if !want[k] {
			t.Errorf("unexpected world-fact key %q", k)
		}
	}
	if got["requested"] != "auto" || got["resolved"] != "worktree" || got["degraded"] != true {
		t.Errorf("payload = %v", got)
	}
}

func TestResolveDispatchWorld_ContainerSessionSibling(t *testing.T) {
	rt := RetainedRuntime{
		World:           "auto",
		ContainerWorlds: true,
		Containment:     "auto",
		Network:         "allow_all",
		DispatchWorlds: &DispatchWorlds{
			SessionSandbox: &substrate.SessionSandbox{},
		},
	}

	report, err := resolveDispatchWorld(rt, "reviewer", true)
	if err != nil {
		t.Fatalf("report dispatch: %v", err)
	}
	if report.Resolved != subagent.WorldContainer {
		t.Fatalf("report resolved = %q, want container", report.Resolved)
	}

	proposal, err := resolveDispatchWorld(rt, "developer", false)
	if err != nil {
		t.Fatalf("proposal dispatch: %v", err)
	}
	if proposal.Resolved != subagent.WorldContainer {
		t.Fatalf("proposal resolved = %q, want container", proposal.Resolved)
	}
}

// TestBaseRefWorldRefusal is the pure base_ref x world table: the lane rule is
// decided by the world a dispatch RESOLVED to, so it can be pinned without a
// repository, a runner or a world. The shared case is here deliberately and
// asserts NO refusal: a shared dispatch has no world of its own to seed, so that
// refusal belongs to api.go (which knows the lane by name and runs before any
// world exists), not to this predicate.
func TestBaseRefWorldRefusal(t *testing.T) {
	cases := []struct {
		name     string
		baseRef  string
		resolved subagent.WorldKind
		want     string
	}{
		{name: "absent on the worktree", resolved: subagent.WorldWorktree},
		{name: "absent on a container", resolved: subagent.WorldContainer},
		{name: "blank on a container is absent", baseRef: "   ", resolved: subagent.WorldContainer},
		{name: "a branch on the worktree", baseRef: "side", resolved: subagent.WorldWorktree},
		{name: "a commit on the worktree", baseRef: "0c54a79", resolved: subagent.WorldWorktree},
		{name: "a branch on the shared lane is api.go's refusal", baseRef: "side", resolved: subagent.WorldShared},
		{name: "a branch on a container", baseRef: "side", resolved: subagent.WorldContainer, want: "worktree-lane only"},
		{name: "a tag on a container", baseRef: "v1.2.3", resolved: subagent.WorldContainer, want: "worktree-lane only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := baseRefWorldRefusal(tc.baseRef, tc.resolved)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("baseRefWorldRefusal(%q, %q) = %q, want no refusal", tc.baseRef, tc.resolved, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("baseRefWorldRefusal(%q, %q) = %q, want it to contain %q", tc.baseRef, tc.resolved, got, tc.want)
			}
		})
	}
}

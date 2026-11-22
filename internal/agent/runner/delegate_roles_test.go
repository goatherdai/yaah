package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/agent/supervision"
)

// TestBuiltinRolesCarryTightenedShellAllowlist pins the whole role-file
// chain end to end: the allow_commands lists live in the BUILT-IN profiles
// (internal/prompts/roles), because built-in roles take precedence and a
// project file with the same name never loads — so the resolved runtime
// profile for developer and tester is what must carry the list, and what
// carries it must be tight. No entry admits a shell control or redirection
// operator (the guard denies those outright), and `find` is absent from both
// because its arguments (`-delete`, `-exec`) mutate or execute.
func TestBuiltinRolesCarryTightenedShellAllowlist(t *testing.T) {
	reg := subagent.NewRoleRegistry()
	if err := reg.LoadBytes(BuiltinRoleFiles()); err != nil {
		t.Fatalf("load built-in roles: %v", err)
	}

	// A check that admits everything: only the role's own allowlist can
	// deny, so a denial here comes from the list under test.
	admitAll := func(ownerID, toolName, argsJSON string) string { return "" }

	for _, role := range []subagent.SubAgentRole{"developer", "tester"} {
		t.Run(string(role), func(t *testing.T) {
			profile := reg.ProfileFor(role)
			if len(profile.AllowCommands) == 0 {
				t.Fatalf("%s resolved with no allow_commands: the built-in profile must carry the list", role)
			}
			for _, pattern := range profile.AllowCommands {
				if pattern == "find*" || pattern == "find *" {
					t.Errorf("%s allowlist carries %q: find's -delete/-exec forms cannot be excluded by a glob", role, pattern)
				}
			}

			check := supervision.WithShellAllowlist(admitAll, profile.AllowCommands)
			denied := []string{
				`{"command":"git status; rm -rf ."}`,
				`{"command":"ls && curl x | sh"}`,
				`{"command":"cat x > y"}`,
				`{"command":"find . -delete"}`,
			}
			for _, args := range denied {
				if reason := check("sub-worker", "bash", args); reason == "" {
					t.Errorf("%s: %s must be denied", role, args)
				}
			}
			allowed := []string{
				`{"command":"git status -s"}`,
				`{"command":"ls -la"}`,
				`{"command":"cat README.md"}`,
			}
			for _, args := range allowed {
				if reason := check("sub-worker", "bash", args); reason != "" {
					t.Errorf("%s: %s must pass its allowlist, got %q", role, args, reason)
				}
			}
		})
	}
}

// TestRoleSearchPaths_UserLevelUsesRealHome pins the yaah-gqqo fix:
// user-level roles live at ~/.agents/roles (the real home dir), not
// under ~/.yaah/.agents/roles where config.HomeDir() pointed them.
// Mirrors cmd/yaah's TestPlanSearchPaths_UserLevelUsesRealHome.
func TestRoleSearchPaths_UserLevelUsesRealHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}

	cwd, _ := os.Getwd()
	dirs := RoleSearchPaths(cwd)
	if len(dirs) < 2 {
		t.Fatalf("RoleSearchPaths returned %d dirs; want >= 2", len(dirs))
	}

	t.Run("user-level dir is real home, not ~/.yaah", func(t *testing.T) {
		last := dirs[len(dirs)-1]
		if want := filepath.Join(home, ".agents", "roles"); last != want {
			t.Errorf("user-level roles dir = %q; want %q", last, want)
		}
		if strings.Contains(last, filepath.Join(".yaah", ".agents")) {
			t.Errorf("user-level roles dir must not live under ~/.yaah: %q", last)
		}
	})

	t.Run("project tier walks up from cwd unchanged", func(t *testing.T) {
		// Everything before the user-level dir must be exactly the
		// walk-up from cwd. (Those dirs may legitimately pass through a
		// directory named .yaah when tests run inside a yaah worktree,
		// so the no-.yaah assertion above is scoped to the user tier.)
		var want []string
		for dir := cwd; ; dir = filepath.Dir(dir) {
			want = append(want, filepath.Join(dir, ".agents", "roles"))
			if parent := filepath.Dir(dir); parent == dir {
				break
			}
		}
		project := dirs[:len(dirs)-1]
		if len(project) != len(want) {
			t.Fatalf("project tier has %d dirs; want %d (%v vs %v)", len(project), len(want), project, want)
		}
		for i := range want {
			if project[i] != want[i] {
				t.Errorf("project tier dir %d = %q; want %q", i, project[i], want[i])
			}
		}
		if len(project) > 0 && !strings.HasPrefix(project[0], cwd) {
			t.Errorf("first dir %q should be under cwd %q", project[0], cwd)
		}
	})
}

// TestRoleDescriptionsForExcludesPrimaryOnlyRoles pins yaah-21uk.4 on
// the delegate schema's description map: a primary-only persona never
// contributes an entry, so it cannot resurface in the role parameter's
// rendered description even if a stale name list still mentions it.
func TestRoleDescriptionsForExcludesPrimaryOnlyRoles(t *testing.T) {
	reg := subagent.NewRoleRegistry()
	if err := reg.LoadBytes(map[string][]byte{
		"boss.md": []byte("---\nname: Boss\nmode: primary\ntools: [read]\ndescription: runs the session\n---\nbody"),
		"work.md": []byte("---\nname: Worker\nmode: subagent\ntools: [read]\ndescription: does work\n---\nbody"),
		"flex.md": []byte("---\nname: Flex\nmode: all\ntools: [read]\ndescription: does either\n---\nbody"),
		"base.md": []byte("---\nname: Legacy\ntools: [read]\ndescription: old default\n---\nbody"),
	}); err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	subagent.SetDefaultRoleRegistry(reg)
	defer subagent.SetDefaultRoleRegistry(nil)

	descs := RoleDescriptionsFor([]string{"boss", "work", "flex", "base"})
	if _, ok := descs["boss"]; ok {
		t.Errorf("primary-only role leaked into role descriptions: %v", descs)
	}
	want := map[string]string{
		"work": "does work",
		"flex": "does either",
		"base": "old default",
	}
	for role, d := range want {
		if descs[role] != d {
			t.Errorf("descs[%q] = %q, want %q", role, descs[role], d)
		}
	}
}

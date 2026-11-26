package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/goatherdai/goatherd-runtime/sandbox/containment"
	"github.com/goatherdai/goatherd-runtime/sandbox/landlock"
)

// TestConfinementScratchEnv_Redirects: the redirect points the housekeeping keys
// inside the worktree and creates the directories.
func TestConfinementScratchEnv_Redirects(t *testing.T) {
	root := t.TempDir()
	env, err := ConfinementScratchEnv(root)
	if err != nil {
		t.Fatalf("scratch env: %v", err)
	}
	want := map[string]string{
		"HOME":             filepath.Join(root, confinementScratchName, "home"),
		"TMPDIR":           filepath.Join(root, confinementScratchName, "tmp"),
		"XDG_CACHE_HOME":   filepath.Join(root, confinementScratchName, "cache"),
		"GOCACHE":          filepath.Join(root, confinementScratchName, "go-build"),
		"npm_config_cache": filepath.Join(root, confinementScratchName, "npm"),
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if w, ok := want[k]; ok {
			if v != w {
				t.Errorf("%s = %q, want %q", k, v, w)
			}
			fi, err := os.Stat(v)
			if err != nil || !fi.IsDir() {
				t.Errorf("%s dir %q not created: %v", k, v, err)
			} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
				t.Errorf("%s dir perm = %o, want 700 (HOME/TMPDIR may hold credentials)", k, fi.Mode().Perm())
			}
			delete(want, k)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing redirect keys: %v", want)
	}
}

// TestDefaultGoModCache pins the pre-redirect resolution: the HOME redirect
// would otherwise relocate Go's module cache into the scratch.
func TestDefaultGoModCache(t *testing.T) {
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", "/real/home")
	if got, want := defaultGoModCache(), filepath.Join("/real/home", "go", "pkg", "mod"); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
	t.Setenv("GOPATH", "/go")
	if got, want := defaultGoModCache(), filepath.Join("/go", "pkg", "mod"); got != want {
		t.Errorf("with GOPATH = %q, want %q", got, want)
	}
}

// TestConfinementScratchEnv_PointsGitAtGlobalConfig: the redirect moves HOME, so
// git would look for the user's identity in an empty scratch and refuse to commit
// ("Author identity unknown"). The redirect points git at a SHIM in the scratch
// that includes the real global config files — not a copy, so a config carrying
// `http.<url>.extraheader` tokens never lands in the worktree, and `include`
// paths keep resolving against the files' real locations. A shim rather than a
// direct pointer, because GIT_CONFIG_GLOBAL replaces BOTH default files and
// takes one path: naming a single file would drop the other.
func TestConfinementScratchEnv_PointsGitAtGlobalConfig(t *testing.T) {
	// No inherited pointer: the resolution under test is the HOME one.
	if prev, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
		_ = os.Unsetenv("GIT_CONFIG_GLOBAL")
		t.Cleanup(func() { _ = os.Setenv("GIT_CONFIG_GLOBAL", prev) })
	}
	// gitGlobal runs the redirect and returns the pointer plus the shim's body.
	gitGlobal := func(t *testing.T, root string) (string, string, bool) {
		t.Helper()
		env, err := ConfinementScratchEnv(root)
		if err != nil {
			t.Fatalf("scratch env: %v", err)
		}
		for _, kv := range env {
			if k, v, _ := strings.Cut(kv, "="); k == "GIT_CONFIG_GLOBAL" {
				body, rerr := os.ReadFile(v)
				if rerr != nil {
					t.Fatalf("the pointer must name a readable shim: %v", rerr)
				}
				return v, string(body), true
			}
		}
		return "", "", false
	}

	t.Run("both files are included, in git's precedence order", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		xdg := filepath.Join(home, ".config", "git", "config")
		legacy := filepath.Join(home, ".gitconfig")
		if err := os.MkdirAll(filepath.Dir(xdg), 0o755); err != nil {
			t.Fatalf("seed xdg dir: %v", err)
		}
		if err := os.WriteFile(xdg, []byte("[user]\n\tname = XDG\n"), 0o644); err != nil {
			t.Fatalf("seed xdg config: %v", err)
		}
		if err := os.WriteFile(legacy, []byte("[user]\n\temail = legacy@example.com\n"), 0o644); err != nil {
			t.Fatalf("seed .gitconfig: %v", err)
		}
		root := t.TempDir()
		pointer, body, ok := gitGlobal(t, root)
		if !ok {
			t.Fatal("GIT_CONFIG_GLOBAL must be set when a global config exists")
		}
		if want := filepath.Join(root, confinementScratchName, "home", ".gitconfig"); pointer != want {
			t.Errorf("GIT_CONFIG_GLOBAL = %q, want the shim %q", pointer, want)
		}
		// Both, in order. Including only one would DROP the other: the pointer
		// replaces both defaults rather than adding to them, and a name in one
		// file plus an email in the other is a real configuration.
		if !strings.Contains(body, "path = "+gitConfigValue(xdg)) || !strings.Contains(body, "path = "+gitConfigValue(legacy)) {
			t.Errorf("the shim must include both files:\n%s", body)
		}
		if strings.Index(body, xdg) > strings.Index(body, legacy) {
			t.Errorf("the shim must include the XDG file first (git's order):\n%s", body)
		}
		// Paths, not values: nothing from either config is copied.
		if strings.Contains(body, "XDG") || strings.Contains(body, "legacy@example.com") {
			t.Errorf("the shim must carry paths, not config content:\n%s", body)
		}
	})

	t.Run("a pre-set pointer is left alone", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n\tname = Legacy\n"), 0o644); err != nil {
			t.Fatalf("seed .gitconfig: %v", err)
		}
		t.Setenv("GIT_CONFIG_GLOBAL", "/somewhere/else")
		if pointer, _, ok := gitGlobal(t, t.TempDir()); ok {
			t.Errorf("an inherited pointer must be left alone, got %q", pointer)
		}
	})

	t.Run("XDG_CONFIG_HOME is honored", func(t *testing.T) {
		home := t.TempDir()
		xdg := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
			t.Fatalf("seed xdg dir: %v", err)
		}
		want := filepath.Join(xdg, "git", "config")
		if err := os.WriteFile(want, []byte("[user]\n\tname = XDG\n"), 0o644); err != nil {
			t.Fatalf("seed xdg config: %v", err)
		}
		_, body, ok := gitGlobal(t, t.TempDir())
		if !ok {
			t.Fatal("GIT_CONFIG_GLOBAL must be set when a global config exists")
		}
		if !strings.Contains(body, "path = "+gitConfigValue(want)) {
			t.Errorf("the shim must include the XDG config %q:\n%s", want, body)
		}
	})

	t.Run("no global config adds no pointer", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", "")
		if pointer, _, ok := gitGlobal(t, t.TempDir()); ok {
			t.Errorf("no source config should mean no pointer, got %q", pointer)
		}
	})
}

// TestConfinementScratchEnv_RefusesSymlinkedScratch: the scratch is created by
// the unconfined parent, and MkdirAll follows symlinks — so a `.yaah-scratch`
// planted as a symlink out of the worktree (a checked-in one, or one an earlier
// turn left) must be refused rather than followed, or the redirect would point
// the body outside the jail's writable root.
func TestConfinementScratchEnv_RefusesSymlinkedScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	outside := t.TempDir()
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, confinementScratchName)); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	if _, err := ConfinementScratchEnv(root); err == nil {
		t.Fatal("a symlinked scratch must be refused, not followed")
	}
	// Nothing may have been created through it.
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("the symlink target must be untouched: entries=%v err=%v", entries, err)
	}

	// A final-component symlink is refused too (not just a parent).
	root2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root2, confinementScratchName), 0o700); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root2, confinementScratchName, "home")); err != nil {
		t.Fatalf("plant the inner symlink: %v", err)
	}
	if _, err := ConfinementScratchEnv(root2); err == nil {
		t.Fatal("a symlinked scratch HOME must be refused, not followed")
	}
}

// TestGitConfigValue_QuotesAndEscapes: an unquoted interpolation would be
// truncated at a comment character and reinterpreted at a backslash, so a HOME
// containing either would silently corrupt the shim.
func TestGitConfigValue_QuotesAndEscapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/plain/.gitconfig", `"/home/plain/.gitconfig"`},
		{"/home/has space/.gitconfig", `"/home/has space/.gitconfig"`},
		{"/home/hash # not a comment/.gitconfig", `"/home/hash # not a comment/.gitconfig"`},
		{"/home/semi ; not a comment/.gitconfig", `"/home/semi ; not a comment/.gitconfig"`},
		{`/home/back\slash/.gitconfig`, `"/home/back\\slash/.gitconfig"`},
		{"/home/quote\"/.gitconfig", `"/home/quote\"/.gitconfig"`},
		{"/home/new\nline", `"/home/new\nline"`},
		// A path is a byte string and may not be valid UTF-8; every byte must survive.
		{"/home/\xff\xfe/.gitconfig", "\"/home/\xff\xfe/.gitconfig\""},
	}
	for _, c := range cases {
		if got := gitConfigValue(c.in); got != c.want {
			t.Errorf("gitConfigValue(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// hintWorkspace is the confined workspace the exec-time hint answers for: the
// hint asks it where the boundary is and whether a git write can land there at
// all, so the double carries a root and the confinement signal and nothing else.
// Taking a bare path rather than a jail also lets the Windows phrasings be pinned
// from any host.
type hintWorkspace struct {
	Workspace
	root string
}

func (w hintWorkspace) WorkDir() string { return w.root }
func (w hintWorkspace) Confined() bool  { return true }

// newHintWorkspace returns the workspace a hint assertion is made against. root
// is the fixture's own spelling — not necessarily an existing directory, for the
// Windows cases — and the hint must name it verbatim.
func newHintWorkspace(root string) hintWorkspace {
	pv := NewPathValidator(root, false, nil)
	pv.WorkDir = pv.WorkspaceRoot
	return hintWorkspace{Workspace: newLocalWorkspace(pv), root: root}
}

// TestConfinementHint_ExplainsRefusedWrites: a kernel refusal reaches the model as
// a bare "Permission denied" unless it is explained, and an unexplained refusal
// is what makes a jail feel broken. The hint names the writable root, and names
// the git rule when the refusal is git's own metadata.
func TestConfinementHint_ExplainsRefusedWrites(t *testing.T) {
	// Host-native absolute paths: the hint is only reachable where a kernel jail
	// runs (Linux via Landlock, macOS via Seatbelt, Windows via winacl), but the
	// function is
	// cross-platform code and must not be tested with one platform's path shapes.
	root := t.TempDir()
	outside := t.TempDir()
	ws := newHintWorkspace(root)

	gitCase := fmt.Sprintf("fatal: Unable to create '%s': Permission denied",
		filepath.Join(outside, ".git", "worktrees", "wt", "index.lock"))
	got := confinementHint(ws, gitCase, 0)
	if !strings.Contains(got, root) || !strings.Contains(got, "orchestrator commits") {
		t.Errorf("a git-metadata refusal must name the root and the commit rule, got:\n%s", got)
	}

	plain := fmt.Sprintf("sh: 1: cannot create %s: Permission denied", filepath.Join(outside, "motd"))
	if got := confinementHint(ws, plain, 0); !strings.Contains(got, root) || strings.Contains(got, "orchestrator commits") {
		t.Errorf("a non-git refusal outside the root must name the root without the git rule, got:\n%s", got)
	}

	// An apostrophe before the quoted path must not swallow the path.
	apos := fmt.Sprintf("error: can't open '%s': Permission denied", filepath.Join(outside, "x"))
	if got := confinementHint(ws, apos, 0); got == "" {
		t.Error("an unmatched apostrophe must not drop the refusal")
	}

	// A sibling is not "inside": <root>-other must not pass as the root.
	sibling := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-other", "x")
	if got := confinementHint(ws, "sh: 1: cannot create "+sibling+": Permission denied", 0); got == "" {
		t.Error("a sibling of the root is outside it and must be explained")
	}

	// The false positives the reviewer named: a permission failure INSIDE the
	// worktree is not a boundary refusal, and captioning it as one misdirects.
	for _, s := range []string{
		"", // no failure at all
		"compile error: undefined: foo",
		"ls: cannot access '/nope': No such file or directory",
		// chmod 000 file, in-worktree
		"cat: " + filepath.Join(root, "secret") + ": Permission denied",
		// non-executable script, in-worktree
		"sh: 1: " + filepath.Join(root, "script.sh") + ": Permission denied",
		// read-only file, in-worktree
		"tee: " + filepath.Join(root, "ro.txt") + ": Read-only file system",
		// a refusal the message does not attribute to a path
		"git: Permission denied",
	} {
		if got := confinementHint(ws, s, 0); got != "" {
			t.Errorf("confinementHint(%q) = %q, want no hint (inside the root, or unattributable)", s, got)
		}
	}

	// Windows phrasings, matched on every host: the matching is pure string
	// work, so the Windows producers a winacl body emits are pinned here rather
	// than left to a Windows-only leg. Grow and verify a drive-letter root the
	// way a Windows workspace would spell it.
	wnRoot := `C:\work\wt`
	wns := newHintWorkspace(wnRoot)
	for _, s := range []string{
		// Go PathError form: path inline, lowercased drive (an MSYS-spelled
		// tool run under the same body).
		`open c:\Users\someone\out.txt: Access is denied.`,
		// .NET/PowerShell form: "Access to the path ... is denied."
		`Access to the path 'D:\temp\scratch' is denied.`,
	} {
		if got := confinementHint(wns, s, 0); got == "" {
			t.Errorf("confinementHint(%q) = %q, want a hint for a denial outside the root", s, got)
		}
	}
	// In-worktree refusals in the same phrasings must not be captioned as
	// boundary refusals — case folding included, since producers capitalize
	// inconsistently (root passed as `c:\work\wt`, message as `C:\Work\Wt`).
	for _, s := range []string{
		`open C:\work\wt\file.txt: Access is denied.`,
		`Access to the path 'C:\Work\Wt\sub\log.txt' is denied.`,
	} {
		if got := confinementHint(wns, s, 0); got != "" {
			t.Errorf("confinementHint(%q) = %q, want no hint for a Windows phrasing inside the root", s, got)
		}
	}

	// No workspace, so no boundary to name (an advisory workspace runs no jail) —
	// no hint.
	if got := confinementHint(nil, gitCase, 0); got != "" {
		t.Errorf("a nil workspace must produce no hint, got %q", got)
	}
}

// TestConfinementHint_TranslatesSnapConfine: a command that resolves to a snap
// cannot start inside the jail, and the raw snap-confine message names neither
// the packaging nor the remedy. The hint must say both, without waiting for a
// path-attributed permission failure (there is none).
func TestConfinementHint_TranslatesSnapConfine(t *testing.T) {
	ws := newHintWorkspace(t.TempDir())
	abort := "snap-confine: required permitted capability cap_dac_override not found in current capabilities"

	got := confinementHint(ws, abort, 1)
	for _, want := range []string{"snap package", "/snap/bin", "tarball or distro build"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint must mention %q, got:\n%s", want, got)
		}
	}

	// The phrasings snap-confine uses when it did not run, each on the aborting
	// line. They are snap-specific by construction (see snapFailurePhrases): the
	// generic words that used to be accepted — "failed to", "Permission denied" —
	// appear in ordinary healthy output, this repository's own source included.
	for _, s := range []string{
		"snap-confine: cannot start: no_new_privs refused",
		"snap-confine: can not start",
		"snap-confine: cannot run: no_new_privs",
		"snap-confine: cannot open /var/lib/snapd/state.json",
		"snap-confine: Refusing to continue",
	} {
		// Exit 0 too: a composite can exit 0 with the abort in its transcript,
		// which is why the exit code cannot be the only gate.
		for _, code := range []int{0, 1} {
			if got := confinementHint(ws, s, code); got == "" {
				t.Errorf("snap-confine failing to start must still be explained (exit %d): %q", code, s)
			}
		}
	}

	// Generic capability text from something that is not snap-confine must not
	// be captioned as a toolchain packaging problem.
	if got := confinementHint(ws, "apparmor: required capability denied", 1); got != "" {
		t.Errorf("a generic capability failure produced a snap hint: %q", got)
	}

	// snap-confine's capability abort without the executable's name in the
	// message is still snap-confine: this exact wording is its own, and the abort
	// is often relayed without the token. It counts only for a command that
	// FAILED, because the same line is a fixture in this repository's own tests
	// (see TestConfinementHint_DoesNotCaptionTheRepositorysOwnText): quoted source
	// text is not an abort.
	bare := "required permitted capability cap_dac_override not found in current capabilities"
	if got := confinementHint(ws, bare, 1); got == "" {
		t.Error("snap-confine's capability abort must be explained even without the token")
	}
	if got := confinementHint(ws, bare, 0); got != "" {
		t.Errorf("a successful command quoting the capability abort is not an abort, got: %q", got)
	}

	// The bare token is not evidence of a failure, and neither is a failure
	// phrasing printed on a different line: the pairing is line-scoped, so a
	// transcript that merely lists the token cannot borrow a phrase from
	// elsewhere in the output (bead yaah-pvm8 review, R1).
	for _, s := range []string{
		"internal/prompts/confinement.md: snap-confine is setuid and needs capabilities",
		"snap-confine is setuid\ncannot start without privileges\n",
	} {
		if got := confinementHint(ws, s, 0); got != "" {
			t.Errorf("a mere mention of the token is not a refusal: %q -> %q", s, got)
		}
	}
}

// TestConfinementHint_DoesNotCaptionTheRepositorysOwnText pins the reviewer's two
// collisions (yaah-pvm8 review, R1) against the ACTUAL lines of this repository,
// read out of the files rather than retyped: a sub-agent grepping this tree is an
// ordinary action, and its output reaches the hint at exit 0.
//
// The previous negative case hand-crafted a line that happened to avoid the extra
// phrases, which is how a fix round could look green while `grep -rn snap-confine
// internal/` was still captioned as a sandbox refusal.
func TestConfinementHint_DoesNotCaptionTheRepositorysOwnText(t *testing.T) {
	ws := newHintWorkspace(t.TempDir())

	fixtures := []struct {
		path     string
		contains [2]string
	}{
		// The packaging rationale in the workspace source, which states the token
		// and the capability detail on one line.
		{
			path:     filepath.Join("substrate", "workspace.go"),
			contains: [2]string{"snap-confine", "no_new_privs"},
		},
		// This file's own capability fixture: snap-confine's abort, verbatim. Kept
		// on separate lines here so the search strings are not themselves a hit.
		{
			path: "confinement_test.go",
			contains: [2]string{
				"required permitted capability",
				"not found in current capabilities",
			},
		},
	}
	for _, f := range fixtures {
		lines := repoLinesWith(t, f.path, f.contains[:]...)
		if len(lines) == 0 {
			t.Fatalf("%s no longer carries %q and %q on one line; update this fixture",
				f.path, f.contains[0], f.contains[1])
		}
		for _, line := range lines {
			// grep's shape: path:line:text, and grep SUCCEEDS when it matched.
			output := fmt.Sprintf("%s:%d:%s\n", f.path, line.number, line.text)
			if got := confinementHint(ws, output, 0); got != "" {
				t.Errorf("a successful grep over %s:%d was captioned as a sandbox refusal:\n  line: %s\n  hint: %s",
					f.path, line.number, line.text, got)
			}
		}
	}
}

// sourceLine is one line of a fixture file, with its 1-based number.
type sourceLine struct {
	number int
	text   string
}

// repoLinesWith returns every line of path that contains all of the given
// substrings. Reading the files keeps these fixtures honest: the collision is
// whatever this repository actually says, not a handcrafted near-miss.
func repoLinesWith(t *testing.T, path string, substrings ...string) []sourceLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the fixture file %s: %v", path, err)
	}
	var out []sourceLine
	for i, line := range strings.Split(string(data), "\n") {
		hasAll := true
		for _, s := range substrings {
			if !strings.Contains(line, s) {
				hasAll = false
				break
			}
		}
		if hasAll {
			out = append(out, sourceLine{number: i + 1, text: line})
		}
	}
	return out
}

// TestScrubConfinementScratch_Removes: the scrub removes the whole scratch tree.
func TestScrubConfinementScratch_Removes(t *testing.T) {
	root := t.TempDir()
	if _, err := ConfinementScratchEnv(root); err != nil {
		t.Fatalf("scratch env: %v", err)
	}
	ScrubConfinementScratch(root)
	if _, err := os.Stat(filepath.Join(root, confinementScratchName)); !os.IsNotExist(err) {
		t.Fatalf("scrub should remove the scratch (stat err=%v)", err)
	}
}

// TestMergeEnv_OverridesWithoutDuplicates: overrides win by key and never
// duplicate it.
func TestMergeEnv_OverridesWithoutDuplicates(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/real/home", "LANG=C"}
	got := mergeEnv(base, []string{"HOME=/work/home", "TMPDIR=/work/tmp"})

	n := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "HOME=") {
			n++
			if kv != "HOME=/work/home" {
				t.Errorf("HOME = %q, want the override", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("HOME appears %d times, want 1 (duplicate keys resolve unpredictably)", n)
	}
	if !slices.Contains(got, "TMPDIR=/work/tmp") {
		t.Error("the new TMPDIR override is missing")
	}
	if !slices.Contains(got, "PATH=/bin") {
		t.Error("an unrelated base key must survive")
	}
}

// TestConfinedWorkspace_EnvRedirectLandsInWorktree: with the redirect, a shell
// writing $HOME and $TMPDIR lands inside the worktree; without it, the same
// write is denied (HOME is outside the jail). Linux only.
func TestConfinedWorkspace_EnvRedirectLandsInWorktree(t *testing.T) {
	backend := landlock.Detect()
	if backend == nil {
		t.Skip("no Landlock on this host")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell redirection")
	}
	root := t.TempDir()
	env, err := ConfinementScratchEnv(root)
	if err != nil {
		t.Fatalf("scratch env: %v", err)
	}
	ws := confinedTestWorkspace(root, backend, containment.PlacementPaddock).WithEnv(env)

	res, err := ws.Exec(context.Background(), ExecRequest{
		Command: "sh",
		Args:    []string{"-c", `echo h > "$HOME/home.txt" && echo t > "$TMPDIR/tmp.txt" && echo DONE`},
	})
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "DONE") {
		t.Fatalf("redirected $HOME/$TMPDIR writes should land: rc=%d out=%q err=%q runErr=%v",
			res.ExitCode, res.Stdout, res.Stderr, err)
	}
	if _, err := os.Stat(filepath.Join(root, confinementScratchName, "home", "home.txt")); err != nil {
		t.Errorf("$HOME write should land in the worktree scratch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, confinementScratchName, "tmp", "tmp.txt")); err != nil {
		t.Errorf("$TMPDIR write should land in the worktree scratch: %v", err)
	}

	// The redirect is load-bearing: without it, HOME is outside the jail.
	plain := confinedTestWorkspace(root, backend, containment.PlacementPaddock)
	if res2, _ := plain.Exec(context.Background(), ExecRequest{
		Command: "sh",
		Args:    []string{"-c", `echo x > "$HOME/yaah-should-be-denied"`},
	}); res2.ExitCode == 0 {
		t.Error("without the redirect, a $HOME write outside the worktree must be denied")
	}
}

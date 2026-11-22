package discover

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSearchDirsOrder(t *testing.T) {
	t.Setenv("YAAH_HOME", "/home/y")
	got := SearchDirs("/ws", "/extra1", "/extra2")
	want := []string{"/extra1", "/extra2", "/ws/workflows", "/home/y/workflows"}
	if len(got) != len(want) {
		t.Fatalf("SearchDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SearchDirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	cwd := t.TempDir()

	// Workspace dir: foo.yaml wins any collision; nested dir and non-spec
	// files are skipped.
	write(t, filepath.Join(cwd, "workflows", "foo.yaml"), "kind: Workflow\n")
	write(t, filepath.Join(cwd, "workflows", "notes.txt"), "not a spec\n")
	if err := os.MkdirAll(filepath.Join(cwd, "workflows", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Global dir: bar.yml and a colliding foo.yaml (cwd must win).
	write(t, filepath.Join(home, "workflows", "bar.yml"), "kind: Workflow\n")
	write(t, filepath.Join(home, "workflows", "foo.yaml"), "kind: Workflow\n")

	got, err := List(cwd)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d specs, want 2: %+v", len(got), got)
	}
	if got[0].Name != "bar" || got[1].Name != "foo" {
		t.Fatalf("names = %q, %q; want sorted [bar foo]", got[0].Name, got[1].Name)
	}
	if got[1].Path != filepath.Join(cwd, "workflows", "foo.yaml") {
		t.Errorf("foo resolved to %q, want the workspace copy (first dir wins)", got[1].Path)
	}
	if got[0].Path != filepath.Join(home, "workflows", "bar.yml") {
		t.Errorf("bar resolved to %q, want the global copy", got[0].Path)
	}
}

func TestListSkipsMissingDirs(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	cwd := t.TempDir() // no workflows/ dir anywhere
	got, err := List(cwd)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List returned %+v, want none", got)
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "workflows", "foo.yaml"), "kind: Workflow\n")
	write(t, filepath.Join(home, "workflows", "bar.yaml"), "kind: Workflow\n")
	write(t, filepath.Join(home, "workflows", "foo.yaml"), "kind: Workflow\n")

	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"bare name workspace wins", "foo", filepath.Join(cwd, "workflows", "foo.yaml")},
		{"bare name global", "bar", filepath.Join(home, "workflows", "bar.yaml")},
		{"relative path with ext", "sub/foo.yaml", filepath.Join(cwd, "sub", "foo.yaml")},
		{"absolute path", "/abs/foo.yaml", "/abs/foo.yaml"},
		{"relative path with separator", "sub/foo", filepath.Join(cwd, "sub", "foo")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(cwd, tc.ref)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.ref, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.ref, got, tc.want)
			}
		})
	}

	t.Run("not found", func(t *testing.T) {
		_, err := Resolve(cwd, "nope")
		if err == nil {
			t.Fatal("Resolve(nope) succeeded, want error")
		}
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("Resolve(nope) error = %T, want *NotFoundError", err)
		}
		if !strings.Contains(err.Error(), "searched") {
			t.Errorf("error does not list search dirs: %v", err)
		}
	})
}

// TestListMatchesResolveExtensionOrder pins the parity between listing and
// running: when a directory holds both foo.json and foo.yaml, List must show
// the same file Resolve runs (extension order .yaml > .yml > .json), not the
// lexically first entry.
func TestListMatchesResolveExtensionOrder(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	cwd := t.TempDir()
	write(t, filepath.Join(cwd, "workflows", "foo.json"), "kind: Workflow\n")
	write(t, filepath.Join(cwd, "workflows", "foo.yaml"), "kind: Workflow\n")

	got, err := List(cwd)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "foo" {
		t.Fatalf("List = %+v, want one foo", got)
	}
	if got[0].Path != filepath.Join(cwd, "workflows", "foo.yaml") {
		t.Errorf("List path = %q, want foo.yaml (Resolve's pick)", got[0].Path)
	}
	resolved, err := Resolve(cwd, "foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got[0].Path != resolved {
		t.Errorf("List path %q != Resolve path %q", got[0].Path, resolved)
	}
}

// TestListSkipsSymlinkToDir pins the symlink parity between listing and
// running: an extension-bearing symlink whose target is a directory is not a
// runnable spec, and both List and Resolve must reject it.
func TestListSkipsSymlinkToDir(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "workflows", "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cwd, "workflows", "target"), filepath.Join(cwd, "workflows", "foo.yaml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := List(cwd)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List returned %+v, want none (symlink to a directory)", got)
	}
	if _, err := Resolve(cwd, "foo"); err == nil {
		t.Fatal("Resolve(foo) succeeded for a symlink-to-directory, want not-found")
	}
}

// TestListAbsolutesRelativeExtraDir pins the relative --workflow-dir fix:
// a caller-supplied relative directory must be made absolute so listed specs
// carry paths that open from any cwd.
func TestListAbsolutesRelativeExtraDir(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	base := t.TempDir()
	write(t, filepath.Join(base, "workflow-specs", "rel.yaml"), "kind: Workflow\n")

	// A relative extra dir resolves against the process cwd; chdir there so
	// "workflow-specs" is unambiguous.
	t.Chdir(base)
	got, err := List(base, "workflow-specs")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "rel" {
		t.Fatalf("List = %+v, want one rel", got)
	}
	if want := filepath.Join(base, "workflow-specs", "rel.yaml"); got[0].Path != want {
		t.Errorf("listed path = %q, want %q", got[0].Path, want)
	}
	if !filepath.IsAbs(got[0].Path) {
		t.Errorf("listed path not absolute: %q", got[0].Path)
	}
}

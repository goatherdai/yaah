package yaah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the workflow default-location feature: `yaah workflow list` and
// bare-name resolution in `yaah workflow run`.

const listSpec = "apiVersion: yaah.workflow/v1\nkind: Workflow\nmetadata:\n  name: %s\nspec:\n  steps:\n    - id: a\n      kind: delegate\n      role: developer\n      prompt: do a\n"

func writeListSpec(t *testing.T, path, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(listSpec, name)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runListCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newWorkflowListCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	if err := cmd.ParseFlags(args); err != nil {
		return buf.String(), err
	}
	positional := cmd.Flags().Args()
	if err := cmd.Args(cmd, positional); err != nil {
		return buf.String(), err
	}
	err := cmd.RunE(cmd, positional)
	return buf.String(), err
}

func TestWorkflowList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)

	writeListSpec(t, filepath.Join(cwd, "workflows", "foo.yaml"), "foo")
	writeListSpec(t, filepath.Join(home, "workflows", "bar.yaml"), "bar")
	writeListSpec(t, filepath.Join(home, "workflows", "foo.yaml"), "foo-dup") // shadowed

	out, err := runListCmd(t)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, name := range []string{"bar", "foo"} {
		if !strings.Contains(out, name) {
			t.Fatalf("list output missing %q:\n%s", name, out)
		}
	}
	if strings.Contains(out, "foo-dup") {
		t.Fatalf("list output leaked the shadowed global copy:\n%s", out)
	}
	// Sorted name-first, and the workspace copy wins the foo collision.
	if !strings.Contains(out, filepath.Join(cwd, "workflows", "foo.yaml")) {
		t.Errorf("foo resolved to the wrong copy:\n%s", out)
	}
	barIdx := strings.Index(out, "bar")
	fooIdx := strings.Index(out, "foo")
	if barIdx < 0 || fooIdx < 0 || barIdx > fooIdx {
		t.Fatalf("list not sorted (bar before foo):\n%s", out)
	}
}

func TestWorkflowListJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	writeListSpec(t, filepath.Join(cwd, "workflows", "foo.yaml"), "foo")

	out, err := runListCmd(t, "--json")
	if err != nil {
		t.Fatalf("list --json: %v\n%s", err, out)
	}
	var specs []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Dir  string `json:"dir"`
	}
	if err := json.Unmarshal([]byte(out), &specs); err != nil {
		t.Fatalf("list --json output is not a JSON array: %v\n%s", err, out)
	}
	if len(specs) != 1 || specs[0].Name != "foo" {
		t.Fatalf("list --json = %+v, want one foo spec", specs)
	}
	if specs[0].Path != filepath.Join(cwd, "workflows", "foo.yaml") {
		t.Errorf("list --json path = %q, want the workspace copy", specs[0].Path)
	}
}

func TestWorkflowRunResolvesBareName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	writeListSpec(t, filepath.Join(cwd, "workflows", "kickoff.yaml"), "kickoff")

	var calls int
	swapRunRunner(t, echoRunner("done "), &calls)

	out, err := runWfRun(t, "kickoff")
	if err != nil {
		t.Fatalf("run kickoff: %v\n%s", err, out)
	}
	if !strings.Contains(out, "status") {
		t.Errorf("report missing status:\n%s", out)
	}
	if calls == 0 {
		t.Error("no dispatch happened (echo runner never called)")
	}
}

func TestWorkflowRunBareNameNotFound(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	_, err := runWfRun(t, "nope")
	if err == nil {
		t.Fatal("run nope succeeded, want a not-found error")
	}
	if !strings.Contains(err.Error(), "searched") {
		t.Errorf("error does not list the search dirs: %v", err)
	}
}

func TestWorkflowRunResolvesFromWorkflowDirFlag(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	extra := t.TempDir()
	t.Chdir(t.TempDir()) // no ./workflows dir here
	writeListSpec(t, filepath.Join(extra, "kickoff.yaml"), "kickoff")

	swapRunRunner(t, echoRunner("done "), nil)

	out, err := runWfRun(t, "--workflow-dir", extra, "kickoff")
	if err != nil {
		t.Fatalf("run with --workflow-dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "status") {
		t.Errorf("report missing status:\n%s", out)
	}
}

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bulkWorkspace(t *testing.T) (string, Workspace) {
	t.Helper()
	dir := t.TempDir()
	pv := NewPathValidator(dir, false, nil)
	pv.WorkDir = pv.WorkspaceRoot
	return pv.WorkspaceRoot, newLocalWorkspace(pv)
}

func seedBulk(t *testing.T, root string) {
	t.Helper()
	for _, rel := range []string{"a.txt", "b.txt", "sub/c.txt", "keep.go"} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBulkFilesDryRunDoesNotMutate(t *testing.T) {
	dir, ws := bulkWorkspace(t)
	src := filepath.Join(dir, "src")
	seedBulk(t, src)
	tool := &BulkFilesTool{WS: ws}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"action": "copy", "pattern": "*.txt", "path": src,
		"to": filepath.Join(dir, "out"), "dry_run": true,
	}))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "copy") || !strings.Contains(out, "a.txt") {
		t.Fatalf("dry run output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("dry run created output: %v", err)
	}
	if tool.IsDangerous(`{"action":"delete","pattern":"*","dry_run":true}`) {
		t.Error("a dry run should not require approval")
	}
	if !tool.IsDangerous(`{"action":"delete","pattern":"*"}`) {
		t.Error("a real bulk change must require approval")
	}
}

func TestBulkFilesCopyPreservesRelativePath(t *testing.T) {
	dir, ws := bulkWorkspace(t)
	src := filepath.Join(dir, "src")
	seedBulk(t, src)
	tool := &BulkFilesTool{WS: ws}
	to := filepath.Join(dir, "out")
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"action": "copy", "pattern": "**/*.txt", "path": src, "to": to, "mkdir_p": true,
	}))
	if err != nil {
		t.Fatalf("copy: %v (%s)", err, out)
	}
	for _, rel := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		if _, err := os.Stat(filepath.Join(to, rel)); err != nil {
			t.Errorf("copied %s missing: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(to, "keep.go")); !os.IsNotExist(err) {
		t.Errorf("copy included non-matching file")
	}
}

func TestBulkFilesRefusesSymlinkDestination(t *testing.T) {
	dir, ws := bulkWorkspace(t)
	src := filepath.Join(dir, "src")
	seedBulk(t, src)
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "out")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(to, "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tool := &BulkFilesTool{WS: ws}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"action": "copy", "pattern": "**/*.txt", "path": src, "to": to, "mkdir_p": true,
	}))
	if err == nil {
		t.Fatalf("bulk copy wrote through a destination symlink: %s", out)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error does not name the symlink: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "c.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("bulk copy escaped to %s: %v", outside, statErr)
	}
}

func TestBulkFilesDeleteAndFailureReporting(t *testing.T) {
	dir, ws := bulkWorkspace(t)
	src := filepath.Join(dir, "src")
	seedBulk(t, src)
	tool := &BulkFilesTool{WS: ws}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"action": "delete", "pattern": "*.txt", "path": src,
	})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, rel := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		if _, err := os.Stat(filepath.Join(src, rel)); !os.IsNotExist(err) {
			t.Errorf("delete left %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "keep.go")); err != nil {
		t.Errorf("delete removed keep.go: %v", err)
	}

	// A pre-existing destination without force is a partial failure: the
	// other file still copies, but the call reports a non-nil error.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "out")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "a.txt"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"action": "copy", "pattern": "*.txt", "path": src, "to": to,
	}))
	if err == nil {
		t.Fatalf("bulk copy with an existing destination reported success: %s", out)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error does not name the existing destination: %v", err)
	}
}

// TestBulkFilesMutationHoldsTheWholeSet pins the fileguard.go contract for a
// multi-path tool: ONE acquisition over every matched source plus its resolved
// destination, held across the per-file primitives. Without it a bulk call was a
// bare sequence of copies/renames a concurrent writer could interleave with,
// file by file.
func TestBulkFilesMutationHoldsTheWholeSet(t *testing.T) {
	pv := NewPathValidator(t.TempDir(), false, nil)
	pv.WorkDir = pv.WorkspaceRoot
	src := filepath.Join(pv.WorkspaceRoot, "src")
	seedBulk(t, src)
	to := filepath.Join(pv.WorkspaceRoot, "out")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}

	probe := newBlockingTransferWorkspace(newLocalWorkspace(pv))
	tool := &BulkFilesTool{WS: probe}
	args := mustJSON(t, map[string]any{"action": "copy", "pattern": "*.txt", "path": src, "to": to, "mkdir_p": true})
	done := runTransferAsync(tool, args)
	defer probe.unblock()
	waitParked(t, probe)

	// The whole set is held, not just the file whose copy is in flight.
	for _, p := range []string{
		filepath.Join(src, "a.txt"),
		filepath.Join(src, "b.txt"),
		filepath.Join(src, "sub", "c.txt"),
		filepath.Join(to, "a.txt"),
		filepath.Join(to, "b.txt"),
		filepath.Join(to, "sub", "c.txt"),
	} {
		if !pathLockHeld(p) {
			t.Errorf("%s is not locked while the bulk copy runs: the call takes no set lock", p)
		}
	}

	probe.unblock()
	if err := <-done; err != nil {
		t.Fatalf("bulk copy: %v", err)
	}
	for _, rel := range []string{"a.txt", "b.txt", filepath.Join("sub", "c.txt")} {
		if _, err := os.Stat(filepath.Join(to, rel)); err != nil {
			t.Errorf("copied %s missing: %v", rel, err)
		}
	}
	for _, p := range []string{filepath.Join(src, "a.txt"), filepath.Join(to, "a.txt")} {
		if pathLockHeld(p) {
			t.Errorf("%s is still locked after the bulk call returned", p)
		}
	}
}

// TestBulkFilesDryRunTakesNoLock pins the flip side: a dry run writes nothing, so
// it must not take the write guard. Every lock the mutating path would take is
// held here, so a dry run that acquired them would park — the bounded wait turns
// that into a failure instead of a hung test.
func TestBulkFilesDryRunTakesNoLock(t *testing.T) {
	dir, ws := bulkWorkspace(t)
	src := filepath.Join(dir, "src")
	seedBulk(t, src)
	to := filepath.Join(dir, "out")
	unlock := lockConflictPathSet(
		filepath.Join(src, "a.txt"),
		filepath.Join(src, "b.txt"),
		filepath.Join(src, "sub", "c.txt"),
		filepath.Join(to, "a.txt"),
		filepath.Join(to, "b.txt"),
		filepath.Join(to, "sub", "c.txt"),
	)
	defer unlock()

	tool := &BulkFilesTool{WS: ws}
	args := mustJSON(t, map[string]any{"action": "copy", "pattern": "*.txt", "path": src, "to": to, "dry_run": true})
	type outcome struct {
		out string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := tool.Execute(context.Background(), args)
		done <- outcome{out, err}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the dry run blocked on the per-path lock: a preview must stay lock-free")
	}
	if got.err != nil {
		t.Fatalf("dry run: %v", got.err)
	}
	if !strings.Contains(got.out, "a.txt") {
		t.Errorf("dry run output = %q, want the matched files", got.out)
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Errorf("dry run created the destination root: %v", err)
	}
}

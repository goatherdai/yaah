package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentctx "github.com/goatherdai/yaah/internal/agent/context"
)

func TestReadTool_readsExistingFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.txt")
	os.WriteFile(path, []byte("hello\ngoodbye\n"), 0o644)

	rt := &ReadTool{}
	args, _ := json.Marshal(map[string]any{"filePath": path})
	result, err := rt.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !contains(result, "hello") {
		t.Errorf("expected content 'hello', got %q", result)
	}
}

func TestReadTool_respectsOffsetAndLimit(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "lines.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	rt := &ReadTool{}
	args, _ := json.Marshal(map[string]any{"filePath": path, "offset": 2, "limit": 2})
	result, err := rt.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !strings.HasPrefix(result, "line2\nline3") {
		t.Errorf("expected 'line2\\nline3' prefix, got %q", result)
	}
	if !strings.HasSuffix(result, moreAvailableMarker) {
		t.Errorf("a limit-shortened read must be marked truncated, got %q", result)
	}
}

func TestReadTool_offsetPastEOFIsExplicit(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "lines.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	rt := &ReadTool{}
	args, _ := json.Marshal(map[string]any{"filePath": path, "offset": 99})
	_, err := rt.Execute(context.Background(), string(args))
	if err == nil {
		t.Fatal("an offset past EOF must be an error, not empty content")
	}
	for _, want := range []string{"offset 99", "past end of file", "5 lines"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestReadTool_limitCoveringAllLinesHasNoMarker(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "lines.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	rt := &ReadTool{}
	args, _ := json.Marshal(map[string]any{"filePath": path, "offset": 1, "limit": 5})
	result, err := rt.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if strings.Contains(result, moreAvailableMarker) {
		t.Errorf("limit covering the whole file must not be marked truncated: %q", result)
	}
	if result != "line1\nline2\nline3\nline4\nline5" {
		t.Errorf("unexpected content: %q", result)
	}
}

func TestReadTool_emptyFileOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(path, nil, 0o644)

	rt := &ReadTool{}
	args, _ := json.Marshal(map[string]any{"filePath": path, "offset": 1})
	result, err := rt.Execute(context.Background(), string(args))
	if err != nil || result != "" {
		t.Errorf("offset 1 on an empty file = (%q, %v), want (\"\", nil)", result, err)
	}

	args, _ = json.Marshal(map[string]any{"filePath": path, "offset": 2})
	if _, err := rt.Execute(context.Background(), string(args)); err == nil {
		t.Error("offset 2 on an empty file must be an error")
	}
}

func TestReadTool_returnsErrorForMissingFile(t *testing.T) {
	rt := &ReadTool{}
	_, err := rt.Execute(context.Background(), `{"filePath":"/nonexistent/file.txt"}`)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadTool_returnsErrorForEmptyPath(t *testing.T) {
	rt := &ReadTool{}
	_, err := rt.Execute(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestReadTool_returnsErrorForInvalidJSON(t *testing.T) {
	rt := &ReadTool{}
	_, err := rt.Execute(context.Background(), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestReadTool_schemaIsValidJSON(t *testing.T) {
	rt := &ReadTool{}
	var v any
	if err := json.Unmarshal(rt.Schema(), &v); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestReadTool_PagesRawSpill (yaah-vw1v): a spill file holds the raw output, so
// the read tool's offset/limit walks it as real lines. For a single-line blob
// the honest answer is one long line; the pager sibling the loop writes beside
// it is what gives offset/limit line structure to step through.
func TestReadTool_PagesRawSpill(t *testing.T) {
	dir := t.TempDir()
	var body strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&body, "line %02d\n", i)
	}
	spill := filepath.Join(dir, "1-tool.txt")
	if err := os.WriteFile(spill, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadTool{}
	out, err := tool.Execute(context.Background(), fmt.Sprintf(`{"filePath":%q,"offset":20,"limit":3}`, spill))
	if err != nil {
		t.Fatalf("read offset/limit: %v", err)
	}
	if !strings.HasPrefix(out, "line 19\nline 20\nline 21") {
		t.Fatalf("unexpected slice: %q", out)
	}
	if !strings.HasSuffix(out, moreAvailableMarker) {
		t.Errorf("a partial window must say more lines remain: %q", out)
	}

	// A single-line blob is one long line: offset 1 returns all of it, which is
	// honest rather than a wrapping fiction.
	oneLine := filepath.Join(dir, "2-tool.txt")
	blob := `{"result":"` + strings.Repeat("y", 5000) + `"}`
	if err := os.WriteFile(oneLine, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tool.Execute(context.Background(), fmt.Sprintf(`{"filePath":%q,"offset":1,"limit":10}`, oneLine))
	if err != nil {
		t.Fatalf("read raw blob: %v", err)
	}
	if got != blob {
		t.Errorf("single-line spill should read back as the one original line: got %d bytes, want %d", len(got), len(blob))
	}
}

// TestReadTool_readsSessionSpillOutsideWorkspace (review round 1, yaah-vw1v):
// a truncation notice promises the spilled output is readable with
// offset/limit, but the spill directory lives outside the workspace. The
// composition root registers it as a read-only exemption on the session's path
// validator (cmd/yaah/wiring_stages.go), so the promise has to be servable
// through the read tool end to end.
func TestReadTool_readsSessionSpillOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	spillDir := filepath.Join(t.TempDir(), "truncated")

	var body strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&body, "line %02d\n", i)
	}
	spill, err := agentctx.WriteSpool(spillDir, "tool", []byte(body.String()))
	if err != nil {
		t.Fatalf("WriteSpool: %v", err)
	}

	pv := NewPathValidator(workspace, false, nil)
	pv.AddReadAllowRoot(spillDir)
	tool := &ReadTool{PV: pv}

	out, err := tool.Execute(context.Background(), fmt.Sprintf(`{"filePath":%q,"offset":20,"limit":3}`, spill))
	if err != nil {
		t.Fatalf("the session's own spill must be readable: %v", err)
	}
	if !strings.HasPrefix(out, "line 19\nline 20\nline 21") {
		t.Fatalf("unexpected slice: %q", out)
	}

	// Without the grant the same read is refused: the exemption is what serves
	// the notice, not a general hole.
	contained := &ReadTool{PV: NewPathValidator(workspace, false, nil)}
	if _, err := contained.Execute(context.Background(), fmt.Sprintf(`{"filePath":%q}`, spill)); err == nil {
		t.Fatal("a contained session must refuse the spill path")
	}
}

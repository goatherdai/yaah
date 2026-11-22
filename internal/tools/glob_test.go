package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobTool_findsMatchingFiles(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "a.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tmp, "b.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tmp, "c.txt"), []byte("x"), 0o644)

	gt := &GlobTool{}
	args, _ := json.Marshal(map[string]any{"pattern": "*.go", "path": tmp})
	result, err := gt.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !contains(result, "a.go") || !contains(result, "b.go") {
		t.Errorf("expected a.go and b.go, got %q", result)
	}
	if contains(result, "c.txt") {
		t.Errorf("should not contain c.txt, got %q", result)
	}
}

func TestGlobTool_noMatches(t *testing.T) {
	tmp := t.TempDir()
	gt := &GlobTool{}
	args, _ := json.Marshal(map[string]any{"pattern": "*.nonexistent", "path": tmp})
	result, err := gt.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if result != "No files found." {
		t.Errorf("expected 'No files found.', got %q", result)
	}
}

func TestGlobTool_returnsErrorForEmptyPattern(t *testing.T) {
	gt := &GlobTool{}
	_, err := gt.Execute(context.Background(), `{"path":"."}`)
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
}

func TestGlobNative_directoryPrefixedPatterns(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "goatherd-ref", "workspace-handles")
	os.MkdirAll(nested, 0o755)
	os.WriteFile(filepath.Join(nested, "a.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tmp, "goatherd-ref", "b.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tmp, "top.py"), []byte("x"), 0o644)

	tests := []struct {
		name    string
		pattern string
		want    []string
		notWant []string
	}{
		{
			name:    "recursive directory prefix",
			pattern: "goatherd-ref/**/*.py",
			want:    []string{"a.py", "b.py"},
			notWant: []string{"top.py"},
		},
		{
			name:    "single-level directory prefix",
			pattern: "goatherd-ref/workspace-handles/*.py",
			want:    []string{"a.py"},
			notWant: []string{"b.py", "top.py"},
		},
		{
			name:    "bare recursive pattern",
			pattern: "**/*.py",
			want:    []string{"a.py", "b.py", "top.py"},
		},
		{
			name:    "absolute pattern",
			pattern: filepath.ToSlash(filepath.Join(tmp, "goatherd-ref", "**", "*.py")),
			want:    []string{"a.py", "b.py"},
			notWant: []string{"top.py"},
		},
		{
			name:    "basename pattern",
			pattern: "*.py",
			want:    []string{"a.py", "b.py", "top.py"},
		},
	}

	gt := &GlobTool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := gt.globNative(context.Background(), tc.pattern, tmp)
			if err != nil {
				t.Fatalf("globNative() error: %v", err)
			}
			for _, want := range tc.want {
				if !contains(result, want) {
					t.Errorf("pattern %q: expected %q in %q", tc.pattern, want, result)
				}
			}
			for _, notWant := range tc.notWant {
				if contains(result, notWant) {
					t.Errorf("pattern %q: did not expect %q in %q", tc.pattern, notWant, result)
				}
			}
		})
	}
}

// TestGlobRipgrep_truncationFooterKeepsCount pins the ripgrep path's
// truncation footer to the native walker's shape. Without it, a capped
// ripgrep result drops "how many files matched", and consumers that read the
// footer as their authoritative count have nothing to read.
func TestGlobRipgrep_truncationFooterKeepsCount(t *testing.T) {
	if !rgAvailable() {
		t.Skip("rg not available")
	}
	tmp := t.TempDir()
	// Enough short-named files that the joined list exceeds
	// toolResultMaxBytes, so the cap (and its footer) is exercised for real
	// rather than through a lowered threshold.
	const want = 400
	for i := 0; i < want; i++ {
		name := fmt.Sprintf("f%03d.go", i)
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error: %v", name, err)
		}
	}

	gt := &GlobTool{}
	ctx := context.Background()
	ws := workspaceOf(gt.WS, gt.PV)
	filter, err := newNestedWorktreeFilter(ctx, ws, tmp)
	if err != nil {
		t.Fatalf("newNestedWorktreeFilter() error: %v", err)
	}
	result, err := gt.globRipgrep(ctx, "*.go", tmp, filter)
	if err != nil {
		t.Fatalf("globRipgrep() error: %v", err)
	}

	// The listing really was capped: body plus footer can only be larger than
	// the cap, and the cap keeps it near toolResultMaxBytes.
	if len(result) <= toolResultMaxBytes {
		t.Fatalf("expected the %d-file listing to exceed toolResultMaxBytes (%d), got %d bytes",
			want, toolResultMaxBytes, len(result))
	}
	footer := fmt.Sprintf("...[truncated: %d files total]...", want)
	if !strings.HasSuffix(result, footer) {
		tail := result
		if len(tail) > 96 {
			tail = tail[len(tail)-96:]
		}
		t.Fatalf("expected footer %q at end of result, got tail %q", footer, tail)
	}
	if strings.Contains(result, truncationMarker) {
		t.Errorf("bare %q marker should be replaced by the count footer", truncationMarker)
	}
	if !strings.Contains(result, "f000.go") {
		t.Errorf("expected the head of the listing to survive the cap")
	}
	last := fmt.Sprintf("f%03d.go", want-1)
	if strings.Contains(result, last) {
		t.Errorf("expected the listing to be cut before %s", last)
	}
}

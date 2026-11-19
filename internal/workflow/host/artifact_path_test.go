package host

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveArtifact(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "artifacts")
	t.Run("nested local path", func(t *testing.T) {
		got, err := resolveArtifact(root, "out/note.md")
		if err != nil {
			t.Fatalf("resolveArtifact: %v", err)
		}
		if want := filepath.Join(root, "out", "note.md"); got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})
	t.Run("parent traversal refused", func(t *testing.T) {
		_, err := resolveArtifact(root, "../escape.md")
		if err == nil || !strings.Contains(err.Error(), "escapes the artifact root") {
			t.Errorf("err = %v, want the escape named", err)
		}
	})
	t.Run("nested parent traversal refused", func(t *testing.T) {
		_, err := resolveArtifact(root, "out/../../escape.md")
		if err == nil || !strings.Contains(err.Error(), "escapes the artifact root") {
			t.Errorf("err = %v, want the escape named", err)
		}
	})
	t.Run("absolute path refused", func(t *testing.T) {
		abs := filepath.Join(string(filepath.Separator), "etc", "passwd")
		_, err := resolveArtifact(root, abs)
		if err == nil || !strings.Contains(err.Error(), "is absolute") {
			t.Errorf("err = %v, want the absolute path named", err)
		}
	})
	t.Run("empty path refused", func(t *testing.T) {
		if _, err := resolveArtifact(root, ""); err == nil {
			t.Error("empty artifact path accepted")
		}
	})
}

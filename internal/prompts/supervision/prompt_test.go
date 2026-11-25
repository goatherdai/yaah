package supervision

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf8"
)

// metaPromptV1SHA256 is the digest of the upstream asset
// (shepherd-agents/shepherd-experiments exp/live-intervention/prompts/
// meta_prompt_v1.txt at HEAD c12ebd1b, blob
// 585c6923abd5a5ba749b2d683b1c119508142fb4). The paper reproduces the same
// text as its Appendix E prompt, so this digest is the parity baseline the
// whole measurement arm (yaah-1050.1.5) rests on: if it drifts, the heads
// are no longer measured against the paper's protocol.
const metaPromptV1SHA256 = "72309b5ed5ce992e1a556916bee8df069e71e5b579e267adcb49960b661dcfea"

func TestMetaPromptV1_Drift(t *testing.T) {
	got := sha256.Sum256([]byte(MetaPromptV1))
	t.Run("byte length", func(t *testing.T) {
		if n := len(MetaPromptV1); n != 4200 {
			t.Errorf("len(MetaPromptV1) = %d, want 4200 (the upstream asset is 4200 bytes)", n)
		}
	})
	t.Run("sha256", func(t *testing.T) {
		if sum := hex.EncodeToString(got[:]); sum != metaPromptV1SHA256 {
			t.Errorf("sha256 = %s, want %s (the asset drifted from upstream)", sum, metaPromptV1SHA256)
		}
	})
	t.Run("exactly one trailing newline", func(t *testing.T) {
		if !strings.HasSuffix(MetaPromptV1, "\n") {
			t.Fatal("asset does not end with a newline")
		}
		if strings.HasSuffix(MetaPromptV1, "\n\n") {
			t.Error("asset ends with a blank line; it must end after the final period plus one newline")
		}
	})
	t.Run("line count", func(t *testing.T) {
		if n := strings.Count(MetaPromptV1, "\n"); n != 35 {
			t.Errorf("newline count = %d, want 35", n)
		}
	})
	t.Run("valid utf8", func(t *testing.T) {
		if !utf8.ValidString(MetaPromptV1) {
			t.Fatal("asset is not valid UTF-8")
		}
	})
	t.Run("ascii straight quotes only", func(t *testing.T) {
		for _, r := range []rune{'\u2018', '\u2019', '\u201C', '\u201D'} {
			if strings.ContainsRune(MetaPromptV1, r) {
				t.Errorf("asset contains a curly quote %q; the upstream text uses ASCII straight quotes", r)
			}
		}
	})
	t.Run("non-ascii preserved", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			r    rune
		}{
			{"U+226A much-less-than", '\u226A'},
			{"U+2013 en dash", '\u2013'},
			{"U+2014 em dash", '\u2014'},
		} {
			if !strings.ContainsRune(MetaPromptV1, tc.r) {
				t.Errorf("asset is missing %s (U+%04X)", tc.name, tc.r)
			}
		}
	})
	t.Run("action vocabulary", func(t *testing.T) {
		for _, action := range []string{`"none"`, `"steer"`, `"redirect"`, `"revert"`} {
			if !strings.Contains(MetaPromptV1, action) {
				t.Errorf("asset is missing the action %s", action)
			}
		}
	})
}

func TestMetaPromptV1Name(t *testing.T) {
	if MetaPromptV1Name != "meta_prompt_v1" {
		t.Errorf("MetaPromptV1Name = %q, want %q (the config token)", MetaPromptV1Name, "meta_prompt_v1")
	}
}

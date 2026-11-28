// The snapshot's feature description (yaah-k0x1): what the supervisor
// reads to decide whether an agent's work still matches its task.
// DeriveFeature derives it once per dispatch, before Register — the
// dispatch label when one was supplied, otherwise the collapsed prompt —
// so the registry never holds a full prompt; clipRunes is the rune-safe
// cap both the derivation and the builder's defensive clip share, so no
// feature leaves this package over the design's cap.

package supervision

import (
	"github.com/goatherdai/yaah/internal/textclip"
	"strings"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// DeriveFeature derives a run's snapshot feature description: the
// caller-supplied dispatch label wins when non-empty; otherwise the
// dispatch prompt is trimmed, every run of whitespace collapses to one
// space, and the result is clipped to MaxFeatureRunes runes. The runner
// calls it ONCE per dispatch, before registering, so the registry never
// holds a full prompt.
func DeriveFeature(label, prompt string) string {
	if label = strings.TrimSpace(label); label != "" {
		return clipRunes(label, supervisionapi.MaxFeatureRunes)
	}
	return clipRunes(strings.Join(strings.Fields(prompt), " "), supervisionapi.MaxFeatureRunes)
}

// clipRunes caps a string to max runes, the rune-safe sibling of
// clipLine: a rune cap is what a feature description's "350 chars" means,
// and a byte cut would break it on the first non-ASCII summary. A longer
// string is sliced to the cap and nothing is appended — the reference
// implementation this design reproduces slices to 350 characters and adds
// no marker — so the result never exceeds the cap it reports and clipping
// is idempotent.
func clipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	kept, _ := textclip.Runes(s, max)
	return kept
}

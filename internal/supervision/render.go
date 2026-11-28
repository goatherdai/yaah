// Package supervision renders a supervision snapshot into the meta-agent
// prompt text the supervisor policy loop hands to a decider: one text format
// shared by the future `supervise` tool (yaah-fy28, agent mode) and the
// headless decider (yaah-0nbb), which is what makes the two modes comparable
// in measurement (yaah-1050.1.5).
//
// It is a PURE helper: no clock reads (the tick number and the per-scope age
// in ticks arrive in RenderOptions), no trace or registry reads (every fact
// comes from the supervisionapi.Snapshot it is given), no I/O, and no state
// that outlives one call. Rendering the same snapshot twice yields the same
// bytes.
//
// Import direction is one-way: internal/supervisionapi must never import this
// package.
//
// Three packages in this tree are named `supervision`: this renderer
// (internal/supervision), the agent-side snapshot builder
// (internal/agent/supervision), and the locked prompt assets
// (internal/prompts/supervision). The collision is accepted rather than
// renamed — docs/supervisor-policy-design.md §4.3 and §4.4 fix all three
// import paths — so a file that imports two of them must alias one
// deliberately.
//
// Design: docs/supervisor-policy-design.md §4.3.
package supervision

import (
	"fmt"
	"github.com/goatherdai/yaah/internal/textclip"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// DefaultRenderBudgetTokens is the SNAPSHOT-ONLY budget
// (docs/supervisor-policy-design.md §4.3): the snapshot is the part of the
// paper's sub-3K-token prompt a renderer controls, so the rendered snapshot
// alone is bounded by it.
//
// It is NOT the whole prompt. Headless mode (yaah-0nbb) pairs this text with
// the locked v1 system prompt — internal/prompts/supervision's 4200-byte
// asset, about 1.4K estimated tokens by this file's own estimator (4200/4 ×
// 1.3 ≈ 1365; a bare chars/4 would say 1050, under a third too low) — so a
// caller that also sends a system prompt must reserve that estimate and pass
// a REDUCED RenderOptions.BudgetTokens, or the pair reaches ~4.4K (3000 +
// 1365). That reservation is exactly what the per-call budget seam is for;
// this package stays a leaf and does not import the prompt package to
// compute it.
const DefaultRenderBudgetTokens = 3000

// estimateFactor is internal/agent/context's DefaultEstimateFactor. The
// convention is chars/4 tokens times 1.3 to compensate for provider
// tokenizers undercounting code and JSON (internal/agent/context/tokens.go);
// it is duplicated here as two lines rather than imported, because this
// package must stay a leaf with no dependency on the agent family — the
// formula, not the package, is the contract.
const estimateFactor = 1.3

// Render-time caps on model-authored text. These are a looser backstop, not a
// re-application of the builder's cuts: the snapshot builder bounds args and
// errors in BYTES (internal/agent/supervision/snapshot.go, 160) while
// maxArgRunes and maxErrorRunes are RUNE caps, so a multi-byte argument can
// satisfy both. The file list and the in-flight list are capped or
// more-generously capped at source, which is why the renderer cuts them too
// and says by how much. The point in every case is that a hand-built snapshot
// (a test, or a future source) cannot blow the budget through one agent.
//
// maxPathRunes caps ONE modified-file path. A path is model-authored the same
// way an argument is (internal/agent/supervision's argsFilePath reads it out
// of a tool call's arguments), so it is collapsed and clipped like one: a raw
// path carrying a newline could otherwise forge a block header or a field
// line in the text a revert decision is taken from.
const (
	maxArgRunes         = 160
	maxErrorRunes       = 100
	maxNoteRunes        = 200
	maxPathRunes        = 120
	maxFilesRendered    = 8
	maxInFlightRendered = 5
	trailingNewline     = "\n"
	emptyFeatureValue   = "(none)"
)

// footerJSON is the return instruction the locked v1 prompt's protocol
// expects, in yaah's decision vocabulary: the keys are
// supervisionapi.Decision's field names (encoding/json matches them
// case-insensitively, so Scope decodes from "scope") and the action values
// are the Action constants — a conforming reply therefore unmarshals straight
// into the decision type the decider returns. It carries ONE valid JSON
// example, because a `"a"|"b"` menu is not JSON, and states the allowed
// values as prose.
//
// The paper's per-decision key is "agent" and its verbs are
// inject/handoff/discard: yaah names the scope label (the label THIS text
// assigned) and maps those verbs onto steer/redirect/revert. The pinned
// contract test (TestRender_FooterContract) parses the example below into
// supervisionapi.Decision, so a change to either side fails the build.
//
// The two word limits are INTERPOLATED from supervisionapi.MaxReasonWords and
// MaxGuidanceWords rather than written out: those constants are what the
// arbiter (yaah-3l34) and the headless decider (yaah-0nbb) enforce, so a
// literal here would leave the instruction the model reads silently
// disagreeing with the limit applied to its reply. fmt.Sprintf is not
// constant, hence a package-level var built once at init — it is still
// deterministic and read-only in practice.
var footerJSON = fmt.Sprintf(`Return one JSON object: {"decisions": [{"scope": "w1", "action": "steer", "reason": "...", "guidance": "..."}]}. "scope" is a label from above (w1, w2, …); "action" is "none", "steer", "redirect" or "revert"; "reason" is max %d words; "guidance" is max %d words and required for steer, redirect and revert.`,
	supervisionapi.MaxReasonWords, supervisionapi.MaxGuidanceWords)

// TickState is batch-owned state the snapshot does not carry: the
// meta-agent's last intervention on a scope and its age in ticks. The Batch
// that owns it lands in yaah-3l34; until then a caller passes whatever it
// tracks, and a missing entry (or a zero value) means "never acted".
type TickState struct {
	// LastIntervention is none|steer|redirect|revert — the action of the
	// last intervention the META-AGENT took on this scope, empty or "none"
	// when there was none. It is deliberately not named LastAction:
	// supervisionapi.AgentSnapshot.LastAction is the scope's OWN most recent
	// tool call, a different fact that the supervisor tool's status view
	// prints as "last:". (The paper calls this field "last action" and the
	// loop keeps it as ticks_since_last_action; the RENDERED key is
	// last_intervention for the same reason.)
	LastIntervention string
	// TicksAgo is the number of ticks since that intervention.
	TicksAgo int
}

// RenderOptions is the per-call context the snapshot cannot supply.
type RenderOptions struct {
	// Tick is the tick number rendered in the header. 0 renders the
	// header without a tick number.
	Tick int
	// BudgetTokens bounds the estimated size of the WHOLE rendered text —
	// header, agent blocks, and return instruction — because that is what
	// the decider is charged for; Render compares estimateTokens of the
	// complete text against it. 0 means DefaultRenderBudgetTokens.
	//
	// There is a floor the budget cannot squeeze past: the header and the
	// return instruction are irreducible (about 110 estimated tokens, 125
	// with the omission footer), so a budget at or below that floor still
	// renders non-empty text — header, omitted-agents footer, and instruction
	// — rather than nothing. Termination is guaranteed at every budget.
	BudgetTokens int
	// State is the batch's per-scope tick state, keyed by ScopeID. Nil is
	// fine and renders every scope as last_intervention=none.
	State map[string]TickState
	// Known seeds the label assignment from a previous render: ScopeID →
	// the label it was given then. Nil means "assign from this snapshot
	// alone" (ScopeID ascending, w1..wN).
	//
	// It exists so a caller that carries observations from one tick to the
	// next keeps naming the same worker the same name; see the Render doc
	// comment for what is and is not stable without it.
	Known map[string]string
}

// Labeled binds a rendered short label to the scope it names, so a Decision
// whose Scope is "w2" resolves back to a scope ID without re-deriving the
// assignment.
type Labeled struct {
	Label   string
	ScopeID string
}

// labeledAgent is one agent plus the label the text calls it by.
type labeledAgent struct {
	label string
	snap  supervisionapi.AgentSnapshot
}

// limits are the degradation knobs one render attempt is made under. The
// ladder in Render only ever TIGHTENS them, which is what makes each attempt
// render no more text than its predecessor for any input.
type limits struct {
	// trail is the maximum number of trailing tool calls rendered per
	// agent; -1 renders the whole (already capped) trail, 0 omits the
	// section entirely.
	trail int
	// hideFinishedHeader replaces a finished agent's role/running line with
	// a one-token "state: finished" marker: the role is redundant for one
	// that has stopped, but the block must still say that it has.
	hideFinishedHeader bool
	// featureMax clips every agent's feature line to this many runes;
	// 0 or less applies supervisionapi.MaxFeatureRunes.
	featureMax int
	// omitted counts the agents cut from this attempt, for the honest
	// footer.
	omitted int
}

// Render renders snap into meta-agent prompt text and returns the label map
// the text used.
//
// The returned labels cover EVERY agent in the snapshot, not just the ones
// that survived budget degradation, and degradation never renumbers them:
// renumbering under budget pressure would silently re-point a decision.
//
// Label stability has two different scopes, and this comment used to over-claim
// the second. WITHIN one snapshot a label is stable by construction — a pure
// function of this scope set (ScopeID ascending, w1..wN) — so a neighbour
// finishing does not move it. ACROSS ticks it is stable only when the caller
// threads RenderOptions.Known: the assignment is recomputed from the current
// scope set on every call, so a scope joining at a lower ScopeID, or a
// finished scope leaving the bounded history, otherwise reshuffles the rest
// and an agent-mode supervisor carrying observations across ticks associates
// them with the wrong worker. A caller that does carry them — the Batch,
// yaah-3l34 — must pass the previous call's labels back as Known.
//
// Budget degradation is tiered, cheapest information loss first, and each
// tier is re-rendered until the estimate fits (see the tier comment on the
// Render body). The tiers are MONOTONE: each attempt renders no more text
// than the attempt before it, for any input, so a tighter budget always
// yields a shorter render and never a longer one.
func Render(snap supervisionapi.Snapshot, opts RenderOptions) (text string, labels []Labeled) {
	budget := opts.BudgetTokens
	if budget <= 0 {
		budget = DefaultRenderBudgetTokens
	}
	labels, ordered := labelAgents(snap.Agents, opts.Known)
	r := renderer{tick: opts.Tick, state: opts.State, dropped: snap.Dropped}
	// The header's live/finished counts cover the WHOLE ordered set, before
	// tier 4 omits anyone: the header states the true agent count and the
	// footer states how many of them went unrendered, so the decider can
	// tell a quiet scope from a cut one.
	for _, a := range ordered {
		if a.snap.StillRunning {
			r.live++
		} else {
			r.finished++
		}
	}

	// The ladder below is MONOTONE: every attempt renders no more text than
	// the attempt before it, for any input, so the tiers are cumulative
	// reductions and a tighter budget can never produce a longer render. The
	// earlier shape restarted tier 2 at a 1-call trail after tier 1 had
	// already reached 0, so a finished-heavy batch could render MORE text at
	// tier 2 than at the tier-1 attempt that had just failed.
	//
	// Tier 1 — shorten each agent's trail from the OLDEST end (the newest
	// calls are what a supervisor acts on). No tier trims any other field
	// here: the file list, the in-flight list, and the note are capped
	// unconditionally in writeBlock, so there is nothing left to trade.
	for _, trail := range []int{-1, 12, 6, 3, 1, 0} {
		if txt := r.render(ordered, limits{trail: trail}); estimateTokens(txt) <= budget {
			return txt, labels
		}
	}
	// Tier 2 — hide the role/running line of finished agents, replacing it
	// with a one-token "state: finished" marker, while still naming them and
	// keeping their feature, stats, file list, note, and (already trimmed)
	// trail section. Resumes at trail 0, where tier 1's last attempt ended.
	mid := limits{trail: 0, hideFinishedHeader: true}
	if txt := r.render(ordered, mid); estimateTokens(txt) <= budget {
		return txt, labels
	}
	// Tier 3 — clip EVERY agent's feature line (350 → 120 → 60 runes) before
	// any agent is dropped at all: a clipped feature keeps every scope named
	// and placeable, where tier 4 loses the agent outright. The cut is
	// visible (clipRunes ends a clipped field with an ellipsis) and the
	// feature is the least actionable field in the block.
	for _, featureMax := range []int{120, 60} {
		mid.featureMax = featureMax
		if txt := r.render(ordered, mid); estimateTokens(txt) <= budget {
			return txt, labels
		}
	}
	// Tier 4 — omit whole agents, oldest-finished first. Render order is
	// live (ScopeID ascending) then finished (newest first), so the tail
	// of the slice is exactly the oldest finished agents; with no finished
	// agents left the tail is the highest-ScopeID live ones. The footer
	// states the omission and the header keeps claiming the true agent
	// count, so the decider can tell a quiet scope from an unrendered one.
	for n := len(ordered) - 1; n >= 0; n-- {
		cut := mid
		cut.omitted = len(ordered) - n
		txt := r.render(ordered[:n], cut)
		if estimateTokens(txt) <= budget || n == 0 {
			return txt, labels
		}
	}
	// No agents at all: the header and footer are all that is left, and
	// they always fit.
	return r.render(nil, mid), labels
}

// renderer carries the per-call state every render attempt shares.
type renderer struct {
	tick     int
	state    map[string]TickState
	dropped  int
	live     int
	finished int
}

// render produces one candidate text. It is called repeatedly under
// progressively tighter limits, so it does no allocation it can avoid
// beyond the builder's.
func (r renderer) render(agents []labeledAgent, lim limits) string {
	var b strings.Builder

	// The total is the true agent count even when this attempt omitted
	// some of them; the omission is named in the same parenthesis so the
	// header alone never overstates what the decider can see.
	header := fmt.Sprintf("%d agents (%d live, %d finished",
		r.live+r.finished, r.live, r.finished)
	if lim.omitted > 0 {
		header += fmt.Sprintf(", %d omitted", lim.omitted)
	}
	header += ")"
	if r.tick > 0 {
		fmt.Fprintf(&b, "TICK %d — %s", r.tick, header)
	} else {
		b.WriteString(header)
	}
	if r.dropped > 0 {
		fmt.Fprintf(&b, " (+%d agents over the snapshot cap)", r.dropped)
	}
	b.WriteString(trailingNewline)

	for _, a := range agents {
		b.WriteString(trailingNewline)
		r.writeBlock(&b, a, lim)
	}
	if lim.omitted > 0 {
		fmt.Fprintf(&b, "\n(+%d agents omitted for budget)\n", lim.omitted)
	}
	b.WriteString(trailingNewline)
	b.WriteString(footerJSON)
	b.WriteString(trailingNewline)
	return b.String()
}

// writeBlock writes one agent's block, in the paper's Appendix E field order
// with yaah's extra fields.
func (r renderer) writeBlock(b *strings.Builder, a labeledAgent, lim limits) {
	s := a.snap
	fmt.Fprintf(b, "## AGENT %s (scope %s)\n", a.label, s.ScopeID)

	if !(lim.hideFinishedHeader && !s.StillRunning) {
		role := singleLine(s.Role)
		if role == "" {
			role = "(unknown)"
		}
		fmt.Fprintf(b, "role: %s   running: %s\n", role, yesNo(s.StillRunning))
	} else {
		// Tier 2 hides the role/running line of a finished agent, but the
		// block must stay self-identifying: without this marker a finished
		// block reads like a live one whose role line was cut, and the
		// header's live/finished split would be the only liveness fact left.
		// The marker is far shorter than the line it replaces, which is part
		// of what keeps the degradation ladder monotone.
		b.WriteString("state: finished\n")
	}

	// The note is disclosure, not decoration: an agent whose trace could not
	// be read must not render byte-identical to a quiet one. It sits in the
	// kept header/stats section rather than in the trail, so every
	// degradation tier that still shows the agent still shows why its facts
	// are thin.
	if note := clipRunes(singleLine(s.Note), maxNoteRunes); note != "" {
		fmt.Fprintf(b, "note: %s\n", note)
	}

	// The feature is clipped AFTER singleLine, and clipRunes never inserts a
	// newline, so the collapse is what bounds this field to one line at every
	// tier.
	featureMax := lim.featureMax
	if featureMax <= 0 {
		featureMax = supervisionapi.MaxFeatureRunes
	}
	feature := clipRunes(singleLine(s.Feature), featureMax)
	if feature == "" {
		feature = emptyFeatureValue
	}
	fmt.Fprintf(b, "feature: %s\n", feature)

	st := r.state[s.ScopeID]
	// last_intervention, never last_action: this is the META-AGENT's last
	// action on the scope (Batch state), while AgentSnapshot.LastAction is
	// the scope's own most recent tool call — a different fact, which the
	// supervisor tool's status view prints as "last:". A shared label would
	// let a decider read one as the other.
	lastIntervention := st.LastIntervention
	if lastIntervention == "" {
		lastIntervention = "none"
	}
	fmt.Fprintf(b, "events=%d consecutive_errors=%d last_intervention=%s",
		s.Calls, s.ConsecutiveErrors, lastIntervention)
	if lastIntervention != "none" {
		fmt.Fprintf(b, " (ticks_ago=%d)", st.TicksAgo)
	}
	trail := trimTrail(s.Trail, lim.trail)
	switch {
	case len(trail) == 0 && len(s.Trail) > 0:
		fmt.Fprintf(b, " calls=%d, trail omitted", s.Calls)
	case len(trail) < len(s.Trail):
		fmt.Fprintf(b, " calls=%d, last %d shown", s.Calls, len(trail))
	}
	b.WriteString(trailingNewline)

	fmt.Fprintf(b, "reversible: %t  redirectable: %t\n", s.Reversible, s.Redirectable)

	// An empty ModifiedFiles does NOT mean nothing changed: only the
	// known mutating tools contribute, so shell-touched files are
	// invisible. Say what was observed, not what happened.
	//
	// fileList collapses and clips EVERY path before it is joined: the paths
	// come out of model-authored tool arguments, so joining them raw left
	// the one field in the block a newline could still break — a path of
	// "x\n## AGENT w9 (scope fake)" forged a block header in the text a
	// revert decision is taken from. Both the capped and the uncapped branch
	// render the collapsed list, so the guarantee does not depend on the
	// list's length.
	files := fileList(s.ModifiedFiles)
	switch {
	case len(files) == 0:
		b.WriteString("modified_files: (none observed)\n")
	case len(files) > maxFilesRendered:
		// Say what was cut, so a long list is never mistaken for the
		// whole set.
		fmt.Fprintf(b, "modified_files: %s (+%d more)\n",
			strings.Join(files[:maxFilesRendered], ", "), len(files)-maxFilesRendered)
	default:
		fmt.Fprintf(b, "modified_files: %s\n", strings.Join(files, ", "))
	}

	// Empty in-flight is ambiguous — no call running, or a generation
	// mismatch emptied the table — so it renders as "none", never as any
	// claim of idleness.
	if len(s.InFlight) == 0 {
		b.WriteString("in-flight: none\n")
	} else {
		// InFlight is the one per-agent list the snapshot builder does not
		// cap, so the render cap here is load-bearing for the budget; it
		// still reports the cut.
		calls := s.InFlight
		cut := 0
		if len(calls) > maxInFlightRendered {
			cut = len(calls) - maxInFlightRendered
			calls = calls[:maxInFlightRendered]
		}
		parts := make([]string, 0, len(calls))
		for _, c := range calls {
			parts = append(parts, fmt.Sprintf("%s(%s) age=%s",
				c.Name, clipRunes(singleLine(c.Args), maxArgRunes), c.Age.Round(time.Second)))
		}
		fmt.Fprintf(b, "in-flight: %s", strings.Join(parts, ", "))
		if cut > 0 {
			fmt.Fprintf(b, " (+%d more)", cut)
		}
		b.WriteString(trailingNewline)
	}

	switch {
	case len(trail) == 0 && lim.trail == 0 && len(s.Trail) > 0:
		// Tightest tier: the section is omitted outright rather than
		// claiming "no tool calls yet", which would be false.
	case len(trail) == 0:
		b.WriteString("no tool calls yet\n")
	default:
		b.WriteString("recent tool calls (oldest → newest):\n")
		for i, c := range trail {
			fmt.Fprintf(b, "  %d. %s\n", i+1, callLine(c))
		}
	}
}

// callLine renders one trailing tool call. An unpaired digest renders as
// capture-missing, never "pending": the trace middleware appends declaration
// and capture together only after a call returns, so an unpaired declaration
// normally means the capture append failed (yaah-k0x1's ToolCallDigest note).
func callLine(c supervisionapi.ToolCallDigest) string {
	var b strings.Builder
	b.WriteString(c.Name)
	if args := clipRunes(singleLine(c.Args), maxArgRunes); args != "" {
		b.WriteString("(")
		b.WriteString(args)
		b.WriteString(")")
	}
	switch {
	case !c.Paired:
		b.WriteString(" →capture-missing")
	case c.Success:
		b.WriteString(" →ok")
	default:
		b.WriteString(" →error")
		if e := clipRunes(singleLine(c.Error), maxErrorRunes); e != "" {
			b.WriteString(":ERR ")
			b.WriteString(e)
		}
	}
	return b.String()
}

// trimTrail keeps the NEWEST max entries, preserving oldest-first order, and
// returns the input unchanged for max < 0.
func trimTrail(trail []supervisionapi.ToolCallDigest, max int) []supervisionapi.ToolCallDigest {
	if max < 0 || len(trail) <= max {
		return trail
	}
	return trail[len(trail)-max:]
}

// labelAgents assigns labels by ScopeID ascending over a copy of agents —
// positionally when known is nil, from known when it is not (see
// assignLabels) — and returns the render order: live agents by ScopeID
// ascending, then finished agents newest-first.
//
// Label assignment deliberately ignores the caller's slice order — the
// snapshot builder emits live registrations sorted by ScopeID and then
// finished ones newest-first, so a positional scheme would renumber every
// agent the moment a neighbour finished. (Pinning a decision to a particular
// run incarnation is not this package's job: the snapshot type carries no
// generation token, and the arbiter resolves a claimed label against the
// registry's generation at claim time — yaah-3l34 / yaah-fy28.)
func labelAgents(agents []supervisionapi.AgentSnapshot, known map[string]string) ([]Labeled, []labeledAgent) {
	byID := make([]supervisionapi.AgentSnapshot, len(agents))
	copy(byID, agents)
	sort.SliceStable(byID, func(i, j int) bool { return byID[i].ScopeID < byID[j].ScopeID })

	labelOf, labels := assignLabels(byID, known)

	live := make([]supervisionapi.AgentSnapshot, 0, len(byID))
	finished := make([]supervisionapi.AgentSnapshot, 0, len(byID))
	for _, a := range byID {
		if a.StillRunning {
			live = append(live, a)
			continue
		}
		finished = append(finished, a)
	}
	sort.SliceStable(finished, func(i, j int) bool {
		return finished[i].FinishedAt.After(finished[j].FinishedAt)
	})

	ordered := make([]labeledAgent, 0, len(byID))
	for _, a := range live {
		ordered = append(ordered, labeledAgent{label: labelOf[a.ScopeID], snap: a})
	}
	for _, a := range finished {
		ordered = append(ordered, labeledAgent{label: labelOf[a.ScopeID], snap: a})
	}
	return labels, ordered
}

// assignLabels gives every scope in byID (ScopeID ascending) a label and
// returns the label map plus the ordered assignment.
//
// With known == nil the assignment is w1..wN by position: a pure function of
// this snapshot's scope set. With known != nil:
//
//   - a scope already in known KEEPS its label, so an unchanged worker keeps
//     its name even when a lower-sorting scope joins the batch;
//   - a scope unknown to the caller takes the smallest wN not already in
//     use, so a newcomer never collides with a carried-over label;
//   - a known scope absent from this snapshot is simply not rendered, and its
//     label stays RESERVED for it — it is not handed to a newcomer this call,
//     so a scope that reappears next call finds its own label free instead of
//     pointing at a stranger.
func assignLabels(byID []supervisionapi.AgentSnapshot, known map[string]string) (map[string]string, []Labeled) {
	labelOf := make(map[string]string, len(byID))
	used := make(map[string]bool, len(byID)+len(known))
	for _, label := range known {
		used[label] = true
	}
	next := 1
	for _, a := range byID {
		label := known[a.ScopeID] // nil-map read is the empty string
		if label == "" {
			for {
				label = fmt.Sprintf("w%d", next)
				next++
				if !used[label] {
					break
				}
			}
		}
		labelOf[a.ScopeID] = label
		used[label] = true
	}
	labels := make([]Labeled, 0, len(byID))
	for _, a := range byID {
		labels = append(labels, Labeled{Label: labelOf[a.ScopeID], ScopeID: a.ScopeID})
	}
	return labelOf, labels
}

// estimateTokens mirrors internal/agent/context's chars/4 × 1.3 convention
// (DefaultEstimateFactor). It counts bytes, which over-estimates multi-byte
// text — the safe direction for a budget.
func estimateTokens(s string) int {
	return int(math.Ceil(float64(len(s)) / 4 * estimateFactor))
}

// singleLine collapses all whitespace runs to one space and trims, so
// model-authored text cannot break the one-line-per-field format.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clipRunes truncates s to at most max runes, ellipsis included: the markers
// are reserved INSIDE the budget, so a field capped at 350 runes is 350 runes
// long, not 351. Runes, not bytes: a byte cut would split a multi-byte
// character. max <= 0 means no cap, and the input is returned unchanged.
func clipRunes(s string, max int) string {
	if max == 1 {
		kept, truncated := textclip.Runes(s, 1)
		if !truncated {
			return kept // an empty or one-rune value already fits
		}
		return "…" // the marker is the whole budget
	}
	return textclip.HeadRunesWithin(s, max, "…")
}

// fileList renders a modified-file set for its one line of the block: every
// path through the same singleLine collapse as the other model-authored
// fields, then clipped to maxPathRunes so one absurd path cannot spend the
// budget. Paths that collapse to nothing are dropped rather than rendered as
// an empty slot between commas (an empty result renders as "(none observed)"
// at the call site, which is what a set of blank paths observed).
func fileList(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if path := clipRunes(singleLine(f), maxPathRunes); path != "" {
			out = append(out, path)
		}
	}
	return out
}

// yesNo renders a bool in the prompt's vocabulary.
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

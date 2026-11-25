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
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// DefaultRenderBudgetTokens is the Appendix E snapshot budget
// (docs/supervisor-policy-design.md §4.3): the paper's whole meta-agent
// prompt stays under ~3K tokens, so the rendered snapshot must too.
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
const (
	maxArgRunes         = 160
	maxErrorRunes       = 100
	maxNoteRunes        = 200
	maxFilesRendered    = 8
	maxInFlightRendered = 5
	trailingNewline     = "\n"
	emptyFeatureValue   = "(none)"
)

// footerJSON is the return instruction the locked v1 prompt's protocol
// expects. yaah's action vocabulary is none|steer|redirect|revert; the
// paper's inject/handoff/discard map onto steer/redirect/revert.
const footerJSON = `Return JSON: {"decisions": [{"agent": "...", "action": "none"|"steer"|"redirect"|"revert", "reason": "...", "guidance": "..."}]}`

// TickState is batch-owned state the snapshot does not carry: the
// meta-agent's last action on a scope and its age in ticks. The Batch that
// owns it lands in yaah-3l34; until then a caller passes whatever it tracks,
// and a missing entry (or a zero value) means "never acted".
type TickState struct {
	// LastAction is none|steer|redirect|revert — the action of the last
	// intervention on this scope, empty or "none" when there was none.
	LastAction string
	// TicksAgo is the number of ticks since that intervention.
	TicksAgo int
}

// RenderOptions is the per-call context the snapshot cannot supply.
type RenderOptions struct {
	// Tick is the tick number rendered in the header. 0 renders the
	// header without a tick number.
	Tick int
	// BudgetTokens bounds the estimated size of the DEGRADABLE part of the
	// text. 0 means DefaultRenderBudgetTokens. There is a floor the budget
	// cannot squeeze past: the header and the return instruction are
	// irreducible (~65 estimated tokens between them), so a budget at or
	// below that floor still renders non-empty text — header, omitted-agents
	// footer, and instruction — rather than nothing. Termination is
	// guaranteed at every budget.
	BudgetTokens int
	// State is the batch's per-scope tick state, keyed by ScopeID. Nil is
	// fine and renders every scope as last_action=none.
	State map[string]TickState
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

// limits are the degradation knobs one render attempt is made under.
type limits struct {
	// trail is the maximum number of trailing tool calls rendered per
	// agent; -1 renders the whole (already capped) trail, 0 omits the
	// section entirely.
	trail int
	// hideFinishedHeader drops the role/running line of a finished
	// agent — it is redundant for one that has stopped.
	hideFinishedHeader bool
	// omitted counts the agents cut from this attempt, for the honest
	// footer.
	omitted int
}

// Render renders snap into meta-agent prompt text and returns the label map
// the text used.
//
// The returned labels cover EVERY agent in the snapshot, not just the ones
// that survived budget degradation: the assignment is a pure function of the
// snapshot's scope set (sorted by ScopeID ascending, w1..wN), so a label
// never moves between ticks because a neighbour finished or was omitted.
// Renumbering under budget pressure would silently re-point a decision.
//
// Budget degradation is tiered, cheapest information loss first, and each
// tier is re-rendered until the estimate fits (see the tier comment on the
// Render body).
func Render(snap supervisionapi.Snapshot, opts RenderOptions) (text string, labels []Labeled) {
	budget := opts.BudgetTokens
	if budget <= 0 {
		budget = DefaultRenderBudgetTokens
	}
	labels, ordered := labelAgents(snap.Agents)
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

	// Tier 1 — shorten each agent's trail from the OLDEST end (the newest
	// calls are what a supervisor acts on). No tier trims any other field:
	// the file list, the in-flight list, and the note are capped
	// unconditionally in writeBlock, so there is nothing left to trade.
	for _, trail := range []int{-1, 12, 6, 3, 1, 0} {
		if txt := r.render(ordered, limits{trail: trail}); estimateTokens(txt) <= budget {
			return txt, labels
		}
	}
	// Tier 2/3 — drop the role/running line of finished agents, then drop
	// their TRAIL too (trail 0), while still naming them and keeping their
	// feature, stats, file list, and note. Only the trail goes: the block
	// itself stays.
	for _, trail := range []int{1, 0} {
		lim := limits{trail: trail, hideFinishedHeader: true}
		if txt := r.render(ordered, lim); estimateTokens(txt) <= budget {
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
		lim := limits{trail: 0, hideFinishedHeader: true, omitted: len(ordered) - n}
		txt := r.render(ordered[:n], lim)
		if estimateTokens(txt) <= budget || n == 0 {
			return txt, labels
		}
	}
	// No agents at all: the header and footer are all that is left, and
	// they always fit.
	return r.render(nil, limits{hideFinishedHeader: true}), labels
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
	}

	// The note is disclosure, not decoration: an agent whose trace could not
	// be read must not render byte-identical to a quiet one. It sits in the
	// kept header/stats section rather than in the trail, so every
	// degradation tier that still shows the agent still shows why its facts
	// are thin.
	if note := clipRunes(singleLine(s.Note), maxNoteRunes); note != "" {
		fmt.Fprintf(b, "note: %s\n", note)
	}

	feature := clipRunes(singleLine(s.Feature), supervisionapi.MaxFeatureRunes)
	if feature == "" {
		feature = emptyFeatureValue
	}
	fmt.Fprintf(b, "feature: %s\n", feature)

	st := r.state[s.ScopeID]
	lastAction := st.LastAction
	if lastAction == "" {
		lastAction = "none"
	}
	fmt.Fprintf(b, "events=%d consecutive_errors=%d last_action=%s",
		s.Calls, s.ConsecutiveErrors, lastAction)
	if lastAction != "none" {
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
	if len(s.ModifiedFiles) == 0 {
		b.WriteString("modified_files: (none observed)\n")
	} else if files := s.ModifiedFiles; len(files) > maxFilesRendered {
		// Say what was cut, so a long list is never mistaken for the
		// whole set.
		fmt.Fprintf(b, "modified_files: %s (+%d more)\n",
			strings.Join(files[:maxFilesRendered], ", "), len(files)-maxFilesRendered)
	} else {
		fmt.Fprintf(b, "modified_files: %s\n", strings.Join(s.ModifiedFiles, ", "))
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

// labelAgents assigns w1..wN by ScopeID ascending over a copy of agents, and
// returns the render order: live agents by ScopeID ascending, then finished
// agents newest-first.
//
// Label assignment deliberately ignores the caller's slice order — the
// snapshot builder emits live registrations sorted by ScopeID and then
// finished ones newest-first, so a positional scheme would renumber every
// agent the moment a neighbour finished. (Pinning a decision to a particular
// run incarnation is not this package's job: the snapshot type carries no
// generation token, and the arbiter resolves a claimed label against the
// registry's generation at claim time — yaah-3l34 / yaah-fy28.)
func labelAgents(agents []supervisionapi.AgentSnapshot) ([]Labeled, []labeledAgent) {
	byID := make([]supervisionapi.AgentSnapshot, len(agents))
	copy(byID, agents)
	sort.SliceStable(byID, func(i, j int) bool { return byID[i].ScopeID < byID[j].ScopeID })

	labels := make([]Labeled, 0, len(byID))
	labelOf := make(map[string]string, len(byID))
	for i, a := range byID {
		label := fmt.Sprintf("w%d", i+1)
		labels = append(labels, Labeled{Label: label, ScopeID: a.ScopeID})
		labelOf[a.ScopeID] = label
	}

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
	if max <= 0 || len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…" // guard max-1 == 0
	}
	return string(runes[:max-1]) + "…"
}

// yesNo renders a bool in the prompt's vocabulary.
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

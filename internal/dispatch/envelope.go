package dispatch

// envelope.go builds the ONE typed result envelope every dispatch lane returns
// (yaah-was9.1). There is deliberately no parallel result type: proposalEnvelope
// — the shape the proposal lane and the settlement verbs already used — is
// extended with the dispatch identity and runner facts, so a proposal, a report,
// a shared dispatch, a failure, and a background completion all parse the same
// way. A lane that gains a fact adds it here, once.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/jobs"
)

// Envelope status values. The set is closed: a lane that cannot describe its
// outcome with one of these has found a new kind of completion, not a new word
// for one. The proposal lane keeps its own settlement vocabulary on top
// (settled, open, conflict), which is a verdict on a proposal rather than a
// dispatch outcome.
const (
	// EnvelopeStatusProposal: an isolated world was sealed and awaits a
	// verdict from the proposal tool.
	EnvelopeStatusProposal = "proposal"
	// EnvelopeStatusReport: an isolated report dispatch finished; the result
	// is the text, and nothing is left to settle.
	EnvelopeStatusReport = "report"
	// EnvelopeStatusShared: a dispatch ran directly on the shared workspace,
	// so whatever it wrote is already applied.
	EnvelopeStatusShared = "shared"
	// EnvelopeStatusRunning: a background dispatch was accepted; the work
	// arrives later as its own completion envelope.
	EnvelopeStatusRunning = "running"
	// EnvelopeStatusCompleted: a background dispatch finished and its result
	// is delivered in `result` (yaah-was9.1).
	EnvelopeStatusCompleted = "completed"
	// EnvelopeStatusError: the dispatch did not complete. `error` says why,
	// `stop_reason` names the kind where one is known, and `recovery` says
	// whether re-dispatching is worth it.
	EnvelopeStatusError = "error"
)

// ResultContract is RESERVED for yaah-was9.2: the per-role completion contract a
// sub-agent result is checked against. Nothing populates it yet, so the field is
// absent from every envelope today. It is present in the type (and documented)
// so the child lands a field rather than a shape change, and so no caller reads
// its absence as "checked and passed".
type ResultContract struct {
	// Name is the contract the result is checked against, usually the role.
	Name string `json:"name,omitempty"`
	// Status is the contract's verdict: pass, fail, or incomplete.
	Status string `json:"status,omitempty"`
}

// ResultArtifacts is the nested view of the work a dispatch produced. It
// duplicates the envelope's top-level proposal_id/files/diff on purpose: those
// flat fields are the settlement contract the proposal tool and the model
// already read, and a nested block is where a lane can add artifacts without
// widening that contract.
type ResultArtifacts struct {
	ProposalID string   `json:"proposal_id,omitempty"`
	Files      []string `json:"files,omitempty"`
	Diff       string   `json:"diff,omitempty"`
}

// ResultRecovery answers the question every caller asks of a dispatch that did
// not simply complete: is re-running it worth anything, and what should the next
// call look like? It is derived from stop_reason, never guessed from prose.
type ResultRecovery struct {
	// Retryable reports that dispatching again (or continuing an open
	// proposal) can make progress. False does not mean "fatal" — it means the
	// stop was deliberate (completed, halted) or already final.
	Retryable bool `json:"retryable"`
	// Recommended is short next-step guidance for a retryable stop.
	Recommended string `json:"recommended,omitempty"`
}

// EnvelopeMeta is the dispatch identity and runner facts every completion
// envelope carries. It is populated from what the dispatch lane actually knows:
// a value the lane cannot observe is left empty and therefore omitted, never
// fabricated.
type EnvelopeMeta struct {
	// DispatchID is the per-dispatch id the conflict tracker minted
	// (label#n), or the background job id when the delivery has no other.
	DispatchID string
	// Role is the sub-agent role that ran.
	Role string
	// Lane is the dispatch lane: proposal, report, or shared.
	Lane string
	// World is the world the dispatch actually ran in: worktree, container,
	// or shared.
	World string
	// Model is the model the runner resolved for the child.
	Model string
	// Duration is the wall-clock span of the dispatch.
	Duration time.Duration
}

// after returns m with the elapsed dispatch time recorded.
func (m EnvelopeMeta) after(d time.Duration) EnvelopeMeta {
	m.Duration = d
	return m
}

// newEnvelope starts a completion envelope with the identity and result every
// lane shares.
func (m EnvelopeMeta) newEnvelope(status, result string) ProposalEnvelope {
	env := proposalEnvelope{
		Status:     status,
		Result:     result,
		DispatchID: m.DispatchID,
		Role:       m.Role,
		Lane:       m.Lane,
		World:      m.World,
		Model:      m.Model,
	}
	if m.Duration > 0 {
		env.DurationMs = m.Duration.Milliseconds()
	}
	return env
}

// FillMeta stamps the dispatch identity onto an envelope that a lane built
// itself, leaving any value the lane already knew in place. It is the join
// between a lane's own envelope (the retained proposal lane marshals its own)
// and the dispatch-level facts resolved in Run.
func (e *proposalEnvelope) FillMeta(m EnvelopeMeta) {
	if e.DispatchID == "" {
		e.DispatchID = m.DispatchID
	}
	if e.Role == "" {
		e.Role = m.Role
	}
	if e.Lane == "" {
		e.Lane = m.Lane
	}
	if e.World == "" {
		e.World = m.World
	}
	if e.Model == "" {
		e.Model = m.Model
	}
	if e.DurationMs == 0 && m.Duration > 0 {
		e.DurationMs = m.Duration.Milliseconds()
	}
}

// AddWarning records a caveat in the typed warnings list and, for the single
// line every existing reader looks at, in Warning. A dispatch can degrade in
// more than one way (world degradation plus a teardown note), which is why the
// list exists; the scalar keeps the historical field populated.
func (e *proposalEnvelope) AddWarning(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	e.Warnings = append(e.Warnings, msg)
	if e.Warning == "" {
		e.Warning = msg
	}
}

// SetProposalArtifacts mirrors the envelope's proposal_id/files/diff into the
// nested artifacts block. The flat fields stay exactly as they were: they are
// what the proposal tool and the model settle on.
func (e *proposalEnvelope) SetProposalArtifacts() {
	if e.ProposalID == "" && len(e.Files) == 0 && e.Diff == "" {
		return
	}
	e.Artifacts = &ResultArtifacts{ProposalID: e.ProposalID, Files: e.Files, Diff: e.Diff}
}

// SetRecovery derives the recovery block from stop_reason when the lane has not
// already set one.
func (e *proposalEnvelope) SetRecovery() {
	if e.Recovery == nil {
		e.Recovery = RecoveryFor(e.StopReason)
	}
}

// SetFailureClass derives the dispatch-level failure classification from the
// status and stop_reason the envelope already carries, when the lane has not set
// one itself. Deriving it here rather than at each call site is what keeps it
// from contradicting the fields a reader checks it against.
func (e *proposalEnvelope) SetFailureClass() {
	if e.FailureClass != "" {
		return
	}
	e.FailureClass = FailureClass(FailureInput{Status: e.Status, StopReason: e.StopReason})
}

// SetAttempts fills the attempt counters from the ids the envelope carries: an
// envelope always reports at least one attempt, and names one attempt id — the
// dispatch id when the lane had one, else the proposal id — so a reader can
// correlate the attempt with the conflict evidence. A retried dispatch
// (yaah-dgkh) appends the further ids and the counters follow.
func (e *proposalEnvelope) SetAttempts() {
	if len(e.AttemptIDs) == 0 {
		if id := e.attemptID(); id != "" {
			e.AttemptIDs = []string{id}
		}
	}
	if e.Attempts < len(e.AttemptIDs) {
		e.Attempts = len(e.AttemptIDs)
	}
	if e.Attempts < 1 {
		e.Attempts = 1
	}
	e.Retried = e.Attempts > 1
}

// applyAttempts stamps the retry loop's bookkeeping onto an envelope before it
// is finalized (yaah-dgkh). A result that never entered the loop carries no
// attempt number and leaves the derives-from-ids behavior of SetAttempts alone,
// so a single-attempt dispatch renders exactly as it did before the loop
// existed.
func (e *proposalEnvelope) applyAttempts(attempt int, ids []string) {
	if attempt < 1 {
		return
	}
	if len(ids) > 0 {
		e.AttemptIDs = ids
	}
	e.Attempts = attempt
}

// attemptID is the id that names this envelope's attempt: the dispatch id the
// conflict tracker minted, or failing that the proposal the attempt produced.
func (e *proposalEnvelope) attemptID() string {
	if e.DispatchID != "" {
		return e.DispatchID
	}
	return e.ProposalID
}

// finalize fills the blocks every dispatch-completion envelope must carry, so no
// lane has to remember all of them. Settlement envelopes call it too: the
// recovery block is nil-safe there because a settlement's stop_reason is empty,
// and an empty stop_reason on a non-error status classifies as no failure.
func (e *proposalEnvelope) finalize() {
	e.SetAttempts()
	e.SetProposalArtifacts()
	e.SetRecovery()
	e.SetFailureClass()
}

// RecoveryFor derives retryable/recommended from a stop_reason. Budget and
// deadline stops are retryable through continue/fork (the sealed work survives);
// a cancelled or provider-errored run is worth re-dispatching; completed and
// halted runs are not automatically repeatable.
func RecoveryFor(stopReason string) *ResultRecovery {
	r := &ResultRecovery{}
	switch stopReason {
	case "turn_budget", "iteration_budget":
		r.Retryable = true
		r.Recommended = "the run stopped at its turn/iteration budget with reviewable work: continue or fork the proposal for more turns, or dispatch again with a larger max_iterations/max_turns"
	case "max_tokens":
		r.Retryable = true
		r.Recommended = "the provider truncated the final answer at max_tokens: continue or fork the proposal, or dispatch again with a smaller output_limit and a tighter ask"
	case "timeout":
		r.Retryable = true
		r.Recommended = "the run hit its wall-clock budget: dispatch again with a larger timeout_seconds, or continue or fork the proposal"
	case "stuck":
		r.Retryable = true
		r.Recommended = "the child went silent and the stuck-child watchdog killed it: dispatch again — a repeat points at the provider or the prompt"
	case "canceled":
		r.Retryable = true
		r.Recommended = "the dispatch was cancelled (turn cancelled, or the parent deadline expired): dispatch again if the work is still wanted"
	case "provider_error":
		r.Retryable = true
		r.Recommended = "a provider error ended the run: dispatch again — the provider-level retries are already exhausted by the time this is reported"
	case "halted":
		r.Recommended = "the supervisor halted the run: inspect it with the supervisor tool before re-dispatching"
	}
	return r
}

// failureRecovery is the recovery block for an error-status envelope. A failed
// dispatch is worth re-running unless the stop reason says the run was
// deliberately ended, so an unclassified failure defaults to retryable with a
// pointer at the error text rather than staying silent.
func failureRecovery(stopReason string) *ResultRecovery {
	r := RecoveryFor(stopReason)
	if stopReason == "" {
		r.Retryable = true
		r.Recommended = "the dispatch ended with an error: read `error` and any partial `result`, then dispatch again or continue an open proposal"
	}
	return r
}

// stopReasonForStatus maps the typed dispatch status onto the envelope's
// stop_reason vocabulary. A failed dispatch carries no reason the core can name
// (the cause is arbitrary), so it stays empty rather than claiming
// provider_error.
func stopReasonForStatus(status DispatchStatus) string {
	switch status {
	case DispatchCompleted:
		return "completed"
	case DispatchTimedOut:
		return "timeout"
	case DispatchStuck:
		return "stuck"
	case DispatchCancelled:
		return "canceled"
	default:
		return ""
	}
}

// LaneStatus maps a lane onto the status a completed dispatch on it reports. An
// unset lane is a report: the result is text and nothing is left to settle.
func LaneStatus(lane string) string {
	switch Lane(lane) {
	case LaneProposal:
		return EnvelopeStatusProposal
	case LaneShared:
		return EnvelopeStatusShared
	default:
		return EnvelopeStatusReport
	}
}

// withMeta stamps the dispatch-level facts onto a result: the result carries the
// metadata for a caller that renders later, and a proposal envelope the lane
// built itself is filled in without overwriting what it knew.
func (r DispatchResult) withMeta(m EnvelopeMeta) DispatchResult {
	r.Meta = m
	if r.Proposal != nil {
		r.Proposal.FillMeta(m)
	}
	return r
}

// withAttempts stamps the retry loop's bookkeeping onto a result (yaah-dgkh):
// which attempt this is and every attempt id the dispatch has produced so far,
// in order. The count and the ids are what both the attempt fact
// (attempt_evidence.go) and the final envelope read, so they can never disagree.
func (r DispatchResult) withAttempts(attempt int, ids []string) DispatchResult {
	r.Attempt = attempt
	r.AttemptIDs = append([]string(nil), ids...)
	return r
}

// aggregate turns the last attempt's result into the whole sequence's: the
// attempt counters and ids already cover every attempt, and the duration becomes
// the dispatch's total wall clock instead of the final attempt's span.
func (r DispatchResult) aggregate(elapsed time.Duration) DispatchResult {
	r.Meta = r.Meta.after(elapsed)
	if r.Proposal != nil {
		r.Proposal.DurationMs = elapsed.Milliseconds()
	}
	return r
}

// Envelope renders the typed result in the one model-facing completion shape.
// Every lane goes through it: report and shared lanes for their status, failure
// lanes for status=error plus recovery, and the proposal lane for the metadata
// its own envelope does not know.
func (r DispatchResult) Envelope() ProposalEnvelope {
	meta := r.Meta
	if meta.Role == "" {
		meta.Role = r.Role
	}
	switch r.Status {
	case DispatchProposal:
		if r.Proposal != nil {
			env := *r.Proposal
			env.FillMeta(meta)
			env.applyAttempts(r.Attempt, r.AttemptIDs)
			env.finalize()
			return env
		}
		env := meta.newEnvelope(EnvelopeStatusProposal, r.Result)
		env.applyAttempts(r.Attempt, r.AttemptIDs)
		env.finalize()
		return env
	case DispatchRunning:
		return meta.newEnvelope(EnvelopeStatusRunning, "")
	case DispatchCompleted:
		env := meta.newEnvelope(LaneStatus(meta.Lane), r.Result)
		env.StopReason = stopReasonForStatus(DispatchCompleted)
		env.applyAttempts(r.Attempt, r.AttemptIDs)
		env.finalize()
		return env
	}

	// Failure lanes: status=error. The partial body is the lane's own text and
	// is preserved in result; the error carries the untruncated cause.
	env := meta.newEnvelope(EnvelopeStatusError, r.Partial)
	env.StopReason = stopReasonForStatus(r.Status)
	env.Error = errorString(r.Err)
	env.Partial = strings.TrimSpace(r.Partial) != ""
	if r.Timeout > 0 {
		env.Timeout = r.Timeout.String()
	}
	// A lane that knows more than the stop reason says — a retained start that
	// failed before its world ran — names the failure class itself.
	env.FailureClass = r.FailureClass
	env.Recovery = failureRecovery(env.StopReason)
	env.applyAttempts(r.Attempt, r.AttemptIDs)
	env.finalize()
	return env
}

// ParseEnvelopeResult reparses a lane's already-rendered result as an envelope.
// The retained lanes render their own envelope to text (the background job
// contract carries a string), so a delivery that only has the final text must
// not wrap it a second time. RESERVED for yaah-was9.2 (response-contract
// validation), which must reparse a delivered envelope to check its fields;
// there is no production caller yet.
func ParseEnvelopeResult(text string) (ProposalEnvelope, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return ProposalEnvelope{}, false
	}
	var env proposalEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil || env.Status == "" {
		return ProposalEnvelope{}, false
	}
	return env, true
}

// EnvelopeResultText returns the raw result text an envelope carries, or the
// input unchanged when it is not an envelope. RESERVED for yaah-was9.2:
// consumers that must read a dispatch's prose out of an already-rendered
// envelope (contract validation, quality-gate verdicts) will use it so the
// envelope does not hide the model's words behind JSON escaping. There is no
// production caller yet.
func EnvelopeResultText(text string) string {
	env, ok := ParseEnvelopeResult(text)
	if !ok {
		return text
	}
	return env.Result
}

// BackgroundCompletionEnvelope renders the one completion envelope for a
// background dispatch from its terminal job snapshot and raw result. It is the
// single place that maps a background job's terminal status onto the envelope
// vocabulary, so the foreground and background lanes agree (yaah-was9.1).
func BackgroundCompletionEnvelope(st jobs.BackgroundJobStatus, result string, err error) ProposalEnvelope {
	env := proposalEnvelope{
		Status:     EnvelopeStatusCompleted,
		DispatchID: st.ID,
		Label:      st.Description,
		Role:       st.Role,
		Result:     result,
		Model:      st.Model,
		World:      st.World,
		DurationMs: st.DurationMs,
	}
	switch st.Status {
	case jobs.BGStatusFailed:
		env.Status = EnvelopeStatusError
		env.Partial = strings.TrimSpace(result) != ""
		if err != nil {
			env.Error = err.Error()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			env.StopReason = "timeout"
		}
	case jobs.BGStatusCancelled:
		env.Status = EnvelopeStatusError
		env.StopReason = "canceled"
		env.Partial = strings.TrimSpace(result) != ""
		if err != nil {
			env.Error = err.Error()
		}
	default:
		env.StopReason = "completed"
	}
	// A background dispatch is one attempt, and its job id names it (yaah-i5k8).
	env.SetAttempts()
	if env.Status == EnvelopeStatusError {
		env.Recovery = failureRecovery(env.StopReason)
	}
	env.SetFailureClass()
	return env
}

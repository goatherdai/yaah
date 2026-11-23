package exec

import (
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/trace"
)

// trace.go is the run's trace projection (yaah-0u0v.17): the one seam that
// turns a reducer event into the typed records the trace surface reads.
//
// The projection is deliberately NOT a second bookkeeping path. It runs inside
// commit, immediately after the event's canonical log line is appended, and it
// reads only:
//
//   - the event itself (every fact the driver reported),
//   - the immutable graph node the event names (id, kind, declared lane),
//   - the container a fan-out instance was minted by (its scope id),
//   - the output bindings the producing work already recorded.
//
// It reads no lifecycle state that the fold would change, which is why it can
// run BEFORE the switch: a record is a statement about the event, not about the
// state the event is about to produce, and ordering the two that way keeps the
// record sequence identical to the event sequence.
//
// # Derivation table
//
//	record kind        reducer event(s)                  which fields
//	workflow.started   run_started, run_resumed          status (started|resumed)
//	node.started       node_ready                        node id/kind/scope
//	dispatch           dispatch_started                  attempt, dispatch id,
//	                                                     lane, status=started
//	dispatch           attempt_completed                 attempt, dispatch id,
//	                                                     lane, envelope status,
//	                                                     stop reason (reason),
//	                                                     result, output digest,
//	                                                     error body, retryable
//	                                                     assert (yaah-0u0v.25)
//	                   attempt_timed_out                 attempt, dispatch id,
//	                                                     lane, status=error,
//	                                                     reason=timeout, and the
//	                                                     retryable assert — the
//	                                                     same shape a lane
//	                                                     timeout records, so a
//	                                                     replay re-derives it
//	proposal           attempt_completed w/ proposal id  proposal id, attempt
//	settle             settlement_asked                  proposal id, action,
//	                                                     status=asked
//	settle             proposal_settled                  proposal id, action,
//	                                                     door status, reason
//	settle             abort_asked, abort_settled        proposal id, action,
//	                                                     door status, reason
//	                                                     (no node: the record
//	                                                     is the run's own)
//	node.finished      node_succeeded                    status, output digest
//	                   node_failed, node_skipped,        status, reason,
//	                   node_cancelled                    output digest; a
//	                                                     call node's record
//	                                                     also names the child
//	                                                     run (child_run_id)
//	workflow.ended     run_finished                      run status, reason
//
// Events that project no record are the ones whose consequence is already
// recorded by the records above: settlement_planned, fanout_expanded,
// loop_iteration_started, loop_completed, branch_routed, and aggregate_inputs
// are inputs the reducer turns into instances (whose node.started /
// node.finished records state the outcome), retry_scheduled is stated by the
// next dispatch record's attempt number, and budget_exceeded reaches the trace
// as the workflow.ended record's reason.

// recordTrace projects one event and appends its records to the run's trace. It
// is called from commit, after the event's log line is written, so
// Record.EventSeq and the log's sequence number are the same number.
func (l *traceLedger) recordTrace(ev Event, seq int, v traceView) {
	for _, rec := range l.traceRecords(v, ev) {
		rec.Seq = len(l.records) + 1
		rec.EventSeq = seq
		if rec.RunID == "" {
			rec.RunID = v.runID
		}
		if rec.SpecHash == "" {
			rec.SpecHash = v.specHash
		}
		l.records = append(l.records, rec)
	}
}

// traceRecords is the projection proper: a pure function of the event and the
// graph facts the run already holds.
func (l *traceLedger) traceRecords(v traceView, ev Event) []trace.Record {
	switch e := ev.(type) {
	case RunStartedEvent:
		return []trace.Record{{
			Kind:   trace.KindRunStarted,
			RunID:  l.traceRunID(v, e.RunID),
			Status: trace.StatusStarted,
		}}
	case RunResumedEvent:
		// A resume is the run entering execution again. The vocabulary has one
		// word for "this run's execution started here", so it is recorded as
		// workflow.started with the resume status rather than as an eighth kind
		// nobody documented.
		return []trace.Record{{
			Kind:   trace.KindRunStarted,
			RunID:  l.traceRunID(v, e.RunID),
			Status: trace.StatusResumed,
		}}
	case NodeReadyEvent:
		return []trace.Record{l.nodeRecord(v, trace.KindNodeStarted, e.NodePath, trace.StatusStarted)}
	case DispatchStartedEvent:
		if e.Attempt == nil {
			return nil
		}
		rec := l.nodeRecord(v, trace.KindDispatch, e.Attempt.NodePath, trace.StatusStarted)
		rec.Attempt = e.Attempt.Number
		rec.DispatchID = e.Attempt.DispatchID
		rec.Lane = l.laneOf(v, e.Attempt)
		return []trace.Record{rec}
	case AttemptCompletedEvent:
		return l.attemptRecords(v, e)
	case AttemptTimedOutEvent:
		return []trace.Record{l.timeoutRecord(v, e)}
	case NodeSucceededEvent:
		rec := l.nodeRecord(v, trace.KindNodeFinished, e.NodePath, trace.StatusSucceeded)
		rec.OutputDigest = l.outputDigest(v, e.NodePath)
		rec = l.markCallChild(v, e.NodePath, rec)
		return []trace.Record{rec}
	case NodeFailedEvent:
		rec := l.nodeRecord(v, trace.KindNodeFinished, e.NodePath, trace.StatusFailed)
		rec.Reason = e.Reason
		rec.OutputDigest = l.outputDigest(v, e.NodePath)
		rec = l.markCallChild(v, e.NodePath, rec)
		return []trace.Record{rec}
	case NodeSkippedEvent:
		rec := l.nodeRecord(v, trace.KindNodeFinished, e.NodePath, trace.StatusSkipped)
		rec.Reason = e.Reason
		return []trace.Record{rec}
	case NodeCancelledEvent:
		rec := l.nodeRecord(v, trace.KindNodeFinished, e.NodePath, trace.StatusCancelled)
		rec.Reason = e.Reason
		rec.DispatchID = e.DispatchID
		if e.CancelErr != "" {
			// A cancel that failed is a fact about the world, not a state
			// change, and the node's own reason already says why it ended. Both
			// go on the one line so the record is not a trimmed copy of the
			// node's state.
			rec.Reason = rec.Reason + " (cancel failed: " + e.CancelErr + ")"
		}
		rec.OutputDigest = l.outputDigest(v, e.NodePath)
		return []trace.Record{rec}
	case SettlementAskedEvent:
		rec := l.nodeRecord(v, trace.KindSettle, e.NodePath, trace.StatusAsked)
		rec.ProposalID = e.ProposalID
		rec.Action = e.Action
		return []trace.Record{rec}
	case ProposalSettledEvent:
		rec := l.nodeRecord(v, trace.KindSettle, e.NodePath, e.Settled.Status)
		rec.ProposalID = e.Settled.ProposalID
		rec.Action = e.Settled.Action
		rec.Reason = settledReason(e)
		rec.Result = e.Settled.Outcome
		rec.Files = e.Settled.Files
		if e.Settled.Outcome != "" {
			rec.OutputDigest = trace.Digest(e.Settled.Outcome)
		}
		return []trace.Record{rec}
	case AbortAskedEvent:
		// The abort pass owns no node, so the record is built directly:
		// identity is the run and the proposal, and the reason carries the
		// policy that caused the ask so a reader can tell these settled
		// records from a settle node's at a glance (yaah-0u0v.24).
		return []trace.Record{{
			Kind:       trace.KindSettle,
			Status:     trace.StatusAsked,
			ProposalID: e.ProposalID,
			Action:     e.Action,
			Reason:     "on_abort",
		}}
	case AbortSettledEvent:
		reason := "on_abort"
		if cause := firstSet(e.DoorError, e.Settled.Error); cause != "" {
			reason += ": " + flatten(cause)
		}
		rec := trace.Record{
			Kind:       trace.KindSettle,
			Status:     e.Settled.Status,
			ProposalID: e.Settled.ProposalID,
			Action:     e.Settled.Action,
			Reason:     reason,
			Result:     e.Settled.Outcome,
		}
		if e.Settled.Outcome != "" {
			rec.OutputDigest = trace.Digest(e.Settled.Outcome)
		}
		return []trace.Record{rec}
	case RunFinishedEvent:
		return []trace.Record{{
			Kind:   trace.KindRunEnded,
			Status: string(e.Status),
			Reason: e.Reason,
		}}
	}
	return nil
}

// attemptRecords projects one completion: the dispatch's own record, plus the
// proposal record when the attempt sealed a handle.
func (l *traceLedger) attemptRecords(v traceView, e AttemptCompletedEvent) []trace.Record {
	c := e.Completion
	status := c.Status
	if status == "" {
		// A door malfunction carries no envelope, so there is no status to
		// quote. It is not a completion the run accepted, and the record says
		// so with the one word the vocabulary has for it — for the dispatch and
		// for any proposal it tried to seal alike.
		status = trace.StatusRuntimeError
	}
	rec := l.nodeRecord(v, trace.KindDispatch, e.NodePath, status)
	rec.Attempt = e.Attempt
	rec.DispatchID = e.DispatchID
	rec.Lane = l.laneOfCompletion(v, e.NodePath, c.Lane)
	rec.Reason = firstSet(e.RuntimeError, c.StopReason, c.Error)
	rec.Result = c.Result
	rec.OutputDigest = trace.Digest(c.Result)
	// The error body and the recovery assert are what the fold reads after
	// Status: a node's failure reason quotes both, and a retry decision is
	// exactly the difference between "asserted retryable" and "asserted
	// nothing". They ride the dispatch record so a replay (yaah-0u0v.25)
	// re-derives the run's own sequence — not one that fails where the
	// original retried.
	if c.Error != "" {
		rec.Error = c.Error
	}
	if c.Recovery != nil {
		retryable := c.Recovery.Retryable
		rec.Retryable = &retryable
	}
	out := []trace.Record{rec}
	if c.ProposalID != "" {
		p := l.nodeRecord(v, trace.KindProposal, e.NodePath, status)
		p.Attempt = e.Attempt
		p.DispatchID = e.DispatchID
		p.Lane = rec.Lane
		p.ProposalID = c.ProposalID
		p.OutputDigest = rec.OutputDigest
		out = append(out, p)
	}
	return out
}

// timeoutRecord projects a scheduler-concluded timeout as the dispatch record a
// lane-reported timeout would have produced: status=error, reason=timeout, and
// the retryable assert a retry decision reads. The two are deliberately
// indistinguishable here, because they are the same fact about the attempt —
// which side of the door enforced the deadline is the log's own word
// (attempt_timed_out), not the record's.
//
// The shape is also what makes a replay re-derive the run: the replay door
// answers a dispatch from exactly these fields (Status, Reason, Retryable), so a
// timed-out attempt replays as the same failure — retryable or not — it was
// folded as.
func (l *traceLedger) timeoutRecord(v traceView, e AttemptTimedOutEvent) trace.Record {
	rec := l.nodeRecord(v, trace.KindDispatch, e.NodePath, StatusError)
	rec.Attempt = e.Attempt
	rec.DispatchID = e.DispatchID
	rec.Reason = StopReasonTimeout
	// A failed Cancel makes the timeout terminal, not retryable: the old work
	// may still be running, so the record must not invite a re-dispatch.
	retryable := e.CancelErr == ""
	rec.Retryable = &retryable
	return rec
}

// nodeRecord builds a record about one node, filling the identity facts the
// graph and the instance table already hold.
func (l *traceLedger) nodeRecord(v traceView, kind trace.Kind, path, status string) trace.Record {
	rec := trace.Record{Kind: kind, NodePath: path, Status: status}
	if n := v.byPath[path]; n != nil {
		rec.NodeID = n.ID
		rec.NodeKind = string(n.Kind)
		rec.Lane = string(n.Lane)
	}
	if inst := v.instances[path]; inst != nil {
		rec.ScopeID = inst.owner
	}
	return rec
}

// markCallChild points a call node's finished record at the child run's own
// records (yaah-0u0v.25, the .23 residue): the id is derived from the parent's
// run id, so the parent's trace can name the child without a second identity
// scheme. It reads CallRunID, which callStep records when the door answers —
// before the terminal event the record projects — so this still observes no
// lifecycle state the fold of THAT event is about to change.
func (l *traceLedger) markCallChild(v traceView, path string, rec trace.Record) trace.Record {
	if n := v.byPath[path]; n == nil || n.Kind != ir.KindCall {
		return rec
	}
	if st := v.state[path]; st != nil {
		rec.ChildRunID = st.CallRunID
	}
	return rec
}

// traceRunID prefers the id the event names, because a run_started may carry the
// id the caller minted, and falls back to the run's own.
func (l *traceLedger) traceRunID(v traceView, fromEvent string) string {
	if fromEvent != "" {
		return fromEvent
	}
	return v.runID
}

// laneOf echoes the attempt's declared lane, falling back to the graph's.
func (l *traceLedger) laneOf(v traceView, a *Attempt) string {
	if a == nil {
		return ""
	}
	if a.Lane != "" {
		return a.Lane
	}
	if n := v.byPath[a.NodePath]; n != nil {
		return string(n.Lane)
	}
	return ""
}

// laneOfCompletion reads the lane from the completion, then the graph: a lane
// that reported its own lane is quoted, and one that stayed silent states what
// the document declared.
func (l *traceLedger) laneOfCompletion(v traceView, path, lane string) string {
	if lane != "" {
		return lane
	}
	if n := v.byPath[path]; n != nil {
		return string(n.Lane)
	}
	return ""
}

// outputDigest is the content address of what a node produced: its last
// attempt's recorded result when it has one, and otherwise the recorded output
// bindings joined in declaration order (an emit or a settle records a value
// without dispatching anything).
func (l *traceLedger) outputDigest(v traceView, path string) string {
	st := v.state[path]
	if st == nil {
		return ""
	}
	if a := st.LastAttempt(); a != nil && a.Result != "" {
		return trace.Digest(a.Result)
	}
	if len(st.Outputs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, o := range st.Outputs {
		b.WriteString(o.Name)
		b.WriteString("\x00")
		b.WriteString(o.Value)
		b.WriteString("\x00")
	}
	return trace.Digest(b.String())
}

// settledReason states why a settlement ended the way it did: a disposition the
// scheduler refused to ask for, a door that malfunctioned, or the error a
// completed disposition reported.
func settledReason(e ProposalSettledEvent) string {
	if e.Refused != "" {
		return e.Refused
	}
	if e.DoorError != "" {
		return e.DoorError
	}
	return e.Settled.Error
}

// firstSet returns the first non-empty value, so a reason quotes the most
// specific cause available without the caller writing a nested if.
func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// traceViewOf is the view one record is projected from: the graph, the state,
// the instance relation, and the run identity a record defaults to. Every field
// is read-only here, which is what keeps the projection off the reducer's state.
func traceViewOf(r *Run) traceView {
	return traceView{
		byPath:    r.byPath,
		state:     r.state,
		instances: r.inst.instances,
		runID:     r.runID,
		specHash:  r.specHash,
	}
}

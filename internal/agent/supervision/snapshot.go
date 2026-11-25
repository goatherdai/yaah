// snapshot.go builds the supervisor's compact per-agent view from data that
// already exists (yaah-1050.1.1, the read-only increment of the effect
// stream): the run registry says who is executing, and the kernel trace
// already holds appends every tool call as a declaration plus a capture. A
// snapshot is therefore a READ of two existing stores, joined — nothing is
// recorded, no hook is added, and the worker's context is never touched. The
// builder has no handle on an agent at all: it takes run metadata and a
// trace reader, so "snapshot the worker" cannot become "poke the worker".
package supervision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	kerneltrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// Snapshot bounds. A snapshot is a summary polled on demand, so every axis
// is capped: agents, the trailing trail, the file set, and the length of the
// model-authored strings inside them.
const (
	// maxSnapshotAgents caps how many agents one snapshot describes.
	maxSnapshotAgents = 50
	// maxTrailCalls is the trailing window per agent: recent is what a
	// supervisor acts on, and the totals (Calls, Errors) keep the rest of
	// the picture without carrying it.
	maxTrailCalls = 25
	// maxSnapshotFiles caps an agent's modified-file set.
	maxSnapshotFiles = 12
	// Model-authored text is clipped before it leaves this package: args
	// and error strings are one line each in the tool's rendering.
	maxSnapshotArgsBytes  = 160
	maxSnapshotErrorBytes = 160
	maxSnapshotNoteBytes  = 200
)

// Read window. The kernel's owner-prefix read has no "since" parameter — it
// returns ordinals 0..through — so a tail read is a bounded prefix read plus
// a client-side window. The doubling probe keeps a short dispatch cheap (one
// read of snapshotFirstRead ordinals) while a long one converges on the real
// length in logarithmically many reads, and snapshotMaxOrdinal stops the
// growth so one pathological owner cannot make a poll expensive.
const (
	snapshotFirstRead  = 128
	snapshotMaxOrdinal = 16384
	// snapshotReadTimeout bounds the whole snapshot, not one read: a
	// poll is best-effort, and a stalled store must not hold the turn.
	snapshotReadTimeout = 3 * time.Second
)

// toolSchemaRefPrefix marks a tool declaration in the trace: the middleware
// writes "<prefix><tool name>.v1" for the call and its ".applied" capture for
// the result.
const toolSchemaRefPrefix = "yaah.tool."

// mutatingTools is the set of tools whose declaration args name a file the
// agent is writing. It is deliberately an explicit list: the trace carries no
// file-set fact, so the only honest source is "which call, on which path" —
// and widening it is a decision about a specific tool, not a heuristic over
// tool names. A mutating tool outside this set (bulk, apply_out) contributes
// nothing yet; a file the agent touched through the shell is invisible here,
// because the shell's writes are not in its arguments.
var mutatingTools = map[string]bool{
	"write":  true,
	"edit":   true,
	"append": true,
	"patch":  true,
	"delete": true,
}

// TraceReader is the one read a snapshot needs: an owner's facts up to an
// inclusive ordinal, in both modes. *kerneltrace.SQLiteTraceStore satisfies
// it; a test can substitute a fake without a store.
type TraceReader interface {
	ReadOwnerPrefix(ctx context.Context, auth kerneltrace.ReadContext, ownerID string, through int, modeFilter kerneltrace.ModeFilter) (kerneltrace.Slice, error)
}

// Snapshotter builds the batched per-agent snapshot: the registry's live runs
// plus its bounded finished tail, each joined with the tool-call facts its
// trace owner holds.
//
// It carries the session's ALREADY-OPEN trace store rather than a store
// config, on purpose: the wiring hands it the same handle the session and the
// runner use, so a poll opens nothing, cannot conjure a store at the default
// path, and reads exactly what the worker wrote.
type Snapshotter struct {
	// Runs is the session's run registry. Nil yields an empty snapshot.
	Runs *Registry
	// Store is the session's trace store. Nil yields agents with a Note
	// rather than an error: the run registry alone is still a real
	// snapshot of who is executing.
	Store TraceReader
	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// Snapshot reads the registry and each agent's trace prefix. It never
// returns an error: a poll is best-effort, and every failure is reported on
// the agent it concerned (AgentSnapshot.Note), because "this agent's trace
// could not be read" is information, not a reason to fail the whole batch.
//
// Live runs come first from SnapshotEntries (one lock, so a run that ends
// mid-poll cannot appear twice), then finished newest-first. An agent cap
// therefore drops the OLDEST finished runs before one that just ended.
func (s *Snapshotter) Snapshot() supervisionapi.Snapshot {
	now := time.Now()
	if s != nil && s.Now != nil {
		now = s.Now()
	}
	out := supervisionapi.Snapshot{At: now}
	if s == nil || s.Runs == nil {
		return out
	}

	regs := s.Runs.SnapshotEntries()
	if len(regs) > maxSnapshotAgents {
		out.Dropped = len(regs) - maxSnapshotAgents
		regs = regs[:maxSnapshotAgents]
	}

	// One deadline for the batch: an unresponsive store degrades every
	// agent's note rather than hanging the poll.
	ctx, cancel := context.WithTimeout(context.Background(), snapshotReadTimeout)
	defer cancel()

	out.Agents = make([]supervisionapi.AgentSnapshot, 0, len(regs))
	for _, reg := range regs {
		out.Agents = append(out.Agents, s.agent(ctx, reg, now))
	}
	return out
}

// agent builds one agent's snapshot from its registration plus its facts.
// now is the snapshot's clock: it ages the run and its in-flight calls, so
// one poll reads one instant.
func (s *Snapshotter) agent(ctx context.Context, reg Registration, now time.Time) supervisionapi.AgentSnapshot {
	out := supervisionapi.AgentSnapshot{
		ScopeID: reg.ScopeID,
		OwnerID: ownerOf(reg.ScopeID),
		Role:    reg.Role,
		Label:   reg.Label,
		// Feature is already clipped by the registrant's DeriveFeature;
		// the clip is defensive, for a registrant that did not derive it.
		Feature:      clipRunes(reg.Feature, supervisionapi.MaxFeatureRunes),
		Workdir:      reg.Workdir,
		StillRunning: reg.StillRunning,
		Reversible:   reg.Reversible,
		Redirectable: reg.Redirectable,
		StartedAt:    reg.StartedAt,
		FinishedAt:   reg.FinishedAt,
		Through:      -1,
	}
	// In-flight calls come from the registry's per-run table, not the
	// trace: only a live run can have them, and a finished one reports
	// nil — the property that makes a call cut off by cancellation
	// disappear.
	if reg.StillRunning && s.Runs != nil {
		out.InFlight = s.Runs.InFlightCalls(reg.ScopeID, now)
	}
	if s.Store == nil || out.OwnerID == "" {
		out.Note = "trace store unavailable for this session"
		return out
	}
	slice, note := s.readOwner(ctx, out.OwnerID)
	out.Note = note
	foldAgentFacts(&out, slice)
	return out
}

// ownerOf recovers a scope's trace owner: the runner registers the scope ID
// as "scope:"+sub-agent session ID, which is exactly the TraceOwnerID the
// pipeline appends under.
func ownerOf(scopeID string) string {
	return strings.TrimPrefix(scopeID, "scope:")
}

// readOwner reads an owner's facts, doubling the ordinal bound until the
// read stops being saturated (a read of ordinals 0..bound returns at most
// bound+1 facts, so a full window is the signal that more exists). The
// returned note is empty unless the read failed or was cut at the hard
// ceiling.
func (s *Snapshotter) readOwner(ctx context.Context, ownerID string) (kerneltrace.Slice, string) {
	bound := snapshotFirstRead
	for {
		slice, err := s.Store.ReadOwnerPrefix(ctx, kerneltrace.TrustedReadContext, ownerID, bound, kerneltrace.ModeBoth)
		if err != nil {
			return kerneltrace.Slice{}, "trace read failed: " + clipLine(err.Error(), maxSnapshotNoteBytes)
		}
		n := len(slice.FactIDs())
		if n <= bound {
			return slice, ""
		}
		if bound >= snapshotMaxOrdinal {
			return slice, fmt.Sprintf("read stopped at %d facts; the totals cover that prefix", n)
		}
		if bound > snapshotMaxOrdinal/2 {
			bound = snapshotMaxOrdinal
		} else {
			bound *= 2
		}
	}
}

// foldAgentFacts joins one owner's declarations to their captures and fills
// the agent's view. Pairing is causal, not positional: the middleware appends
// the capture with the declaration's fact IDs as causal parents, so a
// capture's first parent IS its call — the same join the failed-dispatch
// profile uses (runner.subagentTraceProfile).
func foldAgentFacts(out *supervisionapi.AgentSnapshot, slice kerneltrace.Slice) {
	ids := slice.FactIDs()
	if len(ids) == 0 {
		return
	}

	captures := make(map[string]captureOutcome, len(ids)/2)
	for _, id := range ids {
		fact := slice.FactsByID[id]
		env := fact.GetEnvelope()
		if env.Mode != kerneltrace.Capture || len(env.CausedByIDs) == 0 {
			continue
		}
		parent := env.CausedByIDs[0]
		if _, seen := captures[parent]; seen {
			continue
		}
		rec, ok := fact.(kerneltrace.Record)
		if !ok {
			continue
		}
		var o captureOutcome
		o.success, _ = rec.Body.Payload["success"].(bool)
		if msg, ok := rec.Body.Payload["error"].(string); ok {
			o.errMsg = msg
		}
		captures[parent] = o
	}

	var trail []supervisionapi.ToolCallDigest
	files := make([]string, 0, maxSnapshotFiles)
	seenFile := make(map[string]bool, maxSnapshotFiles)
	for _, id := range ids {
		fact := slice.FactsByID[id]
		env := fact.GetEnvelope()
		if v := fact.GetView(); v != nil && v.OwnerOrdinal > out.Through {
			out.Through = v.OwnerOrdinal
		}
		if env.Mode != kerneltrace.Declaration || !strings.HasPrefix(env.SchemaRef, toolSchemaRefPrefix) {
			continue
		}
		view := fact.GetView()
		if view == nil || view.KindLabel == "" {
			continue
		}
		rec, ok := fact.(kerneltrace.Record)
		if !ok {
			continue
		}
		call := supervisionapi.ToolCallDigest{
			Name: view.KindLabel,
			Args: clipLine(argText(rec.Body.Payload["args"]), maxSnapshotArgsBytes),
		}
		capture, paired := captures[id]
		call.Paired = paired
		if paired {
			call.Success = capture.success
			if capture.errMsg != "" {
				call.Error = clipLine(capture.errMsg, maxSnapshotErrorBytes)
			}
		}
		// Calls counts COMPLETED calls. An unpaired declaration is not a
		// call the supervisor can act on yet, and counting it here would
		// report a total that disagrees with the (paired) trail (Qodo
		// review). If the pair never lands — a capture append failed — the
		// declaration stays marked pending in the trail instead.
		if paired {
			out.Calls++
			if !capture.success {
				out.Errors++
				// ConsecutiveErrors counts over the FULL fact stream, not
				// the capped trail: a streak that opened outside the
				// trailing window is the stuck signal a supervisor acts on
				// (yaah-k0x1). A success resets it; an unpaired call —
				// a pending call, or a capture append that failed — is
				// not an observed failure, so it neither counts nor resets.
				out.ConsecutiveErrors++
			} else {
				out.ConsecutiveErrors = 0
			}
		}
		// A file is "modified" only when the call that named it returned
		// successfully: a refusal touched nothing, and an unpaired call has
		// not returned yet.
		if paired && capture.success && mutatingTools[view.KindLabel] {
			if path := argsFilePath(rec.Body.Payload["args"]); path != "" && !seenFile[path] {
				seenFile[path] = true
				if len(files) < maxSnapshotFiles {
					files = append(files, clipLine(path, maxSnapshotArgsBytes))
				}
			}
		}
		out.LastAction = call.Name
		out.LastArgs = call.Args
		trail = append(trail, call)
		if len(trail) > maxTrailCalls {
			trail = trail[1:]
		}
	}
	out.Trail = trail
	out.ModifiedFiles = files
}

// captureOutcome is what a call's capture says about it.
type captureOutcome struct {
	success bool
	errMsg  string
}

// argsFilePath reads the file a mutating call named, out of the call's own
// args. Every tool in mutatingTools takes its target under the schema key
// "filePath", so the lookup is a field read, not a guess over argument names.
// The args arrive either decoded (an object, which is what the pipeline's
// RawMessage becomes after the store's canonical round trip) or as raw JSON
// text (a caller that passed a string), so both shapes are read.
func argsFilePath(v any) string {
	switch a := v.(type) {
	case map[string]any:
		path, _ := a["filePath"].(string)
		return path
	case string:
		var args struct {
			FilePath string `json:"filePath"`
		}
		if json.Unmarshal([]byte(a), &args) == nil {
			return args.FilePath
		}
	}
	return ""
}

// argText renders a declaration's args for display. The payload round-trips
// through canonical JSON, so args arrive either as the decoded value (an
// object) or as a raw string; both render as JSON text.
func argText(v any) string {
	switch a := v.(type) {
	case nil:
		return ""
	case string:
		return a
	default:
		b, err := json.Marshal(a)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// clipLine collapses a model-authored string to one line and caps it. The
// collapse comes first: an error message with embedded newlines would
// otherwise break the tool's one-line-per-item rendering even after the
// byte cap.
func clipLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

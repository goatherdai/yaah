// Settlement: projecting and filtering values that name proposals.

package exec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// selectorValue walks a compiled selector source against recorded run state.
// Steps read through the same value extraction a template reads, so
// `proposals: {from: steps.work.output.proposal_id}` and the template
// `{{ steps.work.output.proposal_id }}` answer from one place — and a fan-out's
// `steps.<id>.collect` is the joined list that same template would render.
func (r *Run) selectorValue(sel *ir.Selector) (any, error) {
	src := sel.From
	var (
		v   any
		err error
	)
	switch src.Kind {
	case ir.SourceStepOutput:
		dep, ok := r.byID[src.StepID]
		if !ok {
			return nil, fmt.Errorf("selector %s names an out-of-scope step %q", src.String(), src.StepID)
		}
		v = r.stepValue(dep, r.state[dep.Path])
	case ir.SourceStepCollect:
		dep, ok := r.byID[src.StepID]
		if !ok {
			return nil, fmt.Errorf("selector %s names an out-of-scope step %q", src.String(), src.StepID)
		}
		st := r.state[dep.Path]
		if st == nil || st.State != StateSucceeded || st.Collect == nil {
			return nil, fmt.Errorf("selector %s: %s has produced no collected items", src.String(), dep.Path)
		}
		v = st.Collect
	case ir.SourceParams:
		p, ok := r.params[src.Name]
		if !ok {
			return nil, fmt.Errorf("selector %s names an unbound param %q", src.String(), src.Name)
		}
		v = p
	default:
		return nil, fmt.Errorf("selector %s is not resolvable here", src.String())
	}
	v, err = walkValue(v, src.Path)
	return v, err
}

// stepValue is the recorded value of one node — the fenced per-binding value
// extraction (yaah-0u0v.13). A report delegate's value is its delivered text,
// unchanged. A proposal-lane delegate's value is a mapping that carries the
// typed handle, so `steps.<id>.output.proposal_id` resolves and a whole-output
// read renders the handle rather than pretending a proposal is a report. A
// settle's value is its settled record.
//
// A node that never succeeded has no value: callers get the empty result text
// and the reference that asked for it fails as it always has.

// stepValue is the recorded value of one node — the fenced per-binding value
// extraction (yaah-0u0v.13). A report delegate's value is its delivered text,
// unchanged. A proposal-lane delegate's value is a mapping that carries the
// typed handle, so `steps.<id>.output.proposal_id` resolves and a whole-output
// read renders the handle rather than pretending a proposal is a report. A
// settle's value is its settled record.
//
// A node that never succeeded has no value: callers get the empty result text
// and the reference that asked for it fails as it always has.
func (r *Run) stepValue(n *ir.Node, st *NodeState) any {
	if n == nil || st == nil {
		return ""
	}
	switch n.Kind {
	case ir.KindSettle:
		return st.settledValue()
	case ir.KindFanout:
		// A fan-out's value IS its collected items: `steps.<id>.output` and
		// `steps.<id>.collect` are two readings of one list, in input order.
		return st.collectValue()
	case ir.KindLoop:
		// A loop's value is its accumulation, for the same reason and in the same
		// order: one entry per succeeded iteration.
		return st.collectValue()
	case ir.KindAggregate:
		// An aggregate's value is the combine it performed: the concatenated list,
		// the merged mapping, or a summarize body's product — structured, so a
		// selector can read into it instead of re-parsing its text.
		return st.Combined
	case ir.KindCall:
		// A call's value is the child run's output bindings — the JSON the
		// step recorded, so `steps.<id>.output.summary` reads one binding a
		// child emit produced, the same decode-on-demand walk a structured
		// report's fields get (yaah-0u0v.23).
		if obj, ok := decodeJSONObject(st.Result); ok {
			return obj
		}
		if st.CallRunID != "" {
			return map[string]any{"child_run": st.CallRunID}
		}
		return st.Result
	case ir.KindGate:
		// A gate's value is its disposition plus the reason and verdict around it,
		// so `steps.<id>.output.disposition` is what a downstream node routes on
		// (yaah-0u0v.21).
		return st.gateValue()
	case ir.KindSelect:
		// A select's value is its decision: the winning candidate plus the ledger
		// around it, so `steps.<id>.output.winner` is what a downstream settle
		// applies and `steps.<id>.output` alone still names the proposal
		// (yaah-0u0v.20).
		return st.selectionValue()
	case ir.KindDelegate:
		if st.Proposal != nil && st.Proposal.ProposalID != "" {
			m := map[string]any{
				"proposal_id": st.Proposal.ProposalID,
				"text":        st.Result,
				"status":      st.Status,
			}
			if st.Proposal.Role != "" {
				m["role"] = st.Proposal.Role
			}
			if st.Proposal.Lane != "" {
				m["lane"] = st.Proposal.Lane
			}
			if st.World != "" {
				m["world"] = st.World
			}
			if st.StopReason != "" {
				m["stop_reason"] = st.StopReason
			}
			// Per-binding values inside the delivered payload: a structured
			// Report or Verdict body is walked by name, so `output.diff` and
			// `output.verdict` read the same JSON the lane delivered.
			if obj, ok := decodeJSONObject(st.Result); ok {
				for k, val := range obj {
					if _, exists := m[k]; !exists {
						m[k] = val
					}
				}
			}
			return m
		}
	}
	return st.Result
}

// walkValue walks a dotted access path into a recorded value, decoding a JSON
// object body on demand. It is the Scope-side half of the same rule
// ir.template's walkPath applies to a rendered reference: a delivered result is
// model text, so the object may be wrapped in prose or a fence, and the first
// decodable object in the text is the payload a field is asked of.

// walkValue walks a dotted access path into a recorded value, decoding a JSON
// object body on demand. It is the Scope-side half of the same rule
// ir.template's walkPath applies to a rendered reference: a delivered result is
// model text, so the object may be wrapped in prose or a fence, and the first
// decodable object in the text is the payload a field is asked of.
func walkValue(v any, path []string) (any, error) {
	for _, seg := range path {
		m, ok := v.(map[string]any)
		if !ok {
			if s, isStr := v.(string); isStr {
				if decoded, isObj := decodeJSONObject(s); isObj {
					m = decoded
				}
			}
			if m == nil {
				return nil, fmt.Errorf("value has no field %q (value is not a mapping)", seg)
			}
		}
		next, ok := m[seg]
		if !ok {
			return nil, fmt.Errorf("value has no field %q", seg)
		}
		v = next
	}
	return v, nil
}

// decodeJSONObject reads the first JSON object in a string. A delivered
// result is model text, and a model returning JSON often wraps it in prose or
// a markdown fence; the object is still the structured payload a field is
// asked of. The scan is bounded (first 128 '{' positions) so a large
// malformed text cannot make it quadratic; text with no object at all is a
// refusal, not a payload.

// decodeJSONObject reads the first JSON object in a string. A delivered
// result is model text, and a model returning JSON often wraps it in prose or
// a markdown fence; the object is still the structured payload a field is
// asked of. The scan is bounded (first 128 '{' positions) so a large
// malformed text cannot make it quadratic; text with no object at all is a
// refusal, not a payload.
func decodeJSONObject(s string) (map[string]any, bool) {
	attempts := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if attempts++; attempts > 128 {
			return nil, false
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var m map[string]any
		if err := dec.Decode(&m); err == nil {
			return m, true
		}
	}
	return nil, false
}

// refsFromValue turns a resolved selector value into proposal refs. A ref is a
// proposal id spelled either as the string itself or as the proposal_id (or
// id) field of a handle mapping — the two shapes the proposal runtime and the
// value family both use.

// refsFromValue turns a resolved selector value into proposal refs. A ref is a
// proposal id spelled either as the string itself or as the proposal_id (or
// id) field of a handle mapping — the two shapes the proposal runtime and the
// value family both use.
func refsFromValue(v any) ([]ProposalRef, string) {
	switch t := v.(type) {
	case nil:
		return nil, "is empty"
	case []any:
		var out []ProposalRef
		for _, e := range t {
			refs, why := refsFromValue(e)
			if why != "" {
				return nil, why
			}
			out = append(out, refs...)
		}
		return out, ""
	case []string:
		out := make([]ProposalRef, 0, len(t))
		for _, s := range t {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, ProposalRef{ProposalID: s})
			}
		}
		if len(out) == 0 {
			return nil, "is empty"
		}
		return out, ""
	case string:
		// A collected product is often the producer's report text, and a
		// structured report names its proposal inside a JSON body — the
		// same decode-on-demand a selector's elementField applies, so a
		// handle set and a field filter read one record (yaah-0u0v.27).
		if obj, ok := decodeJSONObject(t); ok {
			return refsFromValue(obj)
		}
		if s := strings.TrimSpace(t); s != "" {
			return []ProposalRef{{ProposalID: s}}, ""
		}
		return nil, "is empty"
	case map[string]any:
		if id := handleID(t); id != "" {
			ref := ProposalRef{ProposalID: id}
			if p, ok := t["producer"].(string); ok {
				ref.Producer = p
			}
			ref.Fields = elementFields(t)
			return []ProposalRef{ref}, ""
		}
		if list, ok := t["proposals"]; ok {
			return refsFromValue(list)
		}
	}
	return nil, fmt.Sprintf("is not a proposal handle (got %T)", v)
}

func handleID(m map[string]any) string {
	for _, k := range []string{"proposal_id", "id"} {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// applyFilter evaluates the settle's where predicate against each resolved
// element. Elements are proposal handles, so the fields a filter can read are
// the handle's own. A field no handle carries is refused rather than compared
// as absent — a filter that matched nothing because a name was mistyped is
// exactly the quiet outcome on_empty exists to prevent.

// applyFilter evaluates the settle's where predicate against each resolved
// element. Elements are proposal handles, so the fields a filter can read are
// the handle's own. A field no handle carries is refused rather than compared
// as absent — a filter that matched nothing because a name was mistyped is
// exactly the quiet outcome on_empty exists to prevent.
func applyFilter(refs []ProposalRef, sel *ir.Selector) ([]ProposalRef, string) {
	if sel.Predicate.Op == ir.PredNone && sel.Predicate.Field == "" {
		return refs, ""
	}
	var out []ProposalRef
	for _, ref := range refs {
		v, why := handleField(ref, sel.Predicate.Field)
		if why != "" {
			return nil, why
		}
		ok, err := predicateMatch(v, sel.Predicate)
		if err != nil {
			return nil, ReasonSettleSourceUnavailable + ": where " + err.Error()
		}
		if ok {
			out = append(out, ref)
		}
	}
	return out, ""
}

// elementFields keeps the source element's bindings other than the handle
// identity, so a same-source where can read what the element said about the
// proposal it named (yaah-0u0v.27).

// elementFields keeps the source element's bindings other than the handle
// identity, so a same-source where can read what the element said about the
// proposal it named (yaah-0u0v.27).
func elementFields(t map[string]any) map[string]any {
	out := make(map[string]any, len(t))
	for k, v := range t {
		switch k {
		case "proposal_id", "id", "producer":
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func handleField(ref ProposalRef, field string) (any, string) {
	switch field {
	case "", "proposal_id", "id":
		return ref.ProposalID, ""
	case "producer", "step":
		if ref.Producer == "" {
			return nil, ReasonSettleSourceUnavailable + ": handle " + ref.ProposalID +
				" records no producing step, so where cannot filter on " + field
		}
		return ref.Producer, ""
	}
	if ref.Fields != nil {
		if v, ok := ref.Fields[field]; ok {
			return v, ""
		}
	}
	return nil, ReasonSettleSourceUnavailable + fmt.Sprintf(
		": a proposal handle carries no field %q; the where reads a field its source element did not carry, or names a field of another step's set, which needs the join across two collects", field)
}

func predicateMatch(v any, p ir.Predicate) (bool, error) {
	switch p.Op {
	case ir.PredNone:
		// A `field:` with no operator is the retention filter: keep the
		// elements whose field is set.
		return !valueEmpty(v), nil
	case ir.PredEquals:
		return valuesEqual(v, p.Arg), nil
	case ir.PredNotEquals:
		return !valuesEqual(v, p.Arg), nil
	case ir.PredIn:
		list, ok := p.Arg.([]any)
		if !ok {
			return false, fmt.Errorf("predicate in requires a list of literals")
		}
		for _, e := range list {
			if valuesEqual(v, e) {
				return true, nil
			}
		}
		return false, nil
	case ir.PredNotEmpty:
		return !valueEmpty(v) == wantTrue(p.Arg), nil
	case ir.PredExists:
		return (v != nil) == wantTrue(p.Arg), nil
	}
	return false, fmt.Errorf("unknown predicate %q", p.Op)
}

func wantTrue(arg any) bool {
	b, _ := arg.(bool)
	return b
}

func valueEmpty(v any) bool {
	return v == nil || v == ""
}

func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	return scalarText(a) == scalarText(b)
}

func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func sameSource(a, b *ir.Source) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Kind != b.Kind || a.Name != b.Name || a.StepID != b.StepID {
		return false
	}
	if len(a.Path) != len(b.Path) {
		return false
	}
	for i := range a.Path {
		if a.Path[i] != b.Path[i] {
			return false
		}
	}
	return true
}

func dedupeRefs(refs []ProposalRef) []ProposalRef {
	seen := map[string]bool{}
	out := make([]ProposalRef, 0, len(refs))
	for _, ref := range refs {
		if ref.ProposalID == "" || seen[ref.ProposalID] {
			continue
		}
		seen[ref.ProposalID] = true
		out = append(out, ref)
	}
	return out
}

// sortRefs fixes the plan's order. Canonical order is producer path then
// proposal id, which is a function of the graph and the recorded handles and
// nothing else.
//
// smallest_diff_first is the one declared order that is not that order, and
// the scheduler holds no diff-size fact: the size lives in the proposal
// runtime's change metadata, which the settlement envelope reports only after
// the disposition. So the plan is taken in canonical order and the run says so
// once in Result.Warnings, rather than silently redefining the author's order
// or refusing a legal declaration. The size fact is fan-out and registry-read
// plumbing (yaah-0u0v.14).

// sortRefs fixes the plan's order. Canonical order is producer path then
// proposal id, which is a function of the graph and the recorded handles and
// nothing else.
//
// smallest_diff_first is the one declared order that is not that order, and
// the scheduler holds no diff-size fact: the size lives in the proposal
// runtime's change metadata, which the settlement envelope reports only after
// the disposition. So the plan is taken in canonical order and the run says so
// once in Result.Warnings, rather than silently redefining the author's order
// or refusing a legal declaration. The size fact is fan-out and registry-read
// plumbing (yaah-0u0v.14).
func sortRefs(refs []ProposalRef, order ir.SettleOrder) {
	if order == ir.OrderSmallestDiffFirst {
		sort.SliceStable(refs, func(i, j int) bool {
			return refs[i].ProposalID < refs[j].ProposalID
		})
		return
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].Producer != refs[j].Producer {
			return refs[i].Producer < refs[j].Producer
		}
		return refs[i].ProposalID < refs[j].ProposalID
	})
}

// flatten renders a multi-line fact as one log field. The event log is a
// byte-stable one-line-per-event artifact, so no settlement prose — an error
// body or a conflict note — may put a newline into it.

// flatten renders a multi-line fact as one log field. The event log is a
// byte-stable one-line-per-event artifact, so no settlement prose — an error
// body or a conflict note — may put a newline into it.
func flatten(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(s), " ")
}

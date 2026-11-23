package exec

import (
	"testing"

	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/trace"
)

func ledgerInt(v int) *int { return &v }

func TestSettlementLedgerIsIndependentlyUsable(t *testing.T) {
	l := newSettlementLedger()
	l.open["p2"] = "steps.b"
	l.open["p1"] = "steps.a"
	l.claims["p1"] = "steps.a"
	l.settled["p0"] = "steps.z"

	if got := l.openProposals(); len(got) != 2 || got[0] != "p1" || got[1] != "p2" {
		t.Fatalf("openProposals = %v, want sorted [p1 p2]", got)
	}
	if got := l.detail(); got != "p1 (steps.a), p2 (steps.b)" {
		t.Errorf("detail = %q, want named owners", got)
	}

	st := &NodeState{SettlePlan: &SettlePlan{Refs: []ProposalRef{
		{ProposalID: "p1"}, {ProposalID: "p0"}, {ProposalID: "p2"},
	}}}
	l.releaseClaims(st, "steps.a")
	if _, ok := l.claims["p1"]; ok {
		t.Error("the node's own claim was not released")
	}
}

func TestInstanceLedgerIsIndependentlyUsable(t *testing.T) {
	l := newInstanceLedger()
	byID := map[string]*ir.Node{
		"branch": {Path: "steps.branch", Kind: ir.KindBranch, Cases: []ir.Case{{Then: "arm"}}},
		"arm":    {Path: "steps.arm"},
	}
	byPath := map[string]*ir.Node{
		"steps.fan": {Path: "steps.fan", Kind: ir.KindFanout, MaxParallel: ledgerInt(2)},
	}
	l.instances["steps.fan.body[0].work"] = &instance{owner: "steps.fan"}
	l.itemInFlight["steps.fan"] = 1

	if got := l.itemMaxParallel("steps.fan.body[0].work", byPath); got != 2 {
		t.Errorf("itemMaxParallel = %d, want 2", got)
	}
	if !l.itemSlotFree("steps.fan.body[0].work", byPath) {
		t.Error("one in-flight dispatch under max_parallel=2 should have room")
	}
	l.itemInFlight["steps.fan"] = 2
	if l.itemSlotFree("steps.fan.body[0].work", byPath) {
		t.Error("a full nested cap must refuse another slot")
	}

	l.registerArms(byID["branch"], byID)
	state := map[string]*NodeState{}
	if selected, undecided := l.armRouting("steps.arm", state); selected || !undecided {
		t.Errorf("unrouted arm = (%v, %v), want undecided", selected, undecided)
	}
	state["steps.branch"] = &NodeState{State: StateSucceeded, Selected: "steps.arm"}
	if selected, undecided := l.armRouting("steps.arm", state); !selected || undecided {
		t.Errorf("selected arm = (%v, %v), want selected", selected, undecided)
	}
}

func TestReadyLedgerIsIndependentlyUsable(t *testing.T) {
	var q readyLedger
	q.insertReady("steps.b")
	q.insertReady("steps.a")
	q.insertReady("steps.a") // duplicate is a no-op
	if got := q.ready; len(got) != 2 || got[0] != "steps.a" || got[1] != "steps.b" {
		t.Fatalf("ready = %v, want sorted [steps.a steps.b]", got)
	}
	q.dropReady("steps.a")
	if got := q.ready; len(got) != 1 || got[0] != "steps.b" {
		t.Fatalf("ready after drop = %v, want [steps.b]", got)
	}
	q.dropReady("steps.missing") // no-op
	if got := q.ready; len(got) != 1 {
		t.Fatalf("ready after missing drop = %v, want unchanged", got)
	}
}

func TestTraceLedgerIsIndependentlyUsable(t *testing.T) {
	l := newTraceLedger()
	l.warnOnce("k", "first")
	l.warnOnce("k", "second") // once-only
	if len(l.warnings) != 1 || l.warnings[0] != "first" {
		t.Fatalf("warnings = %v, want one deduped entry", l.warnings)
	}

	l.events = []string{"a", "b", "c"}
	l.persisted = 1
	if got := l.newEvents(); got != 1 {
		t.Errorf("newEvents = %d, want 1", got)
	}
	l.persisted = 99 // overclamped
	if got := l.newEvents(); got != 3 {
		t.Errorf("overclamped newEvents = %d, want 3", got)
	}
	l.persisted = -1 // negative-clamped
	if got := l.newEvents(); got != 0 {
		t.Errorf("negative newEvents = %d, want 0", got)
	}

	l.records = []trace.Record{{}}
	l.recordsPersisted = 99 // overclamped
	if got := l.newRecords(); got != 1 {
		t.Errorf("overclamped newRecords = %d, want 1", got)
	}
}

func TestJoinLedgerIsIndependentlyUsable(t *testing.T) {
	var l joinLedger
	st := &NodeState{}
	if err := l.onJoinDiscardAsked("steps.f", st, SettlementAskedEvent{}); err == nil {
		t.Error("an ask with no proposal id should be refused")
	}
	if err := l.onJoinDiscardAsked("steps.f", st, SettlementAskedEvent{ProposalID: "p1"}); err != nil {
		t.Fatalf("first ask: %v", err)
	}
	if st.Asked != "p1" {
		t.Fatalf("Asked = %q, want p1", st.Asked)
	}
	if err := l.onJoinDiscardAsked("steps.f", st, SettlementAskedEvent{ProposalID: "p2"}); err == nil {
		t.Error("a second ask over an unanswered proposal should be refused")
	}

	view := joinView{
		byPath: map[string]*ir.Node{
			"steps.fan": {Path: "steps.fan", Kind: ir.KindFanout, Join: ir.JoinAny},
		},
		state: map[string]*NodeState{
			"steps.fan": {State: StateSucceeded},
		},
		instances: map[string]*instance{
			"steps.fan.items[0]": {owner: "steps.fan", item: 0},
		},
	}
	if container, item, ok := l.enclosingCancelJoin(view, "steps.fan.items[0]"); !ok || container != "steps.fan" || item != 0 {
		t.Errorf("enclosingCancelJoin = (%q, %d, %v), want (steps.fan, 0, true)", container, item, ok)
	}
	if !l.inJoinResidue(view, "steps.fan.items[0]") {
		t.Error("inJoinResidue should be true for a decided join item")
	}
	if l.inJoinResidue(view, "steps.other") {
		t.Error("inJoinResidue should be false for a path with no instance")
	}
}

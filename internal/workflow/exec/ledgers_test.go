package exec

import (
	"testing"

	"github.com/goatherdai/yaah/internal/workflow/ir"
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

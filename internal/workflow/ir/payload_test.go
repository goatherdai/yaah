package ir

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/workflow/spec"
)

func intp(v int) *int { return &v }

// TestPayloadOwnershipCoversEveryField pins the table itself: adding a field
// to Node without saying which kinds own it (or that it is common) fails here,
// so the construction check cannot silently stop covering a new payload.
func TestPayloadOwnershipCoversEveryField(t *testing.T) {
	typ := reflect.TypeOf(Node{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if commonNodeFields[name] {
			continue
		}
		owners, ok := payloadFieldKinds[name]
		if !ok {
			t.Errorf("Node field %s is neither common nor owned by any kind", name)
			continue
		}
		if len(owners) == 0 {
			t.Errorf("Node field %s has no owners", name)
		}
		for _, kind := range owners {
			switch kind {
			case KindDelegate, KindFanout, KindBranch, KindLoop, KindGate,
				KindSettle, KindSelect, KindAggregate, KindCall, KindEmit, KindHuman:
			default:
				t.Errorf("Node field %s lists unknown kind %q", name, kind)
			}
		}
	}
	for name := range payloadFieldKinds {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("payloadFieldKinds names %s, which Node does not declare", name)
		}
		if commonNodeFields[name] {
			t.Errorf("field %s is both common and kind-owned", name)
		}
	}
}

func TestPayloadProblem(t *testing.T) {
	tests := []struct {
		name string
		node *Node
		want string // substring of the diagnostic, empty means no problem
	}{
		{"clean delegate", &Node{Kind: KindDelegate, Role: "dev", Prompt: &Template{}}, ""},
		{"clean fanout body", &Node{Kind: KindFanout, Body: []*Node{{Kind: KindEmit}}, Collect: "out"}, ""},
		{"clean loop shared fields", &Node{Kind: KindLoop, Body: []*Node{{Kind: KindEmit}}, Collect: "out", ContinueFrom: &Template{}, MaxIterations: intp(2)}, ""},
		{"clean human prompt", &Node{Kind: KindHuman, Prompt: &Template{}, OnTimeout: OnFailureSkip}, ""},
		{"clean settle policy", &Node{Kind: KindSettle, SettlePolicy: SettlePathDisjoint, Action: SettleApply}, ""},
		{"clean gate judge", &Node{Kind: KindGate, Judge: &Judge{Role: "judge"}}, ""},
		{"delegate with a fanout field", &Node{Kind: KindDelegate, Role: "dev", MaxParallel: intp(2)}, "MaxParallel"},
		{"emit with a call field", &Node{Kind: KindEmit, Value: &Template{}, Workflow: "helper"}, "Workflow"},
		{"branch with an emit artifact", &Node{Kind: KindBranch, On: &Selector{}, ArtifactPath: "out.txt"}, "ArtifactPath"},
		{"settle written to the select policy key", &Node{Kind: KindSettle, Policy: SelectJudge}, "Policy"},
		{"wrong fields are sorted", &Node{Kind: KindEmit, Workflow: "helper", Role: "dev"}, "Role, Workflow"},
		{"common fields are always allowed", &Node{Kind: KindEmit, Needs: []string{"a"}, TimeoutSeconds: intp(5), Output: &OutputSpec{Name: "x"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.node.PayloadProblem()
			if tt.want == "" {
				if got != "" {
					t.Errorf("PayloadProblem = %q, want no problem", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("PayloadProblem = %q, want it to name %q", got, tt.want)
			}
		})
	}
}

// TestResolvedGraphHasCleanPayloads is the integration half: a real document,
// including nested bodies, resolves to nodes whose payload matches their kind.
// It guards the ownership table against false positives — a wrong entry there
// would break every legitimate workflow, not just the hand-built bad cases.
func TestResolvedGraphHasCleanPayloads(t *testing.T) {
	doc, err := spec.Parse([]byte(`apiVersion: yaah.workflow/v1
kind: Workflow
metadata:
  name: payload-clean
spec:
  steps:
    - id: plan
      kind: delegate
      role: developer
      prompt: plan it
    - id: work
      kind: fanout
      over: [a, b]
      as: item
      body:
        - {id: build, kind: delegate, role: developer, prompt: "do {{ item }}"}
    - id: sub
      kind: call
      workflow: helper
      needs: [work]
    - id: report
      kind: emit
      needs: [sub]
      value: done
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	g, err := Resolve(doc)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var walk func([]*Node)
	walk = func(nodes []*Node) {
		for _, n := range nodes {
			if problem := n.PayloadProblem(); problem != "" {
				t.Errorf("resolved node %s: %s", n.Path, problem)
			}
			walk(n.Body)
		}
	}
	walk(g.Steps)
}

package ir

import (
	"reflect"
	"slices"
	"strings"
)

// payloadFieldKinds names the kinds that may set each kind-specific field of
// Node. The common fields (Path, ID, Kind, Needs, When, Retry, TimeoutSeconds,
// OnFailure, Output) belong to every node and are deliberately absent.
//
// Node is one flat struct, so the type system cannot enforce the closed node
// vocabulary: a hand-built node may carry another kind's field without anyone
// saying so. This table is the construction-time half of that contract; the
// validator's WF014 is the reporting half for authoring documents. A field
// shared by two kinds (Prompt, Body, Collect, ContinueFrom, Judge) lists both
// owners.
var payloadFieldKinds = map[string][]Kind{
	// delegate.
	"Role":         {KindDelegate},
	"Lane":         {KindDelegate},
	"SeedFrom":     {KindDelegate},
	"ForkFrom":     {KindDelegate},
	"Provider":     {KindDelegate},
	"Model":        {KindDelegate},
	"MaxTurns":     {KindDelegate},
	"Grants":       {KindDelegate},
	"Network":      {KindDelegate},
	"World":        {KindDelegate},
	"Prompt":       {KindDelegate, KindHuman},
	"ContinueFrom": {KindDelegate, KindLoop},

	// fanout.
	"Over":          {KindFanout},
	"As":            {KindFanout},
	"MaxParallel":   {KindFanout},
	"Join":          {KindFanout},
	"Quorum":        {KindFanout},
	"OnItemFailure": {KindFanout},
	"Body":          {KindFanout, KindLoop, KindAggregate},
	"Collect":       {KindFanout, KindLoop},

	// branch.
	"On":      {KindBranch},
	"Cases":   {KindBranch},
	"Default": {KindBranch},

	// loop.
	"Until":         {KindLoop},
	"MaxIterations": {KindLoop},

	// gate and select.
	"Assert":     {KindGate},
	"OnPass":     {KindGate},
	"OnFail":     {KindGate},
	"Judge":      {KindGate, KindSelect},
	"Candidates": {KindSelect},
	"Policy":     {KindSelect},

	// settle.
	"Proposals":    {KindSettle},
	"Where":        {KindSettle},
	"Action":       {KindSettle},
	"Order":        {KindSettle},
	"OnConflict":   {KindSettle},
	"OnEmpty":      {KindSettle},
	"SettlePolicy": {KindSettle},

	// aggregate.
	"Inputs":   {KindAggregate},
	"Strategy": {KindAggregate},

	// call.
	"Workflow":   {KindCall},
	"CallParams": {KindCall},
	"MaxDepth":   {KindCall},

	// emit.
	"Value":        {KindEmit},
	"ArtifactPath": {KindEmit},

	// human.
	"OnTimeout": {KindHuman},
}

// commonNodeFields are the Node fields every kind may carry. They are exempt
// from the payload-ownership check, and payload_test pins that the two lists
// together cover every field of the struct.
var commonNodeFields = map[string]bool{
	"Path": true, "ID": true, "Kind": true,
	"Needs": true, "When": true, "Retry": true,
	"TimeoutSeconds": true, "OnFailure": true, "Output": true,
}

// PayloadProblem names the set payload fields a node carries that its kind
// does not own, or returns "" when the node's payload is clean. It is the
// construction-time guard for hand-built graphs as well as the assertion
// resolveNode makes on every node it builds. Names are sorted so the same
// node always produces the same diagnostic.
func (n *Node) PayloadProblem() string {
	if n == nil {
		return ""
	}
	v := reflect.ValueOf(*n)
	t := v.Type()
	var bad []string
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		owners, ok := payloadFieldKinds[name]
		if !ok || slices.Contains(owners, n.Kind) {
			continue
		}
		if !v.Field(i).IsZero() {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	slices.Sort(bad)
	return "kind " + string(n.Kind) + " carries fields it does not own: " + strings.Join(bad, ", ")
}

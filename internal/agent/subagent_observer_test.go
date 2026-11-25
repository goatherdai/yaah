package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// feedTool records what its scope's in-flight table holds while it runs:
// the loop publishes the start event BEFORE executing the call, so the
// call must be visible inside Execute; the end event removes it after.
type feedTool struct {
	tbl         *supervision.InFlightTable
	sawInFlight []supervisionapi.InFlightCall
}

func (t *feedTool) Name() string { return "feed" }
func (t *feedTool) Description() string {
	return "records what the in-flight table holds while it runs"
}
func (t *feedTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *feedTool) Execute(ctx context.Context, args string) (string, error) {
	// Broker delivery is asynchronous, so bounded-poll for the start the
	// loop published before executing this call.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if open := t.tbl.Entries(time.Now()); len(open) > 0 {
			t.sawInFlight = open
			return "ok", nil
		}
		if !time.Now().Before(deadline) {
			return "no in-flight entry was visible", nil
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSubAgentLoop_InFlightFeedReachesScopeTable pins the feed seam end to
// end (yaah-k0x1): the observer attached to a sub-agent loop forwards its
// tool start and end events into its OWN scope's table — the start is
// visible while the call executes, the matching end removes it, and
// observing changes nothing about the run itself (no extra model turns).
func TestSubAgentLoop_InFlightFeedReachesScopeTable(t *testing.T) {
	runs := supervision.NewRegistry()
	scopeID := "scope:feed-worker"
	runs.Register(scopeID, supervision.Run{SteerCh: make(chan string, supervision.SteerBuffer)})
	gen, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("Generation must report the live registration")
	}
	tbl := runs.InFlightTable(scopeID, gen)
	if tbl == nil {
		t.Fatal("the registered run must expose its in-flight table")
	}

	tool := &feedTool{tbl: tbl}
	reg := tools.NewEmptyRegistry()
	reg.Register(tool)
	fp := &fakeProvider{responses: []*types.ChatResponse{
		steerToolCallResponse("tc1", "feed"),
		{
			Choices: []types.Choice{{Message: types.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}},
			Usage:   types.Usage{TotalTokens: 5},
		},
	}}

	loop := NewSubAgentLoop(fp, reg, "test-model", "sys", SubAgentConfig{
		MaxLoopCycles: 5,
		MaxToolTurns:  5,
		SessionID:     "feed-worker",
		EventObserver: tbl.Observe,
	})

	res, err := loop.Run(context.Background(), "use the feed tool")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res != "done" {
		t.Errorf("Run = %q, want the loop to finish", res)
	}
	if len(fp.requests) != 2 {
		t.Errorf("model turns = %d, want exactly the tool turn and the final turn (observing must add none)", len(fp.requests))
	}
	if len(tool.sawInFlight) != 1 || tool.sawInFlight[0].Name != "feed" {
		t.Fatalf("in-flight during execution = %+v, want the feed call the start event opened", tool.sawInFlight)
	}

	// The end event removed the entry: bounded poll, delivery is async.
	deadline := time.Now().Add(2 * time.Second)
	for len(tbl.Entries(time.Now())) > 0 {
		if !time.Now().Before(deadline) {
			t.Fatal("the tool's end event never removed the in-flight entry")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSubAgentLoop_NoObserverKeepsNoopView pins the seam's default: a
// sub-agent loop built without an observer keeps the bare NoopView, so an
// unobserved run is exactly what it was before the seam existed.
func TestSubAgentLoop_NoObserverKeepsNoopView(t *testing.T) {
	loop := NewSubAgentLoop(nil, tools.NewEmptyRegistry(), "m", "p", SubAgentConfig{})
	if _, ok := loop.View.(NoopView); !ok {
		t.Errorf("View = %T, want the bare NoopView", loop.View)
	}
}

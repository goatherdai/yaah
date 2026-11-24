package runner

import (
	"context"
	"sync"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/types"
)

// TestRecordSubAgentUsage_PerGenerationAppendIntents pins finding #6
// (yaah-12z5): concurrent generations of ONE sub-agent session must not
// corrupt each other's usage facts. The trace append intent is keyed
// per generation, so both records persist even when the generations
// interleave; the old sessionID+":usage" key let one generation's
// append collapse into the other's.
//
// Cross-repo assumption (deps pinned in go.mod): the goatherd-kernel
// trace store dedupes appends by AppendIntentID — the same intent ID
// from two writers collapses into one record. That kernel behavior is
// pinned by the module version in go.mod; this test asserts the yaah
// side of the contract: distinct intent IDs per generation.
func TestRecordSubAgentUsage_PerGenerationAppendIntents(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	defer store.Close(context.Background())

	owner := "sub-gens-sess-9"
	var wg sync.WaitGroup
	for gen := uint64(1); gen <= 2; gen++ {
		wg.Add(1)
		go func(gen uint64) {
			defer wg.Done()
			recordSubAgentUsage(context.Background(), store, owner, "sess-parent", "developer", "deepseek-flash", "retained", gen, types.Usage{
				PromptTokens:     100 * int(gen),
				CompletionTokens: 10 * int(gen),
				TotalTokens:      110 * int(gen),
			})
		}(gen)
	}
	wg.Wait()

	slice, err := store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, 99, kerneltrace.ModeCapturesOnly)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var usageFacts int
	for _, id := range slice.FactIDs() {
		rec, ok := slice.FactsByID[id].(kerneltrace.Record)
		if ok && rec.Envelope.SchemaRef == "yaah.subagent.usage.v1" {
			usageFacts++
		}
	}
	if usageFacts != 2 {
		t.Fatalf("usage facts = %d, want 2 (one per generation; concurrent generations must not collapse into one)", usageFacts)
	}
}

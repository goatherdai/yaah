package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/jobs"
)

// TestDelegate_SeedFromValidation pins the threading guards: seed_from needs
// the retained lane, a proposal-lane role, and a known seed_mode. Each refusal
// must happen before any dispatch is provisioned.
func TestDelegate_SeedFromValidation(t *testing.T) {
	noop := func(context.Context, string, jobs.SubAgentParams) (string, error) { return "", nil }
	args := func(extra string) string {
		return `{"description":"d","prompt":"p","role":"developer"` + extra + `}`
	}

	t.Run("requires the retained lane", func(t *testing.T) {
		tt := &DelegateTool{RoleNames: []string{"developer"}, Runner: noop}
		_, err := tt.Execute(context.Background(), args(`,"seed_from":"proposal:x"`))
		if err == nil || !strings.Contains(err.Error(), "retained lane") {
			t.Fatalf("err = %v, want a retained-lane refusal", err)
		}
	})

	t.Run("proposal lane only", func(t *testing.T) {
		rt := &RetainedRuntime{}
		tt := &DelegateTool{
			RoleNames:   []string{"developer"},
			Runner:      noop,
			Retained:    rt,
			LaneForRole: func(string) DelegateLane { return LaneReport },
		}
		_, err := tt.Execute(context.Background(), args(`,"seed_from":"proposal:x"`))
		if err == nil || !strings.Contains(err.Error(), "proposal-lane") {
			t.Fatalf("err = %v, want a proposal-lane refusal", err)
		}
	})

	t.Run("rejects an unknown seed_mode", func(t *testing.T) {
		rt := &RetainedRuntime{}
		tt := &DelegateTool{RoleNames: []string{"developer"}, Runner: noop, Retained: rt}
		_, err := tt.Execute(context.Background(), args(`,"seed_from":"proposal:x","seed_mode":"bogus"`))
		if err == nil || !strings.Contains(err.Error(), "seed_mode") {
			t.Fatalf("err = %v, want a seed_mode refusal", err)
		}
	})

	t.Run("rejects seed_mode without seed_from", func(t *testing.T) {
		rt := &RetainedRuntime{}
		tt := &DelegateTool{RoleNames: []string{"developer"}, Runner: noop, Retained: rt}
		_, err := tt.Execute(context.Background(), args(`,"seed_mode":"workspace"`))
		if err == nil || !strings.Contains(err.Error(), "requires seed_from") {
			t.Fatalf("err = %v, want a seed_mode-without-seed_from refusal", err)
		}
	})
}

// TestDelegate_BaseRefValidation pins the base_ref guards (yaah-witj increment
// B). Each refusal happens in the dispatch core before any world is resolved,
// which is why a hand-built tool with a noop runner is enough to observe them.
func TestDelegate_BaseRefValidation(t *testing.T) {
	noop := func(context.Context, string, jobs.SubAgentParams) (string, error) { return "", nil }
	args := func(extra string) string {
		return `{"description":"d","prompt":"p","role":"developer"` + extra + `}`
	}

	t.Run("refuses base_ref with seed_from", func(t *testing.T) {
		rt := &RetainedRuntime{}
		tt := &DelegateTool{RoleNames: []string{"developer"}, Runner: noop, Retained: rt}
		_, err := tt.Execute(context.Background(), args(`,"base_ref":"side","seed_from":"proposal:x"`))
		if err == nil || !strings.Contains(err.Error(), "both name a basis") {
			t.Fatalf("err = %v, want a both-name-a-basis refusal", err)
		}
	})

	t.Run("refuses base_ref with seed_mode", func(t *testing.T) {
		rt := &RetainedRuntime{}
		tt := &DelegateTool{RoleNames: []string{"developer"}, Runner: noop, Retained: rt}
		_, err := tt.Execute(context.Background(), args(`,"base_ref":"side","seed_mode":"workspace"`))
		if err == nil || !strings.Contains(err.Error(), "seed_mode") || !strings.Contains(err.Error(), "base_ref") {
			t.Fatalf("err = %v, want a refusal naming both base_ref and seed_mode", err)
		}
	})

	t.Run("refuses base_ref on the shared lane", func(t *testing.T) {
		rt := &RetainedRuntime{}
		tt := &DelegateTool{
			RoleNames:   []string{"developer"},
			Runner:      noop,
			Retained:    rt,
			LaneForRole: func(string) DelegateLane { return LaneShared },
		}
		_, err := tt.Execute(context.Background(), args(`,"base_ref":"side"`))
		if err == nil || !strings.Contains(err.Error(), "retained lane") {
			t.Fatalf("err = %v, want a retained-lane refusal", err)
		}
	})

	t.Run("refuses base_ref without a retained runtime", func(t *testing.T) {
		tt := &DelegateTool{RoleNames: []string{"developer"}, Runner: noop}
		_, err := tt.Execute(context.Background(), args(`,"base_ref":"side"`))
		if err == nil || !strings.Contains(err.Error(), "retained lane") {
			t.Fatalf("err = %v, want a retained-lane refusal", err)
		}
	})
}

package context

import (
	"testing"

	"github.com/goatherdai/yaah/internal/types"
)

// The renderer is shared with the persister, so its output is the exact
// placeholder a restored row carries. Restore compares against this value
// rather than the "[tool:" prefix, which genuine replies can also start with.
func TestSyntheticToolCarrier(t *testing.T) {
	calls := []types.ToolCall{
		{Function: types.ToolCallFn{Name: "bash", Arguments: `{"command":"ls"}`}},
		{Function: types.ToolCallFn{Name: "read", Arguments: `{"path":"foo.go"}`}},
	}
	want := `[tool:bash] {"command":"ls"}` + "\n" + `[tool:read] {"path":"foo.go"}`
	if got := SyntheticToolCarrier(calls); got != want {
		t.Errorf("SyntheticToolCarrier = %q, want %q", got, want)
	}
	if got := SyntheticToolCarrier(nil); got != "" {
		t.Errorf("SyntheticToolCarrier(nil) = %q, want empty", got)
	}
}

func TestActiveTaskPromptSkipsSyntheticUserRows(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "read every file and write the report"},
		{Role: "assistant", Content: "working"},
		{Role: "user", Content: "[STEER] You have called read heavily. Switch approach."},
	}
	if got := ActiveTaskPrompt(msgs); got != "read every file and write the report" {
		t.Errorf("ActiveTaskPrompt = %q, want the genuine task (steer rows are not the task)", got)
	}
	allSynthetic := []types.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "[STEER] nudge"},
		{Role: "user", Content: "SUPERVISOR: recover"},
	}
	if got := ActiveTaskPrompt(allSynthetic); got != "" {
		t.Errorf("ActiveTaskPrompt = %q, want empty when every user row is synthetic", got)
	}
}

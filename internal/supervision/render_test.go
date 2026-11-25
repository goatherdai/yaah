package supervision

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// fixedClock keeps the fixtures time-independent.
var fixtureTime = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// order returns the labels in the order they appear in text.
func order(text string) []string {
	var labels []string
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "## AGENT "); ok {
			if i := strings.IndexByte(rest, ' '); i > 0 {
				labels = append(labels, rest[:i])
			}
		}
	}
	return labels
}

func TestRender_Labels(t *testing.T) {
	// snap.Agents is deliberately NOT ScopeID-sorted and not live-first, so
	// a positional label scheme would fail here.
	snap := supervisionapi.Snapshot{
		At: fixtureTime,
		Agents: []supervisionapi.AgentSnapshot{
			{ScopeID: "scope:a", Role: "developer", StillRunning: false,
				FinishedAt: fixtureTime.Add(2 * time.Minute)},
			{ScopeID: "scope:z", Role: "developer", StillRunning: false,
				FinishedAt: fixtureTime.Add(1 * time.Minute)},
			{ScopeID: "scope:m", Role: "developer", StillRunning: true},
		},
	}
	text, labels := Render(snap, RenderOptions{Tick: 4})

	t.Run("labels are ScopeID-sorted", func(t *testing.T) {
		want := []Labeled{
			{Label: "w1", ScopeID: "scope:a"},
			{Label: "w2", ScopeID: "scope:m"},
			{Label: "w3", ScopeID: "scope:z"},
		}
		if len(labels) != len(want) {
			t.Fatalf("got %d labels, want %d: %+v", len(labels), len(want), labels)
		}
		for i := range want {
			if labels[i] != want[i] {
				t.Errorf("labels[%d] = %+v, want %+v", i, labels[i], want[i])
			}
		}
	})

	t.Run("label map resolves back to the scope", func(t *testing.T) {
		for _, l := range labels {
			if !strings.Contains(text, "(scope "+l.ScopeID+")") {
				t.Errorf("label %s names %s but no block carried that scope", l.Label, l.ScopeID)
			}
			if !strings.Contains(text, "## AGENT "+l.Label+" ") {
				t.Errorf("label %s never appears as a block header", l.Label)
			}
		}
	})

	t.Run("blocks are live-first then finished newest-first", func(t *testing.T) {
		got := order(text)
		want := []string{"w2", "w1", "w3"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("render order = %v, want %v", got, want)
		}
	})
}

func TestRender_Deterministic(t *testing.T) {
	snap := pathologicalSnapshot(6, 5)
	opts := RenderOptions{Tick: 9, State: map[string]TickState{"scope:s3": {LastAction: "steer", TicksAgo: 1}}}
	first, firstLabels := Render(snap, opts)
	second, secondLabels := Render(snap, opts)
	if first != second {
		t.Error("two renders of the same snapshot differ")
	}
	if len(firstLabels) != len(secondLabels) {
		t.Fatalf("label counts differ: %d vs %d", len(firstLabels), len(secondLabels))
	}
	for i := range firstLabels {
		if firstLabels[i] != secondLabels[i] {
			t.Errorf("labels[%d] = %+v vs %+v", i, firstLabels[i], secondLabels[i])
		}
	}
}

func TestRender_Content(t *testing.T) {
	snap := supervisionapi.Snapshot{
		Agents: []supervisionapi.AgentSnapshot{{
			ScopeID:      "scope:alpha",
			Role:         "golang-developer",
			Feature:      "Add the renderer",
			StillRunning: true,
			StartedAt:    fixtureTime,
			Reversible:   true,
			Redirectable: false,
			Trail: []supervisionapi.ToolCallDigest{
				{Name: "read", Args: "x.go", Paired: true, Success: true},
				{Name: "bash", Args: "go build", Paired: true, Success: false, Error: "build failed"},
				{Name: "write", Args: "y.go", Paired: false},
			},
			ModifiedFiles:     []string{"a.go", "b.go"},
			InFlight:          []supervisionapi.InFlightCall{{Name: "bash", Args: "go test ./x", Age: 3 * time.Second}},
			Calls:             3,
			Errors:            1,
			ConsecutiveErrors: 1,
		}},
	}
	opts := RenderOptions{Tick: 7, State: map[string]TickState{"scope:alpha": {LastAction: "steer", TicksAgo: 2}}}
	text, _ := Render(snap, opts)

	for _, want := range []string{
		"TICK 7 — 1 agents (1 live, 0 finished)",
		"## AGENT w1 (scope scope:alpha)",
		"role: golang-developer   running: yes",
		"feature: Add the renderer",
		"events=3 consecutive_errors=1 last_action=steer (ticks_ago=2)",
		"reversible: true  redirectable: false",
		"modified_files: a.go, b.go",
		"in-flight: bash(go test ./x) age=3s",
		"recent tool calls (oldest → newest):",
		"  1. read(x.go) →ok",
		"  2. bash(go build) →error:ERR build failed",
		"  3. write(y.go) →capture-missing",
		footerJSON,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text is missing %q\n---\n%s", want, text)
		}
	}

	t.Run("unpaired digest never renders as pending", func(t *testing.T) {
		if strings.Contains(text, "pending") {
			t.Error("rendered text claims a call is pending; an unpaired digest is capture-missing")
		}
	})
	t.Run("empty in-flight is none, not idle", func(t *testing.T) {
		bare, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{ScopeID: "scope:b"}}}, RenderOptions{})
		if !strings.Contains(bare, "in-flight: none") {
			t.Errorf("empty in-flight did not render as none:\n%s", bare)
		}
		for _, forbidden := range []string{"idle", "healthy"} {
			if strings.Contains(bare, forbidden) {
				t.Errorf("rendered text claims %q; empty in-flight is ambiguous", forbidden)
			}
		}
	})
	t.Run("empty feature and files are explicit", func(t *testing.T) {
		bare, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{ScopeID: "scope:b"}}}, RenderOptions{})
		if !strings.Contains(bare, "feature: (none)") {
			t.Errorf("empty feature did not render as (none):\n%s", bare)
		}
		if !strings.Contains(bare, "modified_files: (none observed)") {
			t.Errorf("empty file set did not render as (none observed):\n%s", bare)
		}
		if !strings.Contains(bare, "last_action=none") || strings.Contains(bare, "ticks_ago") {
			t.Errorf("never-acted scope rendered a tick age:\n%s", bare)
		}
		if !strings.Contains(bare, "no tool calls yet") {
			t.Errorf("empty trail did not render the reference's no-calls line:\n%s", bare)
		}
	})
	t.Run("no tick renders without a tick number", func(t *testing.T) {
		bare, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{ScopeID: "scope:b"}}}, RenderOptions{})
		if strings.Contains(bare, "TICK") {
			t.Errorf("tick 0 rendered a TICK header:\n%s", bare)
		}
		if !strings.HasPrefix(bare, "1 agents (0 live, 1 finished)") {
			t.Errorf("header = %q", strings.SplitN(bare, "\n", 2)[0])
		}
	})
	t.Run("snapshot cap is surfaced", func(t *testing.T) {
		capped, _ := Render(supervisionapi.Snapshot{
			Agents:  []supervisionapi.AgentSnapshot{{ScopeID: "scope:b", StillRunning: true}},
			Dropped: 7,
		}, RenderOptions{Tick: 2})
		if !strings.Contains(capped, "(+7 agents over the snapshot cap)") {
			t.Errorf("Dropped was not surfaced:\n%s", capped)
		}
	})
	t.Run("note discloses degraded facts", func(t *testing.T) {
		build := func(note string) string {
			txt, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{
				ScopeID: "scope:quiet", Role: "developer", StillRunning: true, Note: note,
			}}}, RenderOptions{Tick: 1})
			return txt
		}
		withNote := build("trace read failed: permission denied")
		if !strings.Contains(withNote, "note: trace read failed: permission denied") {
			t.Errorf("Note was not rendered:\n%s", withNote)
		}
		if withNote == build("") {
			t.Error("an agent with a Note renders identically to one without: the two are indistinguishable")
		}
		long := build(strings.Repeat("trace read failed  ", 40))
		seen := false
		for _, line := range strings.Split(long, "\n") {
			rest, ok := strings.CutPrefix(line, "note: ")
			if !ok {
				continue
			}
			seen = true
			if n := utf8.RuneCountInString(rest); n > maxNoteRunes {
				t.Errorf("note line has %d runes, want <= %d", n, maxNoteRunes)
			}
			if !strings.HasSuffix(rest, "…") {
				t.Errorf("clipped note carries no ellipsis: %q", rest)
			}
		}
		if !seen {
			t.Errorf("a long note did not render at all:\n%s", long)
		}
	})
	t.Run("note survives the finished-agent header cut", func(t *testing.T) {
		// The note is part of the kept header/stats section, not the
		// trail, so the tier that hides a finished agent's role line must
		// not take it with it.
		r := renderer{tick: 1, finished: 1}
		var b strings.Builder
		r.writeBlock(&b, labeledAgent{label: "w1", snap: supervisionapi.AgentSnapshot{
			ScopeID: "scope:done", Role: "developer", Note: "trace read failed: EOF",
		}}, limits{trail: 0, hideFinishedHeader: true})
		block := b.String()
		if strings.Contains(block, "role:") {
			t.Errorf("the finished header was not hidden:\n%s", block)
		}
		if !strings.Contains(block, "note: trace read failed: EOF") {
			t.Errorf("the note was dropped with the finished header:\n%s", block)
		}
	})
	t.Run("the file list says what it cut", func(t *testing.T) {
		wide, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{
			ScopeID: "scope:wide", StillRunning: true,
			ModifiedFiles: []string{"a.go", "b.go", "c.go", "d.go", "e.go",
				"f.go", "g.go", "h.go", "i.go", "j.go"},
		}}}, RenderOptions{Tick: 1})
		want := "modified_files: a.go, b.go, c.go, d.go, e.go, f.go, g.go, h.go (+2 more)"
		if !strings.Contains(wide, want) {
			t.Errorf("the file cap was silent, want %q:\n%s", want, wide)
		}
		if strings.Contains(wide, "i.go") {
			t.Errorf("rendered past the file cap:\n%s", wide)
		}
	})
	t.Run("in-flight calls are capped and say so", func(t *testing.T) {
		calls := make([]supervisionapi.InFlightCall, 0, 7)
		for i := 0; i < 7; i++ {
			calls = append(calls, supervisionapi.InFlightCall{
				Name: fmt.Sprintf("tool%d", i), Args: "x", Age: time.Duration(i) * time.Second,
			})
		}
		busy, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{
			ScopeID: "scope:busy", StillRunning: true, InFlight: calls,
		}}}, RenderOptions{Tick: 1})
		want := "in-flight: tool0(x) age=0s, tool1(x) age=1s, tool2(x) age=2s, " +
			"tool3(x) age=3s, tool4(x) age=4s (+2 more)"
		if !strings.Contains(busy, want) {
			t.Errorf("the in-flight cap is missing or silent, want %q:\n%s", want, busy)
		}
		if strings.Contains(busy, "tool5") {
			t.Errorf("rendered past the in-flight cap:\n%s", busy)
		}
	})
	t.Run("multi-byte text renders and stays valid UTF-8", func(t *testing.T) {
		cjk, _ := Render(supervisionapi.Snapshot{Agents: []supervisionapi.AgentSnapshot{{
			ScopeID:      "scope:cjk",
			Role:         "golang-developer",
			Feature:      "日本語の機能説明を書く — " + strings.Repeat("長", 400),
			StillRunning: true,
			Trail: []supervisionapi.ToolCallDigest{
				{Name: "write", Args: "日本語.txt", Paired: true, Success: true},
			},
		}}}, RenderOptions{Tick: 1})
		if !utf8.ValidString(cjk) {
			t.Errorf("rendered text is not valid UTF-8:%q", cjk)
		}
		if !strings.Contains(cjk, "## AGENT w1 (scope scope:cjk)") {
			t.Errorf("the CJK block did not render:\n%s", cjk)
		}
		if !strings.Contains(cjk, "日本語.txt") {
			t.Errorf("multi-byte args were lost:\n%s", cjk)
		}
		for _, line := range strings.Split(cjk, "\n") {
			rest, ok := strings.CutPrefix(line, "feature: ")
			if !ok {
				continue
			}
			if n := utf8.RuneCountInString(rest); n > supervisionapi.MaxFeatureRunes {
				t.Errorf("feature line has %d runes, want <= %d (the ellipsis counts)",
					n, supervisionapi.MaxFeatureRunes)
			}
		}
	})
}

func TestRender_Budget(t *testing.T) {
	// Worst case: 50 agents × 25 trailing calls × ~160-byte args is ~105K
	// estimated tokens against a 3K budget.
	snap := pathologicalSnapshot(50, 25)
	text, labels := Render(snap, RenderOptions{Tick: 1, State: map[string]TickState{
		"scope:s1": {LastAction: "redirect", TicksAgo: 3},
	}})

	t.Run("within the default budget", func(t *testing.T) {
		if got := estimateTokens(text); got > DefaultRenderBudgetTokens {
			t.Errorf("estimateTokens = %d, exceeds budget %d (len %d)", got, DefaultRenderBudgetTokens, len(text))
		}
	})
	t.Run("every agent keeps its label", func(t *testing.T) {
		if len(labels) != 50 {
			t.Errorf("got %d labels, want 50 — degradation must not renumber the scopes", len(labels))
		}
	})
	t.Run("degradation is honest", func(t *testing.T) {
		omitted := strings.Contains(text, "agents omitted for budget")
		trimmed := strings.Contains(text, "shown") || strings.Contains(text, "trail omitted")
		if !omitted && !trimmed {
			t.Errorf("budget degradation left no trace in the text:\n%s", text)
		}
		if omitted {
			// The header still reports the true agent count, split live
			// from finished (17 of the 50 are finished).
			if !strings.Contains(text, "50 agents (33 live, 17 finished") {
				t.Errorf("header lost the true agent count:\n%s", strings.SplitN(text, "\n", 2)[0])
			}
		}
	})
	t.Run("footer survives degradation", func(t *testing.T) {
		if !strings.HasSuffix(text, footerJSON+"\n") {
			t.Errorf("text does not end with the JSON instruction:\n%q", text[len(text)-120:])
		}
	})
	t.Run("trail trimming is visible", func(t *testing.T) {
		// One agent, a long trail, a tiny budget: the trail must shrink
		// and say so.
		one := pathologicalSnapshot(1, 25)
		small, _ := Render(one, RenderOptions{BudgetTokens: 400})
		if !strings.Contains(small, "last ") || !strings.Contains(small, " shown") {
			t.Errorf("trimmed trail was not reported:\n%s", small)
		}
		if got := estimateTokens(small); got > 400 {
			t.Errorf("estimateTokens = %d, exceeds 400", got)
		}
	})
	t.Run("custom budget is honored", func(t *testing.T) {
		small, _ := Render(snap, RenderOptions{BudgetTokens: 200})
		if got := estimateTokens(small); got > 200 {
			t.Errorf("estimateTokens = %d, exceeds custom budget 200", got)
		}
	})
	t.Run("absolute byte ceiling", func(t *testing.T) {
		// Not routed through estimateTokens: the renderer has to fit the
		// budget in real bytes, not just agree with its own estimator.
		// budget/1.3*4 bytes is the byte count estimateTokens maps back to
		// the budget, so a render that fits must not exceed it.
		exact := float64(DefaultRenderBudgetTokens) / 1.3 * 4
		if n := len(text); n > int(exact) {
			t.Errorf("len(text) = %d bytes, exceeds the absolute ceiling %d", n, int(exact))
		}
	})
	t.Run("the header splits live from finished", func(t *testing.T) {
		// six agents, two of them finished (i%3 == 0).
		mixed, _ := Render(pathologicalSnapshot(6, 1), RenderOptions{Tick: 3})
		if !strings.Contains(mixed, "TICK 3 — 6 agents (4 live, 2 finished)") {
			t.Errorf("header = %q", strings.SplitN(mixed, "\n", 2)[0])
		}
	})
	t.Run("a budget below the floor still terminates", func(t *testing.T) {
		floor, _ := Render(snap, RenderOptions{BudgetTokens: 1})
		if floor == "" {
			t.Fatal("BudgetTokens 1 rendered nothing; the irreducible floor must still render")
		}
		if !strings.Contains(floor, "agents omitted for budget") {
			t.Errorf("the floor path dropped the omission footer:\n%s", floor)
		}
		if !strings.HasSuffix(floor, footerJSON+"\n") {
			t.Errorf("the floor path dropped the instruction:\n%q", floor)
		}
	})
}

func TestRender_Footer(t *testing.T) {
	text, _ := Render(supervisionapi.Snapshot{}, RenderOptions{})
	if !strings.Contains(text, `"agent": "..."`) {
		t.Errorf("footer is missing the agent key:\n%s", text)
	}
	for _, action := range []string{"none", "steer", "redirect", "revert"} {
		if !strings.Contains(text, `"`+action+`"`) {
			t.Errorf("footer is missing the action %q", action)
		}
	}
	// One format for both modes: the footer closes every render, even an
	// empty snapshot's.
	if !strings.HasSuffix(text, footerJSON+"\n") {
		t.Errorf("text does not end with the footer:\n%q", text)
	}
}

func TestEstimateTokens(t *testing.T) {
	// chars/4 × 1.3, ceiled: 400 chars is 100 × 1.3 = 130 tokens. Pinned as
	// a number so the budget tests cannot agree with a broken estimator.
	if got := estimateTokens(strings.Repeat("x", 400)); got != 130 {
		t.Errorf("estimateTokens(400 chars) = %d, want 130", got)
	}
	if got := estimateTokens("\n"); got != 1 {
		t.Errorf("estimateTokens(1 char) = %d, want 1 (never zero)", got)
	}
}

func TestClipRunes(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		max          int
		wantRunes    int
		wantEllipsis bool
	}{
		{name: "under the cap", in: "short", max: 10, wantRunes: 5},
		{name: "exactly the cap", in: "12345", max: 5, wantRunes: 5},
		{name: "ellipsis fits inside the cap", in: strings.Repeat("a", 20), max: 10, wantRunes: 10, wantEllipsis: true},
		{name: "max one leaves only the ellipsis", in: "ab", max: 1, wantRunes: 1, wantEllipsis: true},
		{name: "multi-byte", in: strings.Repeat("日", 200), max: 160, wantRunes: 160, wantEllipsis: true},
		{name: "no cap", in: strings.Repeat("a", 20), max: 0, wantRunes: 20},
		{name: "negative max is no cap", in: "abc", max: -3, wantRunes: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clipRunes(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Fatalf("clipRunes produced invalid UTF-8: %q", got)
			}
			if n := utf8.RuneCountInString(got); n != tc.wantRunes {
				t.Errorf("rune count = %d, want %d (%q)", n, tc.wantRunes, got)
			}
			if has := strings.HasSuffix(got, "…"); has != tc.wantEllipsis {
				t.Errorf("ellipsis = %v, want %v (%q)", has, tc.wantEllipsis, got)
			}
		})
	}
}

func TestTrimTrail_KeepsNewest(t *testing.T) {
	trail := []supervisionapi.ToolCallDigest{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}
	tests := []struct {
		name string
		max  int
		want string
	}{
		{name: "all", max: -1, want: "a,b,c,d"},
		{name: "newest two", max: 2, want: "c,d"},
		{name: "newest one", max: 1, want: "d"},
		{name: "none", max: 0, want: ""},
		{name: "cap above length", max: 9, want: "a,b,c,d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := trimTrail(trail, tc.max)
			names := make([]string, 0, len(got))
			for _, c := range got {
				names = append(names, c.Name)
			}
			if strings.Join(names, ",") != tc.want {
				t.Errorf("trimTrail(%d) = %v, want %q", tc.max, names, tc.want)
			}
		})
	}
}

// pathologicalSnapshot builds n agents, each with trail trailing calls whose
// args are long enough to blow any budget, and a max-length feature.
func pathologicalSnapshot(n, trail int) supervisionapi.Snapshot {
	agents := make([]supervisionapi.AgentSnapshot, 0, n)
	for i := 0; i < n; i++ {
		calls := make([]supervisionapi.ToolCallDigest, 0, trail)
		for j := 0; j < trail; j++ {
			calls = append(calls, supervisionapi.ToolCallDigest{
				Name:    "bash",
				Args:    fmt.Sprintf("go test ./internal/pkg%d/... -run TestSomething%d -count=1 -v", j, j) + strings.Repeat("x", 100),
				Paired:  true,
				Success: j%7 != 0,
				Error:   "exit status 1: the build failed somewhere deep in the module graph",
			})
		}
		agents = append(agents, supervisionapi.AgentSnapshot{
			ScopeID:           fmt.Sprintf("scope:s%d", i+1),
			Role:              "golang-developer",
			Feature:           strings.Repeat("implement the feature ", 60),
			StillRunning:      i%3 != 0,
			StartedAt:         fixtureTime,
			Reversible:        i%2 == 0,
			Redirectable:      true,
			Trail:             calls,
			ModifiedFiles:     []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go", "i.go", "j.go"},
			Calls:             300 + i,
			Errors:            4,
			ConsecutiveErrors: i % 5,
			Through:           300 + i,
		})
		if i%3 == 0 {
			agents[i].FinishedAt = fixtureTime.Add(time.Duration(i) * time.Minute)
		}
	}
	return supervisionapi.Snapshot{At: fixtureTime, Agents: agents}
}

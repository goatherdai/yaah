package parity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
)

// ledgerPath is the in-repo parity ledger. The external gap report is a
// snapshot; this file and the code are the source of truth.
const ledgerPath = "../../docs/goatherd-parity.md"

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestLedgerPinsMatchGoMod fails when a module pin moves in go.mod without the
// ledger being updated (or vice versa). The pins are the upstream contracts the
// parity claims rest on.
func TestLedgerPinsMatchGoMod(t *testing.T) {
	gomod := read(t, "../../go.mod")
	ledger := read(t, ledgerPath)

	pins := []string{
		"github.com/goatherdai/goatherd-kernel v0.22.0",
		"github.com/goatherdai/goatherd-runtime v0.27.0",
		"github.com/goatherdai/goatherd-runtime/sandbox/croft v0.2.1-0.20261118192512-b4d03a8d225a",
		"github.com/goatherdai/goatherd-runtime/sandbox/landlock v0.1.3-0.20261118192512-b4d03a8d225a",
		"github.com/goatherdai/goatherd-runtime/sandbox/winacl v0.1.2-0.20261118192512-b4d03a8d225a",
		"github.com/goatherdai/goatherd-runtime/sandbox/seatbelt v0.1.1-0.20261118192512-b4d03a8d225a",
		"github.com/goatherdai/croft v0.9.3-0.20261118191751-716dbb80a6dd",
	}
	for _, pin := range pins {
		module, version, ok := strings.Cut(pin, " ")
		if !ok {
			t.Fatalf("bad pin %q", pin)
		}
		if !strings.Contains(gomod, module+" "+version) {
			t.Errorf("go.mod no longer contains %s — update the parity ledger and this guard together", pin)
		}
		if !strings.Contains(ledger, version) {
			t.Errorf("ledger is missing the %s pin %s — refresh docs/goatherd-parity.md", module, version)
		}
	}
}

// TestLedgerNamesEveryWorld keeps the documented vocabulary and the resolver's
// constants in step. A new world kind must be added to both.
func TestLedgerNamesEveryWorld(t *testing.T) {
	ledger := read(t, ledgerPath)
	for _, kind := range []subagent.WorldKind{
		subagent.WorldAuto,
		subagent.WorldWorktree,
		subagent.WorldContainer,
		subagent.WorldShared,
	} {
		if !strings.Contains(ledger, "`"+string(kind)+"`") && !strings.Contains(ledger, string(kind)) {
			t.Errorf("ledger does not name the %q world", kind)
		}
	}
}

// TestLedgerNamesTheContainmentFamily pins the family's members. Dropping a
// platform from the ledger while it ships is a documentation defect; the pin
// list above catches the module version drift.
func TestLedgerNamesTheContainmentFamily(t *testing.T) {
	ledger := strings.ToLower(read(t, ledgerPath))
	for _, member := range []string{"landlock", "winacl", "seatbelt"} {
		if !strings.Contains(ledger, member) {
			t.Errorf("ledger does not name the %q containment member", member)
		}
	}
}

// TestLedgerNamesUsefulSurfaceOwners keeps the full parity scorecard tied to
// the epic that owns it. The dispatch-world ledger used to list closed beads as
// "remaining"; this guard makes the owner list falsifiable.
func TestLedgerNamesUsefulSurfaceOwners(t *testing.T) {
	ledger := read(t, ledgerPath)
	for _, ref := range []string{
		"yaah-hxk3",
		"yaah-hxk3.1",
		"yaah-hxk3.2",
		"yaah-hxk3.3",
		"yaah-hxk3.4",
		"yaah-hxk3.5",
		"yaah-kit",
		"yaah-1tkp.1",
		"yaah-bp4",
	} {
		if !strings.Contains(ledger, "`"+ref+"`") {
			t.Errorf("parity ledger does not name %s as a bounded bead reference; the scorecard owner list drifted", ref)
		}
	}
	marker := "Remaining useful framework work"
	at := strings.Index(ledger, marker)
	if at < 0 {
		t.Fatalf("parity ledger has no %q section", marker)
	}
	remaining := ledger[at:]
	for _, closed := range []string{"yaah-vlwm", "yaah-qlf9"} {
		if strings.Contains(remaining, "`"+closed+"`") {
			t.Errorf("parity ledger still lists closed bead %s as remaining work", closed)
		}
	}
}

// TestLedgerStatesDeliberateBypasses pins the decisions that explain why the
// Python task/materialize surfaces are absent. Without these, a reader sees an
// unimplemented feature rather than a recorded divergence.
func TestLedgerStatesDeliberateBypasses(t *testing.T) {
	ledger := read(t, ledgerPath)
	for _, marker := range []string{"D6 — TaskControl", "D7 — materialize"} {
		if !strings.Contains(ledger, marker) {
			t.Errorf("parity ledger does not record the deliberate bypass %q", marker)
		}
	}
}

// TestLedgerScorecardVocabulary parses the scorecard table and pins its layer
// names and verdict words, so a dropped layer, an added layer, or a verdict
// renamed in a single row all fail the guard. A bare contains() over the whole
// file would still pass if a word drifted into prose only, which is exactly the
// drift this guard exists to catch.
func TestLedgerScorecardVocabulary(t *testing.T) {
	layers := []string{
		"Kernel / record",
		"Retained runs / settlement",
		"Context continuation",
		"Retained-result threading",
		"Dispatch worlds",
		"Authority",
		"Per-binding grants",
		"Egress postures",
		"Supervision",
		"Task / step / combinators",
		"Contexts / effect fold",
		"Citation checking",
		"Export / transform",
		"Devices / remote carriers",
		"Provider SDK shape",
	}
	verdicts := []string{"Shipped", "Partial", "Declined", "Not ported", "Different surface"}

	rows := scorecardRows(t, read(t, ledgerPath))
	if len(rows) != len(layers) {
		t.Errorf("scorecard has %d layer rows, want %d", len(rows), len(layers))
	}
	expected := make(map[string]bool, len(layers))
	for _, layer := range layers {
		expected[layer] = true
		status, ok := rows[layer]
		if !ok {
			t.Errorf("scorecard does not name the %q layer", layer)
			continue
		}
		matched := false
		for _, v := range verdicts {
			if strings.HasPrefix(status, "**"+v) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("scorecard row %q has unrecognized verdict %q", layer, status)
		}
	}
	for layer := range rows {
		if !expected[layer] {
			t.Errorf("scorecard names an unexpected %q layer", layer)
		}
	}
}

// scorecardRows returns the scorecard table's layer→verdict-cell mapping. The
// scorecard is the only table under "## Scope and scorecard"; a row is a `| … |`
// line whose final cell carries a bold verdict token.
func scorecardRows(t *testing.T, ledger string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	in := false
	for _, line := range strings.Split(ledger, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## Scope and scorecard") {
			in = true
			continue
		}
		if in && strings.HasPrefix(trimmed, "## ") {
			break
		}
		if !in || !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 5 {
			continue
		}
		layer := strings.TrimSpace(cols[1])
		status := strings.TrimSpace(cols[4])
		if !strings.Contains(status, "**") {
			continue // header or separator row
		}
		rows[layer] = status
	}
	if len(rows) == 0 {
		t.Fatalf("no scorecard rows parsed under %q", "## Scope and scorecard")
	}
	return rows
}

package dispatch

// report_paging_test.go is the yaah-okcl gate on the dispatch lane: a report
// bigger than the envelope's head window must arrive as a bounded head that
// names its total and its retrieval route, while the SEALED copy stays
// verbatim and pageable — through a continuation too. The tests run the real
// seal path (a real temp git repo, real scope manager, real sealed output), so
// "the full report is retained" is a fact about the artifacts, not about a
// string this package kept in memory.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/reportpage"
	"github.com/goatherdai/yaah/internal/types"
)

// bigReport builds a report well past both the envelope head and the default
// page, with multibyte runes so a page boundary that ignored UTF-8 would
// produce invalid text and pages that failed to concatenate.
func bigReport(label string) string {
	var b strings.Builder
	for i := 0; b.Len() < 60<<10; i++ {
		fmt.Fprintf(&b, "%s line %05d: the quick brown fox — ünïcodé ✓\n", label, i)
	}
	return b.String()
}

// bigReportRuntime builds a worktree-lane runtime whose body writes one file
// (so the seal has a real delta) and reports the given text.
func bigReportRuntime(t *testing.T, fx *sessionFixtures, repo, rootID, report string, out string) RetainedRuntime {
	t.Helper()
	return RetainedRuntime{
		Runner: func(ctx context.Context, _ string, params SubAgentParams) (string, error) {
			writeTestFile(t, params.Workdir, out, "change\n")
			// A continuation needs a captured conversation, so every body
			// leaves one (yaah-okcl continue/fork preservation).
			jobs.CaptureSubAgentTranscript(ctx, []types.Message{
				types.SystemMsg("sys"),
				types.UserMsg("do the work"),
			})
			return report, nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    NewProposalRegistry(),
	}
}

// sealedReport reads the report back out of a proposal's durable sealed
// metadata — the copy `proposal show` pages, and the one a restart rehydrates.
func sealedReport(t *testing.T, p *retainedProposal) string {
	t.Helper()
	if p == nil || p.output == nil {
		t.Fatalf("proposal has no sealed output")
	}
	meta, _, err := proposalMetaFromOutput(p.output)
	if err != nil {
		t.Fatalf("read sealed proposal metadata: %v", err)
	}
	return meta.Result
}

// TestRetained_ReportHeadIsBoundedAndTheSealedCopyIsVerbatim pins the core
// contract: what the envelope shows is a HEAD plus a route, and what gets
// sealed is the whole report.
func TestRetained_ReportHeadIsBoundedAndTheSealedCopyIsVerbatim(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	report := bigReport("seal")
	if len(report) <= 50<<10 {
		t.Fatalf("fixture report is %d bytes, want >50 KiB", len(report))
	}
	rt := bigReportRuntime(t, fx, repo, rootID, report, "out.txt")

	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "developer"}, 30*time.Second, false)
	if err != nil {
		t.Fatalf("StartRetainedRun: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "proposal" || env.ProposalID == "" {
		t.Fatalf("envelope = %+v, want a proposal (%s)", env, env.Error)
	}

	// The envelope is a head, not the report.
	if len(env.Result) >= len(report) {
		t.Fatalf("envelope result is %d bytes of a %d-byte report — nothing was bounded", len(env.Result), len(report))
	}
	if !strings.HasPrefix(report, bigReportHead(env.Result)) {
		t.Errorf("the envelope result must be the report's head verbatim")
	}
	if env.ResultTotalBytes != len(report) {
		t.Errorf("result_total_bytes = %d, want the true total %d", env.ResultTotalBytes, len(report))
	}
	if env.ResultLimit != reportpage.DefaultBytes {
		t.Errorf("result_limit = %d, want %d", env.ResultLimit, reportpage.DefaultBytes)
	}
	if env.ResultNextOffset == 0 || env.ResultNextOffset >= len(report) {
		t.Errorf("result_next_offset = %d, want an in-range offset past the head", env.ResultNextOffset)
	}
	// The marker names the total and the route, including the handle, so the
	// tail is findable without guessing.
	for _, want := range []string{
		"[report truncated",
		fmt.Sprintf("of %d", len(report)),
		"proposal show",
		fmt.Sprintf(`"proposal_id":%q`, env.ProposalID),
		fmt.Sprintf(`"report_offset":%d`, env.ResultNextOffset),
	} {
		if !strings.Contains(env.Result, want) {
			t.Errorf("the truncation marker must contain %q; got:\n%s", want, tailOf(env.Result, 300))
		}
	}

	// The retained copies are verbatim: the in-session proposal AND the sealed
	// metadata a restart rehydrates (the one the old trimResult capped).
	p, err := rt.Proposals.get(env.ProposalID)
	if err != nil {
		t.Fatalf("proposal %s not in the registry: %v", env.ProposalID, err)
	}
	if p.result != report {
		t.Errorf("in-session proposal report is %d bytes, want the verbatim %d", len(p.result), len(report))
	}
	if got := sealedReport(t, p); got != report {
		t.Errorf("SEALED report is %d bytes, want the verbatim %d — the head must never be what is retained", len(got), len(report))
	}
}

// TestProposalShow_PagesTheSealedReport pins the retrieval route: the head page
// by default, exact requested pages, exact concatenation, and an out-of-range
// offset that answers an empty page with the true total instead of failing.
func TestProposalShow_PagesTheSealedReport(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	report := bigReport("show")
	rt := bigReportRuntime(t, fx, repo, rootID, report, "out.txt")

	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "developer"}, 30*time.Second, false)
	if err != nil {
		t.Fatalf("StartRetainedRun: %v", err)
	}
	sealed := decodeProposalEnvelope(t, res)
	if sealed.Status != "proposal" {
		t.Fatalf("status = %q, want proposal (%s)", sealed.Status, sealed.Error)
	}

	tool := &ProposalTool{Proposals: rt.Proposals, Runtime: &rt}
	show := func(args string) proposalEnvelope {
		t.Helper()
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("proposal show %s: %v", args, err)
		}
		var env proposalEnvelope
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("show response is not JSON: %v", err)
		}
		return env
	}
	showPage := func(offset, limit int) proposalEnvelope {
		t.Helper()
		return show(fmt.Sprintf(`{"action":"show","proposal_id":%q,"report_offset":%d,"report_limit":%d}`,
			sealed.ProposalID, offset, limit))
	}

	// Default read: the head page, with the paging facts but NO marker (a
	// marker spliced into a page would stop pages concatenating).
	head := show(fmt.Sprintf(`{"action":"show","proposal_id":%q}`, sealed.ProposalID))
	if strings.Contains(head.Result, "report truncated") {
		t.Errorf("a paged read must not embed a truncation marker; got:\n%s", tailOf(head.Result, 200))
	}
	if !strings.HasPrefix(report, head.Result) {
		t.Errorf("the default page must be the report's head verbatim")
	}
	if n := len(head.Result); n > reportpage.DefaultBytes || n < reportpage.DefaultBytes-3 {
		t.Errorf("default page is %d bytes, want ~%d (rune-snapped)", n, reportpage.DefaultBytes)
	}
	if head.ResultTotalBytes != len(report) || head.ResultOffset != 0 || head.ResultLimit != reportpage.DefaultBytes {
		t.Errorf("head paging facts = total %d offset %d limit %d, want %d/0/%d",
			head.ResultTotalBytes, head.ResultOffset, head.ResultLimit, len(report), reportpage.DefaultBytes)
	}
	if head.ResultNextOffset != len(head.Result) {
		t.Errorf("result_next_offset = %d, want the end of the returned page %d", head.ResultNextOffset, len(head.Result))
	}

	// Every page, in order, concatenates to the sealed bytes exactly.
	var got strings.Builder
	offset := 0
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatalf("paging never reached the end (offset %d of %d)", offset, len(report))
		}
		page := showPage(offset, 7001)
		if page.ResultOffset != offset {
			t.Fatalf("page offset = %d, want the requested %d", page.ResultOffset, offset)
		}
		got.WriteString(page.Result)
		if page.ResultNextOffset == 0 {
			break
		}
		if page.ResultNextOffset <= offset {
			t.Fatalf("paging stalled at offset %d (next %d)", offset, page.ResultNextOffset)
		}
		offset = page.ResultNextOffset
	}
	if got.String() != report {
		t.Fatalf("paged bytes (%d) do not equal the sealed report (%d)", got.Len(), len(report))
	}

	// Out of range is an empty page plus the truth, not an error.
	far := showPage(len(report)+5000, 100)
	if far.Result != "" {
		t.Errorf("out-of-range page = %q, want empty", far.Result)
	}
	if far.ResultTotalBytes != len(report) {
		t.Errorf("out-of-range result_total_bytes = %d, want %d", far.ResultTotalBytes, len(report))
	}
	if far.ResultNextOffset != 0 {
		t.Errorf("out-of-range result_next_offset = %d, want 0", far.ResultNextOffset)
	}
}

// TestRetained_ContinuedReportIsRetainedVerbatimAndPageable pins that a
// continuation is not a second-class report: the SUPERSEDING proposal seals its
// own report verbatim and pages it the same way, so nothing is lost across a
// continuation.
func TestRetained_ContinuedReportIsRetainedVerbatimAndPageable(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	first := bigReport("first")
	second := bigReport("second")

	var calls int
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, _ string, params SubAgentParams) (string, error) {
			calls++
			report := first
			if calls > 1 {
				report = second
				writeTestFile(t, params.Workdir, "two.txt", "second turn\n")
			} else {
				writeTestFile(t, params.Workdir, "one.txt", "first turn\n")
			}
			jobs.CaptureSubAgentTranscript(ctx, []types.Message{
				types.SystemMsg("sys"),
				types.UserMsg("do the work"),
			})
			return report, nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    NewProposalRegistry(),
	}

	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "developer"}, 30*time.Second, false)
	if err != nil {
		t.Fatalf("StartRetainedRun: %v", err)
	}
	orig := decodeProposalEnvelope(t, res)
	if orig.Status != "proposal" {
		t.Fatalf("original status = %q, want proposal (%s)", orig.Status, orig.Error)
	}

	res2, err := ContinueRetainedRun(context.Background(), rt, orig.ProposalID, 3, "", true)
	if err != nil {
		t.Fatalf("ContinueRetainedRun: %v", err)
	}
	cont := decodeProposalEnvelope(t, res2)
	if cont.Status != "proposal" || cont.ProposalID == "" || cont.ProposalID == orig.ProposalID {
		t.Fatalf("continuation envelope = %+v, want a new superseding proposal (%s)", cont, cont.Error)
	}

	// The superseding proposal's sealed report is the continuation's own text,
	// verbatim — not the source's, and not a head.
	p, err := rt.Proposals.get(cont.ProposalID)
	if err != nil {
		t.Fatalf("continuation %s not in the registry: %v", cont.ProposalID, err)
	}
	if got := sealedReport(t, p); got != second {
		t.Fatalf("continued sealed report is %d bytes, want the verbatim %d", len(got), len(second))
	}
	if cont.ResultTotalBytes != len(second) {
		t.Errorf("continuation result_total_bytes = %d, want %d", cont.ResultTotalBytes, len(second))
	}
	if !strings.Contains(cont.Result, "proposal show") || !strings.Contains(cont.Result, cont.ProposalID) {
		t.Errorf("the continuation's head must name its own retrieval route; got:\n%s", tailOf(cont.Result, 300))
	}

	// And it pages exactly like an original.
	tool := &ProposalTool{Proposals: rt.Proposals, Runtime: &rt}
	rest, err := tool.Execute(context.Background(), fmt.Sprintf(
		`{"action":"show","proposal_id":%q,"report_offset":%d,"report_limit":8000}`,
		cont.ProposalID, cont.ResultNextOffset))
	if err != nil {
		t.Fatalf("show continuation: %v", err)
	}
	var tail proposalEnvelope
	if err := json.Unmarshal([]byte(rest), &tail); err != nil {
		t.Fatalf("show response is not JSON: %v", err)
	}
	if tail.ResultOffset != cont.ResultNextOffset {
		t.Errorf("continuation tail offset = %d, want %d", tail.ResultOffset, cont.ResultNextOffset)
	}
	if !strings.HasPrefix(second[tail.ResultOffset:], tail.Result) {
		t.Errorf("continuation tail is not the report's bytes at offset %d", tail.ResultOffset)
	}
	if want := bigReportHead(cont.Result) + tail.Result; !strings.HasPrefix(second, want) {
		t.Errorf("head plus tail do not reproduce the sealed continuation report")
	}
}

// bigReportHead strips the envelope's truncation marker so the head's bytes can
// be compared with the report it came from.
func bigReportHead(result string) string {
	if i := strings.LastIndex(result, "\n...[report truncated"); i >= 0 {
		return result[:i]
	}
	return result
}

// tailOf returns the last n bytes of s, for a readable failure message.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestRetained_UnrecoverableSealFailureMarkerSaysNotRetained pins PR #419
// review finding 2: when the seal fails WITHOUT a recoverable artifact, the
// isolated scope is discarded and no proposal is registered — so the report
// marker must not name a `proposal show` route (that id was never registered
// and show would answer "unknown proposal"). It must say the remainder was
// not retained.
func TestRetained_UnrecoverableSealFailureMarkerSaysNotRetained(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	report := bigReport("unrecoverable")
	rt := bigReportRuntime(t, fx, repo, rootID, report, "out.txt")
	// A plain error, not a *goatherd.SealError with ArtifactID+Handle: the
	// recoverable check fails and the envelope takes the error path.
	rt.sealOutput = func(context.Context, string, ...goatherd.SealOptions) (*goatherd.RetainedOutput, error) {
		return nil, errors.New("injected total seal failure")
	}

	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "developer"}, 30*time.Second, false)
	if err != nil {
		t.Fatalf("StartRetainedRun: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "error" {
		t.Fatalf("status = %q, want error", env.Status)
	}
	if strings.Contains(env.Result, "proposal show") || strings.Contains(env.Result, "proposal_id") {
		t.Errorf("the marker names a route that cannot serve it: %q", env.Result[max(0, len(env.Result)-200):])
	}
	if !strings.Contains(env.Result, "not retained") {
		t.Errorf("the marker must state the remainder was not retained: %q", env.Result[max(0, len(env.Result)-200):])
	}
	if env.ResultTotalBytes != len(report) {
		t.Errorf("result_total_bytes = %d, want %d: the head must still report the true size", env.ResultTotalBytes, len(report))
	}
}

// TestCapDurableReport pins the durable-seal bound (PR #419 review finding 5):
// sealed metadata and meta.json are written VERBATIM into durable storage, so
// a pathological multi-megabyte report needs the same kind of guard the
// transcript already has. Under the cap nothing changes; over it, the cut is
// made on a rune boundary and STATED in the sealed text.
func TestCapDurableReport(t *testing.T) {
	short := strings.Repeat("x", 1000)
	if got := capDurableReport(short); got != short {
		t.Error("a report under the cap must seal unchanged")
	}

	huge := strings.Repeat("y", maxDurableReportBytes+4096)
	got := capDurableReport(huge)
	if !strings.Contains(got, "durable cap") || !strings.Contains(got, "not retained") {
		t.Errorf("an over-cap report must state the cut, tail: %q", got[len(got)-160:])
	}
	if len(got) > maxDurableReportBytes+200 {
		t.Errorf("capped report = %d bytes, want at most the cap plus its marker", len(got))
	}

	// The cut lands on a rune boundary: 4-byte runes straddling the cap.
	multi := strings.Repeat("🀄", maxDurableReportBytes/2)
	gotM := capDurableReport(multi)
	cutAt := strings.Index(gotM, "\n...[report exceeded")
	if cutAt < 0 {
		t.Fatal("over-cap multibyte report was not marked")
	}
	if !utf8.ValidString(gotM[:cutAt]) {
		t.Error("the durable cap must cut on a rune boundary")
	}
}

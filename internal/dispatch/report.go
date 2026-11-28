package dispatch

// report.go is the dispatch lane's half of lazy report retrieval (yaah-okcl).
//
// A sealed report is retained VERBATIM wherever the lane already persists its
// artifacts: a worktree proposal's sealed metadata, a container proposal's
// meta.json, a background job's record. What an envelope carries is a bounded
// HEAD of it plus the total byte count and the retrieval route, so the tail is
// reachable instead of lost. `output_limit` keeps its meaning — it bounds what
// the runner produced — and never decides what is retained: the two are
// different questions, and one used to answer both.

import (
	"fmt"
	"github.com/goatherdai/yaah/internal/textclip"

	"github.com/goatherdai/yaah/internal/reportpage"
)

// reportHeadBytes is the envelope's head window for a dispatch report. It is
// the window trimResult has always applied, so paging changed the retrieval
// route rather than the size of a read.
const reportHeadBytes = reportpage.DefaultBytes

// maxDurableReportBytes caps the report copy sealed into a DURABLE artifact:
// the worktree lane's SealOptions.Metadata (a trace-store row) and the
// container lane's meta.json. Reports are model-authored text and the page
// limit is 1 MiB, so 8 MiB is generous; the cap exists because a pathological
// report — megabytes of accidental stdout — would otherwise be written
// verbatim into durable storage with no bound at all, the same guard rationale
// as the transcript's maxDurableTranscriptBytes. A capped report is sealed with
// its excess cut and the cut STATED, so the durable copy never silently
// pretends completeness, and the envelope pages the sealed (capped) text so
// result_total_bytes always matches what retrieval can serve.
const maxDurableReportBytes = 8 << 20

// capDurableReport bounds a report for durable sealing (see
// maxDurableReportBytes). Under the cap it returns the text unchanged.
func capDurableReport(s string) string {
	return textclip.Head(s, maxDurableReportBytes, fmt.Sprintf(
		"\n...[report exceeded the %d-byte durable cap; the remainder was not retained]...",
		maxDurableReportBytes))
}

// retrieveProposalReport renders the route a bounded proposal report names:
// `proposal show` paged at the byte offset the head stopped at. An empty id
// yields the no-route explanation, because a dispatch that produced no
// proposal has nothing to page.
func retrieveProposalReport(proposalID string) func(next int) string {
	return func(next int) string {
		if proposalID == "" {
			return ""
		}
		return fmt.Sprintf(`retrieve more with proposal show {"proposal_id":%q,"report_offset":%d,"report_limit":%d}`,
			proposalID, next, reportHeadBytes)
	}
}

// retrieveJobReport renders the same route for a background job's report,
// which `subagent_jobs status` pages by job id.
func retrieveJobReport(jobID string) func(next int) string {
	return func(next int) string {
		if jobID == "" {
			return ""
		}
		return fmt.Sprintf(`retrieve more with subagent_jobs status {"job_id":%q,"result_offset":%d,"result_limit":%d}`,
			jobID, next, reportHeadBytes)
	}
}

// setReport bounds a report for this envelope, recording what was withheld and
// how to page it. A report that fits is carried verbatim and adds no paging
// fields at all, so a short report's envelope needs no paging context (the
// pre-okcl trimResult also stripped trailing whitespace, which setReport no
// longer does — carried means carried).
func (e *proposalEnvelope) setReport(report string, retrieve func(next int) string) {
	page := reportpage.Head(report, reportHeadBytes)
	if !page.Truncated() {
		e.Result = report
		return
	}
	var route string
	if retrieve != nil {
		route = retrieve(page.Next())
	}
	e.Result = page.Text + reportpage.Marker(page, route)
	e.ResultTotalBytes = page.Total
	e.ResultLimit = page.Limit
	e.ResultNextOffset = page.Next()
}

// reportPage reads one page of a sealed report for a `proposal show`. Unlike an
// envelope head, a paged read carries no marker: the caller asked for exact
// bytes, and a marker spliced into the text would make pages fail to
// concatenate back to the report.
func reportPage(report string, offset, limit int) reportpage.Page {
	return reportpage.At(report, offset, limit)
}

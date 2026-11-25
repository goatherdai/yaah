package tools

// subagent_jobs_paging_test.go is the yaah-okcl gate on the background lane: a
// job whose result is larger than the status window reports a bounded head that
// names its total and the route, while the full text stays on the job record and
// is retrievable page by page through `subagent_jobs status`.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/reportpage"
)

// bigJobReport builds a result well past the status window, with multibyte
// runes so a page boundary that ignored UTF-8 would show up as invalid text.
func bigJobReport(label string) string {
	var b strings.Builder
	for i := 0; b.Len() < 60<<10; i++ {
		fmt.Fprintf(&b, "%s line %05d: the quick brown fox — ünïcodé ✓\n", label, i)
	}
	return b.String()
}

// waitForJob blocks until the job reaches a terminal state.
func waitForJob(t *testing.T, mgr *jobs.BackgroundJobs, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, ok := mgr.Wait(ctx, id)
	if !ok {
		t.Fatalf("job %s disappeared while waiting", id)
	}
	if st.StillRunning {
		t.Fatalf("job %s still running after the wait", id)
	}
}

// TestSubAgentJobs_StatusPagesTheFullResult pins the retrieval contract: the
// snapshot carries a head plus the total and the route, and any page of the full
// result can be read back and concatenated to the original output exactly.
func TestSubAgentJobs_StatusPagesTheFullResult(t *testing.T) {
	mgr := jobs.NewBackgroundJobs()
	defer mgr.CloseAndWait(5 * time.Second)

	report := bigJobReport("job")
	if len(report) <= 50<<10 {
		t.Fatalf("fixture result is %d bytes, want >50 KiB", len(report))
	}
	runner := func(context.Context, string, jobs.SubAgentParams) (string, error) {
		return report, nil
	}
	id, err := mgr.Launch(context.Background(), runner, "developer", "big report", "go", jobs.SubAgentParams{}, 30*time.Second)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	waitForJob(t, mgr, id)

	tool := &SubAgentJobsTool{Jobs: mgr}
	status := func(offset, limit int) jobs.BackgroundJobStatus {
		t.Helper()
		out, err := tool.executeStatus(id, offset, limit)
		if err != nil {
			t.Fatalf("executeStatus(%d, %d): %v", offset, limit, err)
		}
		var st jobs.BackgroundJobStatus
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			t.Fatalf("status response is not JSON: %v", err)
		}
		return st
	}

	// The default read is a bounded head that names the total and the route.
	head := status(0, 0)
	if !head.ResultAvailable {
		t.Error("result_available = false, want true for a finished job with output")
	}
	if len(head.Result) >= len(report) {
		t.Fatalf("status result is %d bytes of a %d-byte result — nothing was bounded", len(head.Result), len(report))
	}
	if !strings.HasPrefix(report, headText(head.Result)) {
		t.Errorf("the status head must be the result's head verbatim")
	}
	if head.ResultTotalBytes != len(report) {
		t.Errorf("result_total_bytes = %d, want %d", head.ResultTotalBytes, len(report))
	}
	if head.ResultNextOffset == 0 || head.ResultNextOffset >= len(report) {
		t.Errorf("result_next_offset = %d, want an in-range offset past the head", head.ResultNextOffset)
	}
	for _, want := range []string{"[report truncated", fmt.Sprintf("of %d", len(report)), "subagent_jobs status", `"job_id":"` + id + `"`} {
		if !strings.Contains(head.Result, want) {
			t.Errorf("the truncation marker must contain %q; got:\n%s", want, lastBytes(head.Result, 300))
		}
	}

	// The full text is still on the job record — the head never replaced it.
	if full, ok := mgr.Result(id); !ok || full != report {
		t.Errorf("the retained job result is not the verbatim report (len %d, want %d)", len(full), len(report))
	}

	// Every page concatenates to the original output exactly, and no page
	// carries a marker (a marker would break concatenation).
	var got strings.Builder
	offset := 0
	for pages := 0; ; pages++ {
		if pages > 200 {
			t.Fatalf("paging never reached the end (offset %d of %d)", offset, len(report))
		}
		page := status(offset, 9000)
		if page.ResultOffset != offset {
			t.Fatalf("page offset = %d, want the requested %d", page.ResultOffset, offset)
		}
		if strings.Contains(page.Result, "[report truncated") {
			t.Fatalf("a paged read must not embed a marker; got:\n%s", lastBytes(page.Result, 200))
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
		t.Fatalf("paged bytes (%d) do not equal the job result (%d)", got.Len(), len(report))
	}
}

// TestBackgroundJobs_ResultPage pins the underlying read: an exact page with the
// true total, clamping at the bounds, and a false second value for an unknown
// job (so a caller cannot mistake "no such job" for "empty page").
func TestBackgroundJobs_ResultPage(t *testing.T) {
	mgr := jobs.NewBackgroundJobs()
	defer mgr.CloseAndWait(5 * time.Second)

	report := bigJobReport("page")
	runner := func(context.Context, string, jobs.SubAgentParams) (string, error) { return report, nil }
	id, err := mgr.Launch(context.Background(), runner, "developer", "paged", "go", jobs.SubAgentParams{}, 30*time.Second)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	waitForJob(t, mgr, id)

	page, ok := mgr.ResultPage(id, 0, reportpage.DefaultBytes)
	if !ok {
		t.Fatalf("ResultPage(%s) reported an unknown job", id)
	}
	if page.Total != len(report) {
		t.Errorf("Total = %d, want %d", page.Total, len(report))
	}
	if page.Limit != reportpage.DefaultBytes {
		t.Errorf("Limit = %d, want %d", page.Limit, reportpage.DefaultBytes)
	}
	if !strings.HasPrefix(report, page.Text) || !page.Truncated() {
		t.Errorf("page is not the truncated head of the result")
	}

	far, ok := mgr.ResultPage(id, len(report)+1000, 100)
	if !ok {
		t.Errorf("an out-of-range offset must still answer for a known job")
	}
	if far.Text != "" || far.Total != len(report) {
		t.Errorf("out-of-range page = %q total %d, want empty text and total %d", far.Text, far.Total, len(report))
	}

	if _, ok := mgr.ResultPage("bg-does-not-exist", 0, 10); ok {
		t.Errorf("ResultPage reported a page for an unknown job")
	}
}

// headText strips a status head's truncation marker so its bytes can be compared
// with the result it came from.
func headText(result string) string {
	if i := strings.LastIndex(result, "\n...[report truncated"); i >= 0 {
		return result[:i]
	}
	return result
}

// lastBytes returns the last n bytes of s for a readable failure message.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

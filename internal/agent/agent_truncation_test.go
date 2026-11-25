package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testLoop() *Loop { return &Loop{} }

// testLoopWithSpill returns a Loop whose tool-result spill directory is
// an isolated temp dir (the composition root injects ToolSpillDir in
// production; tests own their directories).
func testLoopWithSpill(t *testing.T) *Loop {
	t.Helper()
	return &Loop{Config: LoopConfig{ToolSpillDir: t.TempDir()}}
}

func TestTruncateToolResult_shortResult(t *testing.T) {
	result := "short output"
	got := testLoopWithSpill(t).truncateToolResult(result)
	if got != result {
		t.Errorf("short result should be unchanged, got %q", got)
	}
}

func TestTruncateToolResult_lineCapped(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < defaultTruncateMaxLines+100; i++ {
		sb.WriteString("line\n")
	}
	result := sb.String()
	loop := testLoopWithSpill(t)
	got := loop.truncateToolResult(result)

	if !strings.Contains(got, "[output truncated at") {
		t.Errorf("line-capped result missing truncation marker: %s", got[len(got)-200:])
	}
	if !strings.Contains(got, "read tool with offset/limit") {
		t.Errorf("line-capped result missing recovery hint")
	}

	truncatedDir := loop.Config.ToolSpillDir
	entries, _ := os.ReadDir(truncatedDir)
	if len(entries) == 0 {
		t.Error("expected spillover file in truncated dir")
	}
	for _, e := range entries {
		p := filepath.Join(truncatedDir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if string(data) == result {
			return
		}
	}
	t.Error("did not find spillover file with full content")
}

func TestTruncateToolResult_byteCapped(t *testing.T) {
	var sb strings.Builder
	line := strings.Repeat("x", 80) + "\n"
	for i := 0; i < 1000; i++ {
		sb.WriteString(line)
	}
	result := sb.String()

	got := testLoopWithSpill(t).truncateToolResult(result)

	if len(got) > len(result) {
		t.Errorf("byte-capped result should be shorter than input")
	}
	if !strings.Contains(got, "[output truncated at") {
		t.Errorf("byte-capped result missing truncation marker")
	}
}

func TestTruncateToolResult_emptyResult(t *testing.T) {
	loop := testLoopWithSpill(t)
	got := loop.truncateToolResult("")
	if got != "" {
		t.Errorf("empty result should remain empty, got %q", got)
	}

	beforeEntries := countFiles(loop.Config.ToolSpillDir)
	_ = loop.truncateToolResult("")
	afterEntries := countFiles(loop.Config.ToolSpillDir)
	if afterEntries > beforeEntries {
		t.Error("empty result should not create spillover file")
	}
}

// TestTruncateToolResult_noSpillDirDeclaresNoSpool: with no spill directory the
// cut bytes exist nowhere, so the notice must SAY that instead of leaving a
// bare "truncated" marker that invites a search for a file nobody wrote
// (yaah-vw1v).
func TestTruncateToolResult_noSpillDirFallsBack(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < defaultTruncateMaxLines+100; i++ {
		sb.WriteString("line\n")
	}
	got := testLoop().truncateToolResult(sb.String())
	if !strings.Contains(got, "not stored") {
		t.Errorf("no-spool notice must state the dropped bytes are not stored: %q", tail(got, 240))
	}
	if !strings.Contains(got, "no spill directory") {
		t.Errorf("no-spool notice must say why: %q", tail(got, 240))
	}
	if strings.Contains(got, "full output at") {
		t.Errorf("no-spool notice must not name a spill path: %q", tail(got, 240))
	}
}

func TestTruncateToolResult_bothLimitsExceeded(t *testing.T) {
	line := strings.Repeat("x", 80) + "\n"
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		sb.WriteString(line)
	}
	result := sb.String()
	got := testLoopWithSpill(t).truncateToolResult(result)

	if !strings.Contains(got, "[output truncated at") {
		t.Errorf("result missing truncation marker when both limits exceeded")
	}
}

func TestTruncateToolResult_binaryContent(t *testing.T) {
	result := strings.Repeat("\x00\x01\x02\x03", 20000)
	got := testLoopWithSpill(t).truncateToolResult(result)
	if len(got) > len(result) {
		t.Errorf("binary content should be truncated")
	}
	if !strings.Contains(got, "[output truncated at") && !strings.Contains(got, "[truncated]") {
		t.Errorf("binary content result missing truncation marker")
	}
}

func TestTruncateToolResult_lazyDirCreation(t *testing.T) {
	spillDir := filepath.Join(t.TempDir(), "nested", "spill")
	loop := &Loop{Config: LoopConfig{ToolSpillDir: spillDir}}

	if _, err := os.Stat(spillDir); err == nil {
		t.Fatal("spill dir should not exist before test")
	}

	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		sb.WriteString("line\n")
	}
	_ = loop.truncateToolResult(sb.String())

	if _, err := os.Stat(spillDir); err != nil {
		t.Errorf("spill dir was not created lazily: %v", err)
	}
}

func TestCleanTruncatedDir(t *testing.T) {
	spillDir := t.TempDir()

	oldFile := filepath.Join(spillDir, "old.txt")
	newFile := filepath.Join(spillDir, "new.txt")

	os.WriteFile(oldFile, []byte("old"), 0o644)
	os.WriteFile(newFile, []byte("new"), 0o644)

	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(oldFile, oldTime, oldTime)

	cleanTruncatedDir(spillDir)

	if _, err := os.Stat(oldFile); err == nil {
		t.Error("old file should have been cleaned up")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Error("new file should still exist")
	}
}

func countFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// TestTruncateToolResult_singleLineBlobSpillsRawWithPagerSibling (yaah-vw1v,
// superseding yaah-was9.14): a single-line blob used to spill hard-wrapped, so
// the canonical file was a derived artifact that only reconstructed after
// joining lines and that grep/tail could not search. Now the canonical spill is
// the RAW bytes and the pager copy is a `.wrapped` sibling beside it.
func TestTruncateToolResult_singleLineBlobSpillsRawWithPagerSibling(t *testing.T) {
	result := `{"result":"` + strings.Repeat("report line\\n", 3000) + `"}`
	loop := testLoopWithSpill(t)

	got := loop.truncateToolResult(result)

	canonical, sibling := spillFiles(t, loop.Config.ToolSpillDir)
	raw, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != result {
		t.Errorf("canonical spill must be the original bytes: got %d bytes, want %d", len(raw), len(result))
	}
	wrapped, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatalf("pager sibling: %v", err)
	}
	if strings.Count(string(wrapped), "\n") < 2 {
		t.Error("pager sibling of a single-line blob should be wrapped into multiple lines")
	}
	if joined := strings.ReplaceAll(string(wrapped), "\n", ""); joined != result {
		t.Error("joining the pager sibling's lines must reconstruct the original bytes")
	}
	for _, want := range []string{canonical, sibling, "line-wrapped paging copy at", "byte-for-byte in a single line"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice must name %q: %q", want, tail(got, 400))
		}
	}
	// The paging instruction must name the SIBLING, not the canonical file:
	// offset/limit against a one-line file returns the whole blob, which is the
	// dead end the sibling exists to close.
	idx := strings.Index(got, "To read portions")
	if idx < 0 {
		t.Fatalf("notice must carry the paging instruction: %q", tail(got, 400))
	}
	paging := got[idx:]
	if !strings.Contains(paging, sibling) {
		t.Errorf("the paging instruction must name the pager sibling: %q", tail(got, 400))
	}
	if strings.Contains(paging, "against that file") {
		t.Errorf("the paging instruction still points offset/limit at the one-line canonical file: %q", tail(got, 400))
	}
}

// TestTruncateToolResult_spillIsByteIdentical is the acceptance test for
// yaah-vw1v: whatever the byte shape of the output — multi-byte UTF-8, CRLF
// line endings, one enormous JSON line, trailing newlines, or bytes that are
// not valid UTF-8 at all — the spill file reads back byte-for-byte.
func TestTruncateToolResult_spillIsByteIdentical(t *testing.T) {
	cases := map[string]string{
		"multi-byte-utf8":   strings.Repeat("héllo—世界 🀄\n", 4000),
		"crlf-line-endings": strings.Repeat("row value\r\n", 4000),
		"single-line-json":  `{"result":"` + strings.Repeat("x", 200_000) + `"}`,
		"trailing-newlines": strings.Repeat("line\n", 3000) + "\n\n\n",
		"invalid-utf8":      strings.Repeat("\x00\x80\xff\xfe", 20000),
	}
	for name, original := range cases {
		t.Run(name, func(t *testing.T) {
			loop := testLoopWithSpill(t)
			_ = loop.truncateToolResult(original)

			canonical, _ := spillFiles(t, loop.Config.ToolSpillDir)
			got, err := os.ReadFile(canonical)
			if err != nil {
				t.Fatalf("read spill: %v", err)
			}
			if !bytes.Equal(got, []byte(original)) {
				t.Fatalf("spill is not byte-identical: got %d bytes, want %d", len(got), len(original))
			}
		})
	}
}

// TestTruncateToolResult_spilledEnvelopeParsesAsJSON: the reason the canonical
// spill has to be raw. A consumer extracts a field from a spooled delegate
// envelope by parsing the FILE, not by reassembling it first.
func TestTruncateToolResult_spilledEnvelopeParsesAsJSON(t *testing.T) {
	report := strings.Repeat("hard earned context ", 10_000)
	original := `{"role":"golang-developer","result":` + strconv.Quote(report) + `,"stop_reason":"finished"}`
	if strings.Contains(original, "\n") {
		t.Fatal("fixture must be a single line")
	}

	loop := testLoopWithSpill(t)
	_ = loop.truncateToolResult(original)

	canonical, _ := spillFiles(t, loop.Config.ToolSpillDir)
	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read spill: %v", err)
	}
	var envelope struct {
		Role       string `json:"role"`
		Result     string `json:"result"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("spill does not parse as JSON: %v", err)
	}
	if envelope.Result != report || envelope.Role != "golang-developer" {
		t.Errorf("envelope round-trip mismatch: role=%q result bytes=%d", envelope.Role, len(envelope.Result))
	}
}

// TestTruncateToolResult_noticeNamesRawReadPath pins the shipped notice text:
// the path, the fact that the file is raw, and the read tool's offset/limit as
// the paging mechanism. The old "line-wrapped for paging — join lines" wording
// is gone for good.
func TestTruncateToolResult_noticeDeclaresRawBytes(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < defaultTruncateMaxLines+100; i++ {
		sb.WriteString("line\n")
	}
	loop := testLoopWithSpill(t)
	got := loop.truncateToolResult(sb.String())

	for _, want := range []string{
		"[output truncated at",
		"full output at ",
		". To read portions, use the read tool with offset/limit",
		"raw output byte-for-byte",
		"no line wrapping",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q: %q", want, tail(got, 400))
		}
	}
	for _, banned := range []string{"line-wrapped for paging", "join lines to recover"} {
		if strings.Contains(got, banned) {
			t.Errorf("notice still claims wrapping (%q): %q", banned, tail(got, 400))
		}
	}
}

// spillFiles returns the canonical spill and its pager sibling, failing the
// test unless exactly one of each exists. A `.wrapped` sibling only appears for
// a single-line blob; callers that expect none assert on the canonical only.
func spillFiles(t *testing.T, dir string) (canonical, sibling string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	var siblings []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if strings.HasSuffix(p, ".wrapped") {
			siblings = append(siblings, p)
			continue
		}
		if canonical != "" {
			t.Fatalf("expected exactly one canonical spill, also found %s", p)
		}
		canonical = p
	}
	if canonical == "" {
		t.Fatal("no canonical spill file was written")
	}
	if len(siblings) > 1 {
		t.Fatalf("expected at most one pager sibling, got %v", siblings)
	}
	if len(siblings) == 1 {
		sibling = siblings[0]
	}
	return canonical, sibling
}

// tail returns the last n bytes of s, for readable failure messages.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// TestTruncateToolResult_multiLineSpillStaysVerbatim: content with real line
// structure is already pageable and must spill byte-for-byte.
func TestTruncateToolResult_multiLineSpillStaysVerbatim(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		sb.WriteString(strings.Repeat("m", 80) + "\n")
	}
	result := sb.String()
	loop := testLoopWithSpill(t)

	got := loop.truncateToolResult(result)

	if strings.Contains(got, "line-wrapped") {
		t.Error("multi-line content must not be wrapped")
	}
	files, err := os.ReadDir(loop.Config.ToolSpillDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one spill file, got %v (err %v)", files, err)
	}
	spill, err := os.ReadFile(filepath.Join(loop.Config.ToolSpillDir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(spill) != result {
		t.Error("multi-line spill must be verbatim")
	}
}

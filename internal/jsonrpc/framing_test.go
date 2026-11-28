package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// --- Message helpers ------------------------------------------------------

func TestNewRequestMintsNumericID(t *testing.T) {
	msg := NewRequest(12, "tools/call", json.RawMessage(`{"name":"bash"}`))
	if got := string(msg.ID); got != "12" {
		t.Errorf("ID = %s, want 12", got)
	}
	if msg.JSONRPC != "2.0" || msg.Method != "tools/call" {
		t.Errorf("message = %+v", msg)
	}
	if msg.IsNotification() {
		t.Error("a request must not be a notification")
	}
	// The id must be a JSON number, not a string: some peers correlate by type.
	if id, ok := msg.IDInt(); !ok || id != 12 {
		t.Errorf("IDInt = %d, %v; want 12, true", id, ok)
	}
}

func TestNewNotificationHasNoID(t *testing.T) {
	msg := NewNotification("notifications/initialized", nil)
	if !msg.IsNotification() {
		t.Error("notification must report IsNotification")
	}
	if _, ok := msg.IDInt(); ok {
		t.Error("notification id must not decode as numeric")
	}
}

func TestIDIntRejectsStringAndAbsentIDs(t *testing.T) {
	for name, raw := range map[string]string{
		"string id": `"abc"`,
		"absent id": ``,
		"null id":   `null`,
	} {
		msg := Message{ID: json.RawMessage(raw)}
		if _, ok := msg.IDInt(); ok {
			t.Errorf("%s: id must not decode as numeric", name)
		}
	}
}

// --- Content-Length framing (MCP spec default) ---------------------------

func TestFramedReader_parsesMessage(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`
	raw := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	reader := NewFramedReader(strings.NewReader(raw))

	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error: %v", err)
	}
	if id, ok := msg.IDInt(); !ok || id != 1 {
		t.Errorf("ID = %v, want 1", msg.ID)
	}
	if msg.Result == nil {
		t.Fatal("Result is nil")
	}
}

func TestFramedReader_returnsEOFOnEmpty(t *testing.T) {
	reader := NewFramedReader(strings.NewReader(""))

	_, err := reader.ReadMessage()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestFramedWriter_writesFramedJSON(t *testing.T) {
	var buf bytes.Buffer
	writer := NewFramedWriter(&buf)

	msg := NewRequest(1, "initialize", json.RawMessage(`{"protocolVersion":"2024-11-05"}`))
	if err := writer.WriteMessage(msg); err != nil {
		t.Fatalf("WriteMessage() error: %v", err)
	}

	// First line must contain Content-Length
	line, err := bufio.NewReader(&buf).ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if !strings.HasPrefix(line, "Content-Length:") {
		t.Errorf("first line = %q, want Content-Length prefix", line)
	}
}

// --- Newline-delimited framing (Docker MCP gateway compatibility) --------

func TestNewlineWriter_writesRawJSON(t *testing.T) {
	var buf bytes.Buffer
	writer := NewNewlineWriter(&buf)

	msg := NewRequest(1, "initialize", json.RawMessage(`{"protocolVersion":"2024-11-05"}`))
	if err := writer.WriteMessage(msg); err != nil {
		t.Fatalf("WriteMessage() error: %v", err)
	}

	// Output must be raw JSON ending in \n, with NO Content-Length header.
	out := buf.String()
	if strings.HasPrefix(out, "Content-Length:") {
		t.Errorf("newline writer emitted Content-Length header: %q", out)
	}
	if !strings.HasPrefix(out, "{") {
		t.Errorf("newline writer did not start with '{': %q", out)
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Errorf("newline writer did not end with '}\\n': %q", out)
	}
}

func TestNewlineReader_parsesRawJSON(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`
	reader := NewNewlineReader(strings.NewReader(body + "\n"))

	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage() error: %v", err)
	}
	if id, ok := msg.IDInt(); !ok || id != 7 {
		t.Errorf("ID = %v, want 7", msg.ID)
	}
}

func TestNewlineWriter_concurrentSafe(t *testing.T) {
	var buf bytes.Buffer
	writer := NewNewlineWriter(&buf)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = writer.WriteMessage(NewRequest(int64(i), "ping", nil))
		}(i)
	}
	wg.Wait()

	// Count newlines — must equal number of messages
	lines := bytes.Count(buf.Bytes(), []byte("\n"))
	if lines != 20 {
		t.Errorf("got %d newlines, want 20", lines)
	}
}

// --- Auto-detection -------------------------------------------------------

func TestAutoDetectReader_picksNewline(t *testing.T) {
	raw := "  \n{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"a\":1}}\n"
	reader := NewAutoDetectReader(strings.NewReader(raw))
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if id, ok := msg.IDInt(); !ok || id != 2 {
		t.Errorf("ID = %v, want 2", msg.ID)
	}
}

// TestAutoDetectReader_leadingWhitespace pins the review-B13 fix: a fixed-size
// peek returned EOF on streams that began with more whitespace than the peek
// window, so detection consumes whitespace byte-by-byte instead.
func TestAutoDetectReader_leadingWhitespace(t *testing.T) {
	raw := strings.Repeat(" \n", 32) + "{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}\n"
	reader := NewAutoDetectReader(strings.NewReader(raw))
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if id, ok := msg.IDInt(); !ok || id != 3 {
		t.Errorf("ID = %v, want 3", msg.ID)
	}
}

func TestAutoDetectReader_picksFramed(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"a":1}}`
	raw := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	reader := NewAutoDetectReader(strings.NewReader(raw))
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if id, ok := msg.IDInt(); !ok || id != 1 {
		t.Errorf("ID = %v, want 1", msg.ID)
	}
}

// --- Batch responses ------------------------------------------------------

func TestBatchUnsupportedResponses(t *testing.T) {
	const message = "batches unsupported"
	if got := BatchUnsupportedResponses([]byte(`[{"jsonrpc":"2.0","method":"notifications/initialized"}]`), message); len(got) != 0 {
		t.Errorf("notification-only batch responses = %+v, want none", got)
	}
	got := BatchUnsupportedResponses([]byte(`[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`), message)
	if len(got) != 1 || string(got[0].ID) != "1" || got[0].Error == nil || got[0].Error.Message != message {
		t.Errorf("request batch responses = %+v", got)
	}
	got = BatchUnsupportedResponses([]byte(`not json`), message)
	if len(got) != 1 || string(got[0].ID) != "null" || got[0].Error == nil {
		t.Errorf("malformed batch responses = %+v", got)
	}
	if got := BatchUnsupportedResponses([]byte(`[]`), message); len(got) != 1 || string(got[0].ID) != "null" {
		t.Errorf("empty batch responses = %+v", got)
	}
}

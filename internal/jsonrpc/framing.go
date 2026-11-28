package jsonrpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// FramedReader reads Content-Length-framed JSON-RPC messages from an
// io.Reader — the LSP-style framing the MCP specification recommends for
// stdio.
type FramedReader struct {
	reader *bufio.Reader
}

// NewFramedReader creates a new FramedReader.
func NewFramedReader(r io.Reader) *FramedReader {
	return &FramedReader{reader: bufio.NewReader(r)}
}

// ReadMessage reads the next framed message. Returns io.EOF when the
// stream is closed.
func (r *FramedReader) ReadMessage() (Message, error) {
	var msg Message

	// Read headers until empty line
	contentLength := -1
	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			return msg, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			contentLength, _ = strconv.Atoi(val)
		}
	}

	if contentLength <= 0 {
		return msg, fmt.Errorf("invalid Content-Length: %d", contentLength)
	}

	body := make([]byte, contentLength)
	_, err := io.ReadFull(r.reader, body)
	if err != nil {
		return msg, err
	}

	if err := json.Unmarshal(body, &msg); err != nil {
		return msg, fmt.Errorf("unmarshal JSON-RPC: %w", err)
	}

	return msg, nil
}

// FramedWriter writes Content-Length-framed JSON-RPC messages to an
// io.Writer.
type FramedWriter struct {
	writer io.Writer
	mu     sync.Mutex
}

// NewFramedWriter creates a new FramedWriter.
func NewFramedWriter(w io.Writer) *FramedWriter {
	return &FramedWriter{writer: w}
}

// WriteMessage marshals and writes a framed message.
func (w *FramedWriter) WriteMessage(msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := w.writer.Write([]byte(header)); err != nil {
		return fmt.Errorf("write Content-Length header: %w", err)
	}
	if _, err := w.writer.Write(body); err != nil {
		return fmt.Errorf("write framed body: %w", err)
	}
	return nil
}

// --- Newline-delimited framing -------------------------------------------
//
// Some JSON-RPC-over-stdio peers reject the Content-Length header and
// expect raw newline-delimited JSON instead (notably the Docker MCP
// gateway as of v0.42.x). To stay compatible with both, a parallel
// NewlineReader/Writer exists, and AutoDetectReader picks between the
// two on the first message.

// NewlineReader reads raw newline-delimited JSON-RPC messages from an
// io.Reader.
type NewlineReader struct {
	reader *bufio.Reader
}

// NewNewlineReader creates a new NewlineReader.
func NewNewlineReader(r io.Reader) *NewlineReader {
	return &NewlineReader{reader: bufio.NewReader(r)}
}

// ReadMessage reads one newline-terminated JSON object and unmarshals it.
// Returns io.EOF when the stream is closed.
func (r *NewlineReader) ReadMessage() (Message, error) {
	var msg Message
	for {
		line, err := r.reader.ReadBytes('\n')
		if err != nil {
			return msg, err
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		if err := json.Unmarshal([]byte(trimmed), &msg); err != nil {
			return msg, fmt.Errorf("unmarshal JSON-RPC: %w", err)
		}
		return msg, nil
	}
}

// NewlineWriter writes raw newline-delimited JSON-RPC messages to an
// io.Writer.
type NewlineWriter struct {
	writer io.Writer
	mu     sync.Mutex
}

// NewNewlineWriter creates a new NewlineWriter.
func NewNewlineWriter(w io.Writer) *NewlineWriter {
	return &NewlineWriter{writer: w}
}

// WriteMessage marshals a message and writes it followed by a newline.
func (w *NewlineWriter) WriteMessage(msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.writer.Write(body); err != nil {
		return fmt.Errorf("write newline-delimited body: %w", err)
	}
	if _, err := w.writer.Write([]byte("\n")); err != nil {
		return fmt.Errorf("write trailing newline: %w", err)
	}
	return nil
}

// The framing modes AutoDetectReader can settle on.
const (
	FramingContentLength = "framed"
	FramingNewline       = "newline"
)

// AutoDetectReader picks between the two stdio framings on the first
// message and delegates to the chosen reader for every later one.
// Spec-compliant servers send "{json}\n" (newline mode); LSP-style servers
// send "Content-Length: N\r\n\r\n{json}" (framed mode). The first
// non-whitespace byte decides: '{' means newline, anything else framed.
type AutoDetectReader struct {
	reader *bufio.Reader
	inner  interface {
		ReadMessage() (Message, error)
	}
	mode string
}

// NewAutoDetectReader creates a reader that auto-detects framing on the
// first message.
func NewAutoDetectReader(r io.Reader) *AutoDetectReader {
	return &AutoDetectReader{reader: bufio.NewReader(r)}
}

// Framing reports the framing detected so far: empty before the first
// ReadMessage settles detection, FramingNewline or FramingContentLength
// after. It exists for the peer that must mirror the detected mode in its
// own writer — the MCP stdio server, which answers framed clients with
// framed responses and newline clients with newline responses. The mode is
// written during ReadMessage, so a reader on another goroutine may only
// consult Framing after that ReadMessage's result has been handed over
// through a channel or other synchronization edge.
func (a *AutoDetectReader) Framing() string { return a.mode }

// ReadMessage reads the next message, detecting the framing on the first
// call. Leading whitespace is consumed until the first meaningful byte —
// a fixed-size peek returned EOF on streams that began with more
// whitespace than the peek window.
func (a *AutoDetectReader) ReadMessage() (Message, error) {
	if a.inner != nil {
		return a.inner.ReadMessage()
	}
	for {
		b, err := a.reader.ReadByte()
		if err != nil {
			return Message{}, err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			if err := a.reader.UnreadByte(); err != nil {
				return Message{}, err
			}
			a.inner = NewNewlineReader(a.reader)
			a.mode = FramingNewline
		default:
			if err := a.reader.UnreadByte(); err != nil {
				return Message{}, err
			}
			a.inner = NewFramedReader(a.reader)
			a.mode = FramingContentLength
		}
		return a.inner.ReadMessage()
	}
}

// BatchUnsupportedResponses builds one invalid-request error per request in
// a JSON-RPC batch array, for servers that do not accept batches.
// Notification entries get no response; an empty or malformed batch gets
// one id:null error. The message is the caller's — it is wire-visible, so
// each server states its own reason.
func BatchUnsupportedResponses(raw []byte, message string) []Message {
	invalid := func(id json.RawMessage) Message {
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		return Message{
			JSONRPC: Version,
			ID:      id,
			Error:   &Error{Code: CodeInvalidRequest, Message: message},
		}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return []Message{invalid(nil)}
	}
	if len(items) == 0 {
		return []Message{invalid(nil)}
	}
	out := make([]Message, 0, len(items))
	for _, item := range items {
		var entry struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(item, &entry); err != nil {
			out = append(out, invalid(nil))
			continue
		}
		if len(entry.ID) == 0 {
			continue
		}
		out = append(out, invalid(entry.ID))
	}
	return out
}

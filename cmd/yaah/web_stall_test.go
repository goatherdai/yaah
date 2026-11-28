package yaah

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The /api/stream write path must be bounded: a client that stops reading
// fills the kernel socket buffers, and without a write deadline the blocked
// Fprintf parks the BrokerView forwarder — wedging Run teardown until the
// TCP stack gives up (yaah-zdzm). These tests pin the stall semantics: a
// failed write flips the view, closes the handler's release channel, and
// every later write is a no-op instead of burning the timeout per event.

// failingWriter fails every Write like a broken pipe.
type failingWriter struct{}

var errBrokenPipe = errors.New("write: broken pipe")

func (failingWriter) Header() http.Header       { return http.Header{} }
func (failingWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }
func (failingWriter) WriteHeader(int)           {}

func TestSseViewWriteErrorStallsView(t *testing.T) {
	stallCh := make(chan struct{})
	v := &sseView{w: failingWriter{}, stallCh: stallCh}

	v.write([]byte(`{"type":"ctrl.state"}`))

	select {
	case <-stallCh:
	default:
		t.Fatal("a failed write must close the stall channel")
	}
	if !v.stalled.Load() {
		t.Fatal("a failed write must mark the view stalled")
	}

	// Later writes are no-ops: they must not retry the dead connection and
	// must not double-close the stall channel.
	v.write([]byte(`{"type":"token"}`))
	v.write([]byte(`{"type":"done"}`))
	select {
	case <-stallCh:
	default:
		t.Error("stall channel should stay closed, not re-fired")
	}
}

// TestSseViewBareLiteralStillWrites pins the nil-safety contract: the many
// bare struct literals in existing tests construct a view with no response
// controller and no stall channel, and writes through such a view keep
// behaving exactly as before this change.
func TestSseViewBareLiteralStillWrites(t *testing.T) {
	buf := newSyncBuffer()
	v := &sseView{w: buf}

	v.write([]byte(`{"type":"token"}`))

	if !strings.Contains(buf.String(), `data: {"type":"token"}`) {
		t.Errorf("bare literal view should still write SSE data, got: %q", buf.String())
	}
}

// TestSseViewStalledWriteIsBounded drives the real path: a live TCP
// connection whose client never reads. The kernel buffers fill, the per-write
// deadline (shortened for the test) expires, and the write returns instead of
// parking the caller forever.
func TestSseViewStalledWriteIsBounded(t *testing.T) {
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		v := &sseView{
			w:            w,
			rc:           http.NewResponseController(w),
			stallCh:      make(chan struct{}),
			writeTimeout: 100 * time.Millisecond,
		}
		payload := make([]byte, 64*1024)
		for i := 0; i < 4096 && !v.stalled.Load(); i++ {
			v.write(payload)
		}
	}))
	defer srv.Close()

	// A client that sends one HTTP request and then never reads: its unread
	// socket is what fills the server-side buffers.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	select {
	case <-handlerDone:
		// The handler returned on its own: the deadline bounded the write,
		// stall() fired, and teardown is free to run — the whole point.
	case <-time.After(30 * time.Second):
		t.Fatal("stalled write never returned; Run teardown would hang")
	}
}

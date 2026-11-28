package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/pubsub"
)

// logBrokerDrops is the retiring broker generation's only production reader
// of the per-subscription drop counters: one Warn line per subscription that
// lost events, named so a wedged view is identifiable after the fact. These
// tests pin the shape of that report — what is reported, what is silent, and
// how anonymous subscriptions (BrokerView's forwarder) are named.

// captureLogs swaps the default slog logger for one writing to buf and
// restores the previous one on cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestLogBrokerDrops(t *testing.T) {
	tests := []struct {
		name   string
		rows   []pubsub.SubscriberStats
		want   []string
		noWant []string
	}{
		{
			name: "empty stats reports nothing",
			rows: nil,
		},
		{
			name: "fully delivered subscription reports nothing",
			rows: []pubsub.SubscriberStats{{ID: "tui", Delivered: 10}},
		},
		{
			name: "lossy-only drops stay below the warn level",
			rows: []pubsub.SubscriberStats{{ID: "web", Delivered: 3, Dropped: 4096}},
		},
		{
			name: "named subscription with must-drops is reported with counts",
			rows: []pubsub.SubscriberStats{
				{ID: "tui", Delivered: 10, Dropped: 0},
				{ID: "web", Delivered: 3, Dropped: 5, MustDropped: 1},
			},
			want:   []string{`subscriber=web`, `delivered=3`, `dropped=5`, `must_delivered_dropped=1`},
			noWant: []string{`subscriber=tui`},
		},
		{
			name: "anonymous subscription is reported as view",
			rows: []pubsub.SubscriberStats{{Delivered: 1, Dropped: 4097, MustDropped: 1}},
			want: []string{`subscriber=view`, `must_delivered_dropped=1`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLogs(t)
			logBrokerDrops(tt.rows)
			out := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %q, got: %s", want, out)
				}
			}
			for _, noWant := range tt.noWant {
				if strings.Contains(out, noWant) {
					t.Errorf("log should not mention %q, got: %s", noWant, out)
				}
			}
			// The silent cases must actually be silent: a report emitted
			// for a fully delivered subscriber would be noise on every run.
			if len(tt.want) == 0 && out != "" {
				t.Errorf("expected no log output, got: %s", out)
			}
		})
	}
}

// TestPublishDoneReportsDrops wires the report through publishDone: a broker
// generation whose subscriber was too slow to take every event must surface
// in the log at the end of the Run, after the final DoneEvent is fanned out.
func TestPublishDoneReportsDrops(t *testing.T) {
	buf := captureLogs(t)

	// A broker with a wedged subscriber: a channel nobody drains fills
	// after one event, so the lossy fills count plain drops, and the
	// must-deliver DoneEvent publishDone sends counts a must-drop — the
	// wedged-view signal the report exists to surface.
	broker := pubsub.NewBroker[Event]()
	t.Cleanup(broker.Close)
	sub := broker.Subscribe("", 1)
	broker.Publish(&TokenDeltaEvent{Text: "filler"})
	for i := 0; i < 5; i++ {
		broker.Publish(&TokenDeltaEvent{Text: "x"})
	}

	l := &Loop{}
	l.broker = broker
	l.brokerView = NewBrokerView(broker, NoopView{})
	t.Cleanup(func() {
		if !l.brokerClosed {
			l.brokerView.Close()
		}
	})

	response := "done"
	var runErr error
	l.publishDone(&response, &runErr)

	out := buf.String()
	if !strings.Contains(out, "dropped must-deliver events during run") {
		t.Errorf("publishDone should report the wedged subscriber, got log: %s", out)
	}
	if !strings.Contains(out, `subscriber=view`) {
		t.Errorf("anonymous brokerView subscription should be reported as view, got log: %s", out)
	}
	// The wedged subscriber's single slot still holds the filler
	// TokenDeltaEvent — the DoneEvent itself was counted dropped when the
	// shared 50ms must-deliver budget expired on the full channel. Reading
	// it only confirms the filler stayed queued for this subscriber;
	// the forwarder's exit is separately guaranteed by brokerView.Close,
	// which waits on its done channel.
	select {
	case evt := <-sub:
		if _, ok := evt.(*TokenDeltaEvent); !ok {
			t.Errorf("expected the filler TokenDeltaEvent, got %T", evt)
		}
	default:
		t.Error("wedge setup failed: the filler event is not in the subscription")
	}
}

package mcpinfo

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/rivo/tview"
)

func TestFormat_Empty(t *testing.T) {
	th := colors.NewDarkTheme()
	out := Format(nil, &th)
	if !strings.Contains(out, "no servers") {
		t.Errorf("empty should show 'no servers', got %q", out)
	}
}

func TestFormat_OneConnected(t *testing.T) {
	th := colors.NewDarkTheme()
	servers := []Server{{Name: "filesystem", Connected: true}}
	out := Format(servers, &th)
	if !strings.Contains(out, "filesystem") {
		t.Errorf("should contain server name, got %q", out)
	}
	if !strings.Contains(out, "●") {
		t.Errorf("should contain dot indicator, got %q", out)
	}
}

func TestFormat_OneDisconnected(t *testing.T) {
	th := colors.NewDarkTheme()
	servers := []Server{{Name: "database", Connected: false}}
	out := Format(servers, &th)
	if !strings.Contains(out, "database") {
		t.Errorf("should contain server name, got %q", out)
	}
}

func TestFormat_Multiple(t *testing.T) {
	th := colors.NewDarkTheme()
	servers := []Server{
		{Name: "filesystem", Connected: true},
		{Name: "database", Connected: false},
		{Name: "api", Connected: true},
	}
	out := Format(servers, &th)
	if !strings.Contains(out, "filesystem") {
		t.Error("should contain first server")
	}
	if !strings.Contains(out, "database") {
		t.Error("should contain second server")
	}
	if !strings.Contains(out, "api") {
		t.Error("should contain third server")
	}
}

func TestFormat_ConnectedHasConnectedColor(t *testing.T) {
	th := colors.NewDarkTheme()
	servers := []Server{{Name: "filesystem", Connected: true}}
	out := Format(servers, &th)
	colorTag := "[" + th.Connected + "]"
	if !strings.Contains(out, colorTag) {
		t.Errorf("connected server should use Connected color %q, got %q", colorTag, out)
	}
}

func TestFormat_DisconnectedHasDimColor(t *testing.T) {
	th := colors.NewDarkTheme()
	servers := []Server{{Name: "database", Connected: false}}
	out := Format(servers, &th)
	colorTag := "[" + th.Dim + "]"
	if !strings.Contains(out, colorTag) {
		t.Errorf("disconnected server should use Dim color %q, got %q", colorTag, out)
	}
}

// TestShow_EscapeAndEnterDismiss drives the real tview event loop with a
// simulation screen and asserts the advertised dismissal keys actually
// dismiss the modal (and remove its page).
func TestShow_EscapeAndEnterDismiss(t *testing.T) {
	for _, key := range []tcell.Key{tcell.KeyEscape, tcell.KeyEnter} {
		t.Run(tcell.KeyNames[key], func(t *testing.T) {
			sim := tcell.NewSimulationScreen("UTF-8")
			if err := sim.Init(); err != nil {
				t.Fatalf("init simulation screen: %v", err)
			}
			t.Cleanup(sim.Fini)
			sim.SetSize(80, 24)

			app := tview.NewApplication()
			app.SetScreen(sim)
			pages := tview.NewPages()
			app.SetRoot(pages, true)
			pages.AddPage("root", tview.NewBox(), true, true)
			th := colors.NewDarkTheme()

			// The dismissal callback runs on the app event-loop
			// goroutine, so the signal must be race-free: a channel
			// close, not a shared bool.
			dismissed := make(chan struct{})
			Show(app, pages, []Server{{Name: "docs", Connected: true}}, &th, func() { close(dismissed) })
			if !pages.HasPage(modalPageName) {
				t.Fatal("modal page was not added")
			}

			done := make(chan struct{})
			go func() { app.Run(); close(done) }()
			t.Cleanup(func() {
				app.QueueUpdate(app.Stop)
				<-done
			})

			time.Sleep(100 * time.Millisecond) // let the event loop take focus
			sim.InjectKey(key, 0, tcell.ModNone)

			select {
			case <-dismissed:
			case <-time.After(5 * time.Second):
				t.Fatalf("key %v did not dismiss the MCP modal", key)
			}
			if pages.HasPage(modalPageName) {
				t.Fatal("dismissal left the modal page in place")
			}
		})
	}
}

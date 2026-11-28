package memory

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrate_AddsToolOutcomeColumns verifies that a messages table created
// before tool outcomes were persisted gains the outcome / duration_ms columns
// on Open, repeatedly, and that a pre-existing row reads back as status
// unknown with no measurement (yaah-77f1).
func TestMigrate_AddsToolOutcomeColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// A database with the pre-outcome messages schema: every other column
	// present, nothing for outcome or duration.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacySchema := `
	CREATE TABLE sessions (
		id         TEXT PRIMARY KEY,
		started_at INTEGER NOT NULL
	);
	CREATE TABLE messages (
		session_id        TEXT NOT NULL REFERENCES sessions(id),
		idx               INTEGER NOT NULL,
		role              TEXT NOT NULL,
		content           TEXT NOT NULL,
		reasoning_content TEXT DEFAULT '',
		reasoning_signature TEXT DEFAULT '',
		tool_name         TEXT,
		tool_call_id      TEXT,
		tool_calls        TEXT,
		ts                INTEGER NOT NULL,
		PRIMARY KEY (session_id, idx)
	);
	INSERT INTO sessions (id, started_at) VALUES ('s1', 1);
	INSERT INTO messages (session_id, idx, role, content, tool_name, tool_call_id, ts)
		VALUES ('s1', 0, 'tool', 'error: exit status 1', 'bash', 'call-legacy', 1);
	`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// First open migrates; a second open must be a no-op, not a duplicate
	// ALTER (which SQLite rejects with "duplicate column name").
	for attempt := 1; attempt <= 2; attempt++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", attempt, err)
		}
		for _, col := range []string{"outcome", "duration_ms"} {
			var n int
			if err := db.sql.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = ?", col).Scan(&n); err != nil {
				db.Close()
				t.Fatalf("pragma %s: %v", col, err)
			}
			if n != 1 {
				db.Close()
				t.Errorf("messages.%s column missing after open #%d", col, attempt)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close #%d: %v", attempt, err)
		}
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer db.Close()

	// The legacy row survived and reads as unknown / unmeasured: the
	// zero-value defaults are what keep pre-outcome sessions replayable
	// under the legacy text rule.
	msgs, err := db.GetMessages("s1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("legacy rows = %d, want 1", len(msgs))
	}
	if msgs[0].Outcome != "" {
		t.Errorf("legacy row outcome = %q, want empty (unknown)", msgs[0].Outcome)
	}
	if msgs[0].DurationMs != 0 {
		t.Errorf("legacy row duration = %d, want 0 (unmeasured)", msgs[0].DurationMs)
	}
	if msgs[0].Content != "error: exit status 1" {
		t.Errorf("legacy row content = %q, want it untouched", msgs[0].Content)
	}
}

// TestMessageRoundTripsToolOutcome pins the store-level round trip the
// persister depends on: outcome and duration survive a write and a read.
func TestMessageRoundTripsToolOutcome(t *testing.T) {
	db := openTestDB(t)
	if err := db.AddMessage(Message{SessionID: "ses-test", Idx: 0, Role: "user", Content: "go", Timestamp: 1}); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	if err := db.AddMessage(Message{
		SessionID:  "ses-test",
		Idx:        1,
		Role:       "tool",
		Content:    "error: boom",
		ToolName:   "bash",
		ToolCallID: "call-1",
		Timestamp:  2,
		Outcome:    "error",
		DurationMs: 1500,
	}); err != nil {
		t.Fatalf("AddMessage(tool): %v", err)
	}

	msgs, err := db.GetMessages("ses-test")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("rows = %d, want 2", len(msgs))
	}
	if got := msgs[1]; got.Outcome != "error" || got.DurationMs != 1500 {
		t.Errorf("tool row = outcome %q duration %d, want error / 1500", got.Outcome, got.DurationMs)
	}
}

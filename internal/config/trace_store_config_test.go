package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tracestore"
)

func writeTraceConfig(t *testing.T, yaml string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("YAAH_HOME", tmp)
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
}

// The shepherd_trace stanza resolves traces' backend and DSN; the
// deprecated flat shepherd_trace_path remains the SQLite fallback. Both
// must keep working: existing configurations never change meaning.
func TestLoad_shepherdTraceStanza(t *testing.T) {
	writeTraceConfig(t, `
agents:
  default:
    shepherd_trace:
      backend: postgres
      dsn: postgres://u:p@db.internal:5432/traces
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := cfg.Agent.Default.TraceStoreConfig()
	if got.Backend != tracestore.BackendPostgres {
		t.Errorf("backend = %q, want postgres", got.Backend)
	}
	if got.DSN != "postgres://u:p@db.internal:5432/traces" {
		t.Errorf("dsn = %q", got.DSN)
	}
}

func TestLoad_shepherdTraceStanzaDSNTildeExpanded(t *testing.T) {
	writeTraceConfig(t, `
agents:
  default:
    shepherd_trace:
      dsn: ~/traces/trace.db
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := cfg.Agent.Default.TraceStoreConfig()
	if got.Backend != tracestore.BackendSQLite {
		t.Errorf("backend = %q, want the sqlite default", got.Backend)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, "traces", "trace.db"); got.DSN != want {
		t.Errorf("dsn = %q, want %q (tilde expanded)", got.DSN, want)
	}
}

func TestLoad_shepherdTraceStanzaOverridesLegacyPath(t *testing.T) {
	// The stanza's DSN wins over the legacy flat key: new configuration
	// must not be silently shadowed by a leftover shepherd_trace_path.
	writeTraceConfig(t, `
agents:
  default:
    shepherd_trace_path: /tmp/legacy/trace.db
    shepherd_trace:
      dsn: /tmp/new/trace.db
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := cfg.Agent.Default.TraceStoreConfig()
	if got.DSN != "/tmp/new/trace.db" {
		t.Errorf("dsn = %q, want the stanza's DSN to win", got.DSN)
	}
	if got.Backend != tracestore.BackendSQLite {
		t.Errorf("backend = %q, want the sqlite default", got.Backend)
	}
}

func TestTraceStoreConfig_defaultsToSQLiteTracePath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("YAAH_HOME", tmp)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Nothing configured: the sqlite backend against the default yaah-home
	// trace file — the pre-stanza behavior, byte for byte.
	got := cfg.Agent.Default.TraceStoreConfig()
	if got.Backend != tracestore.BackendSQLite {
		t.Errorf("backend = %q, want sqlite", got.Backend)
	}
	if want := filepath.Join(tmp, "trace.db"); got.DSN != want {
		t.Errorf("dsn = %q, want %q", got.DSN, want)
	}
	if !got.IsSQLite() {
		t.Errorf("IsSQLite() = false for the default config")
	}
}

func TestTraceStoreConfig_legacyPathStillResolves(t *testing.T) {
	// The deprecated flat key, no stanza: existing configurations keep
	// working — the path is the sqlite DSN.
	d := Defaults{ShepherdTracePath: "/tmp/legacy/trace.db"}
	got := d.TraceStoreConfig()
	if got.Backend != tracestore.BackendSQLite || got.DSN != "/tmp/legacy/trace.db" {
		t.Errorf("legacy path resolution = %+v, want sqlite /tmp/legacy/trace.db", got)
	}
	if strings.Contains(got.Describe(), "secret") {
		t.Errorf("Describe() leaked credentials")
	}
}

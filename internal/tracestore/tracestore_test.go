package tracestore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
)

func TestNewConfigValidation(t *testing.T) {
	t.Run("empty backend resolves to sqlite", func(t *testing.T) {
		cfg, err := NewConfig("", "trace.db")
		if err != nil {
			t.Fatalf("NewConfig: %v", err)
		}
		if cfg.Backend != BackendSQLite {
			t.Errorf("backend = %q, want sqlite", cfg.Backend)
		}
		if !cfg.IsSQLite() {
			t.Errorf("IsSQLite() = false for the default backend")
		}
	})

	t.Run("unknown backend fails naming the supported set", func(t *testing.T) {
		_, err := NewConfig("boltdb", "x")
		if err == nil {
			t.Fatalf("NewConfig(boltdb) succeeded, want error")
		}
		for _, want := range []string{"boltdb", BackendSQLite, BackendPostgres} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err.Error(), want)
			}
		}
	})

	t.Run("postgres without a DSN fails", func(t *testing.T) {
		if _, err := NewConfig(BackendPostgres, ""); err == nil {
			t.Fatalf("NewConfig(postgres, no DSN) succeeded, want error")
		}
	})
}

func TestDescribeHidesCredentials(t *testing.T) {
	cfg := Config{Backend: BackendPostgres, DSN: "postgres://user:secret@db.internal:5432/traces?sslmode=disable"}
	got := cfg.Describe()
	if strings.Contains(got, "secret") || strings.Contains(got, "user:") {
		t.Errorf("Describe() leaked credentials: %q", got)
	}
	if !strings.Contains(got, "db.internal:5432") {
		t.Errorf("Describe() dropped the host: %q", got)
	}
	// A SQLite path is not a URL; it passes through under the backend tag.
	sqlite := Config{Backend: BackendSQLite, DSN: "/home/u/.yaah/trace.db"}
	if got := sqlite.Describe(); !strings.Contains(got, "/home/u/.yaah/trace.db") {
		t.Errorf("Describe() dropped the sqlite path: %q", got)
	}
}

func TestDescribeRedactsCredentialParams(t *testing.T) {
	t.Run("URL query parameters", func(t *testing.T) {
		cfg := Config{Backend: BackendPostgres, DSN: "postgres://app@db.internal:5432/traces?sslmode=disable&password=hunter2&sslpassword=sp2&connect_timeout=5#frag"}
		got := cfg.Describe()
		for _, secret := range []string{"hunter2", "sp2"} {
			if strings.Contains(got, secret) {
				t.Errorf("Describe() leaked %q in %q", secret, got)
			}
		}
		for _, want := range []string{"db.internal:5432", "sslmode=disable", "connect_timeout=5", "password=%3Credacted%3E"} {
			if !strings.Contains(got, want) {
				t.Errorf("Describe() = %q, missing %q", got, want)
			}
		}
		if strings.Contains(got, "frag") {
			t.Errorf("Describe() leaked the URL fragment: %q", got)
		}
	})

	t.Run("keyword/value DSN", func(t *testing.T) {
		cfg := Config{Backend: BackendPostgres, DSN: "host=db.internal user=app password=hunter2 sslmode=disable sslpassword=sp2"}
		got := cfg.Describe()
		for _, secret := range []string{"hunter2", "sp2"} {
			if strings.Contains(got, secret) {
				t.Errorf("Describe() leaked %q in %q", secret, got)
			}
		}
		for _, want := range []string{"host=db.internal", "user=app", "sslmode=disable", "password=<redacted>", "sslpassword=<redacted>"} {
			if !strings.Contains(got, want) {
				t.Errorf("Describe() = %q, missing %q", got, want)
			}
		}
	})

	t.Run("keyword/value DSN with quoted values", func(t *testing.T) {
		cfg := Config{Backend: BackendPostgres, DSN: `host=db.internal user=app password='hunter 2'`}
		got := cfg.Describe()
		if strings.Contains(got, "hunter 2") {
			t.Errorf("Describe() leaked the quoted password: %q", got)
		}
	})
}

func TestOpenSQLiteRoundTrip(t *testing.T) {
	cfg := Config{DSN: filepath.Join(t.TempDir(), "trace.sqlite")}
	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close(context.Background())

	receipt, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: "intent:tracestore",
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID: "exec:tracestore",
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode:      kerneltrace.Capture,
				SchemaRef: "yaah.tracestore.probe.v1",
				Payload:   map[string]any{"probe": true},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipt.FactIDs) != 1 {
		t.Fatalf("Append allocated %d facts, want 1", len(receipt.FactIDs))
	}
}

func TestOpenReadOnlyFailsLoudly(t *testing.T) {
	cfg := Config{DSN: filepath.Join(t.TempDir(), "missing.sqlite")}
	if _, err := OpenReadOnly(cfg); err == nil {
		t.Fatalf("OpenReadOnly on a missing store succeeded, want error")
	}
}

// TestOpenPostgres exercises the PostgreSQL backend when a server is
// available (the same KERNEL_PG_DSN convention the kernel's suite uses).
// Owners are round-unique so the shared spike database tolerates repeats.
func TestOpenPostgres(t *testing.T) {
	dsn := os.Getenv("KERNEL_PG_DSN")
	if dsn == "" {
		t.Skip("KERNEL_PG_DSN not set; PostgreSQL tracestore test skipped")
	}
	cfg := Config{Backend: BackendPostgres, DSN: dsn}
	store, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(postgres): %v", err)
	}
	defer store.Close(context.Background())

	owner := "exec:tracestore-pg"
	if _, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: "intent:tracestore-pg",
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID: owner,
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode:      kerneltrace.Capture,
				SchemaRef: "yaah.tracestore.probe.v1",
				Payload:   map[string]any{"probe": "pg"},
			}},
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	slice, err := store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, 99, kerneltrace.ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	if len(slice.FactIDs()) == 0 {
		t.Fatalf("owner path %s holds no facts after append", owner)
	}
}

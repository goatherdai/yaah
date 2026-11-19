// Package tracestore is yaah's single seam onto the Shepherd trace store.
//
// Every component that opens a trace store — session wiring, the retention
// evictor, doctor, the trace and shepherd CLIs, the usage recorder — goes
// through this package instead of calling the kernel's SQLite constructors
// directly, so the storage backend is chosen in exactly one place: the
// configuration. The kernel resolves backend specifics behind its dialect
// seam; this package resolves yaah's configuration into a kernel
// StoreConfig and hands back a store.
package tracestore

import (
	"fmt"
	"net/url"
	"strings"

	shepherdtrace "github.com/goatherdai/goatherd-kernel"
)

// Backends yaah can configure. The empty value means the SQLite default.
const (
	BackendSQLite   = string(shepherdtrace.BackendSQLite)
	BackendPostgres = string(shepherdtrace.BackendPostgres)
)

// Config names a trace store: which backend, and its data source. The
// zero value is the default SQLite store.
type Config struct {
	// Backend is "sqlite" (default) or "postgres"; anything else fails
	// Open with an error naming the supported set.
	Backend string
	// DSN is the backend data source name: a file path (or ":memory:")
	// for SQLite, a connection URL for PostgreSQL. Empty resolves to the
	// default SQLite path for the SQLite backend (see
	// config.Defaults.TraceStoreConfig) and is an error for PostgreSQL.
	DSN string
}

// NewConfig validates and normalizes a backend/DSN pair.
func NewConfig(backend, dsn string) (Config, error) {
	cfg := Config{Backend: strings.TrimSpace(backend), DSN: strings.TrimSpace(dsn)}
	if cfg.Backend == "" {
		cfg.Backend = BackendSQLite
	}
	switch cfg.Backend {
	case BackendSQLite, BackendPostgres:
	default:
		return Config{}, fmt.Errorf("unsupported trace store backend %q: supported backends: %s, %s", cfg.Backend, BackendSQLite, BackendPostgres)
	}
	if cfg.DSN == "" && cfg.Backend == BackendPostgres {
		return Config{}, fmt.Errorf("trace store backend %q needs a DSN (set agent.default.shepherd_trace.dsn)", BackendPostgres)
	}
	return cfg, nil
}

// IsSQLite reports whether the backend stores traces in a local file.
// The empty backend is the SQLite default, so the zero Config is SQLite.
// Callers gate file-only side effects on it: mkdir/chmod around the store
// file, existence stats (a server database always "exists"), and anything
// else that assumes one file per store.
func (c Config) IsSQLite() bool {
	return c.Backend == "" || c.Backend == BackendSQLite
}

// secretParams are credential-bearing PostgreSQL URL query parameters and
// keyword/value DSN fields: Describe renders their values redacted.
// sslkey names a file, not a secret value, but the path can carry
// identity, so it is treated the same.
var secretParams = map[string]bool{
	"password":    true,
	"sslpassword": true,
	"sslkey":      true,
}

// Describe renders the config for diagnostics without leaking
// credentials. A PostgreSQL URL DSN keeps its scheme, host, and database
// but drops userinfo, credential-bearing query parameters, and the
// fragment. A PostgreSQL keyword/value DSN keeps its fields with
// credential values redacted. SQLite DSNs are file paths with no
// credential surface and pass through under the backend tag.
func (c Config) Describe() string {
	if !strings.Contains(c.DSN, "://") {
		if c.IsSQLite() {
			return fmt.Sprintf("%s://%s", c.Backend, c.DSN)
		}
		// PostgreSQL keyword/value form, e.g.
		// "host=db.internal user=app password=secret". The values of
		// fields in secretParams are the leak surface: render those
		// as <redacted> and keep everything else verbatim.
		if c.DSN == "" {
			return fmt.Sprintf("%s://<unconfigured DSN>", c.Backend)
		}
		fields := strings.Fields(c.DSN)
		rendered := make([]string, 0, len(fields))
		for _, field := range fields {
			key, _, found := strings.Cut(field, "=")
			if found && secretParams[strings.ToLower(key)] {
				rendered = append(rendered, key+"=<redacted>")
				continue
			}
			rendered = append(rendered, field)
		}
		return strings.Join(rendered, " ")
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		return fmt.Sprintf("%s://<unparseable DSN>", c.Backend)
	}
	u.User = nil
	u.Fragment = ""
	q := u.Query()
	for key := range q {
		if secretParams[strings.ToLower(key)] {
			q.Set(key, "<redacted>")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Open opens the trace store for read-write use per cfg. The store is the
// kernel's concrete type: callers that need the effect bus attach it via
// StoreConfig (OpenConfigured) instead.
func Open(cfg Config) (*shepherdtrace.SQLiteTraceStore, error) {
	return OpenConfigured(cfg, shepherdtrace.StoreConfig{})
}

// OpenReadOnly opens an existing trace store without write capability.
// The database must already hold the full schema; a missing or non-trace
// store fails loudly instead of materializing an empty one.
func OpenReadOnly(cfg Config) (*shepherdtrace.SQLiteTraceStore, error) {
	sc := shepherdtrace.StoreConfig{}
	sc.ReadOnly = true
	return OpenConfigured(cfg, sc)
}

// OpenConfigured opens the store with caller-supplied kernel options
// (bus, limits, read-only) merged over the backend config.
func OpenConfigured(cfg Config, sc shepherdtrace.StoreConfig) (*shepherdtrace.SQLiteTraceStore, error) {
	normalized, err := NewConfig(cfg.Backend, cfg.DSN)
	if err != nil {
		return nil, err
	}
	sc.Backend = shepherdtrace.Backend(normalized.Backend)
	sc.DSN = normalized.DSN
	return shepherdtrace.OpenSQLite(sc)
}

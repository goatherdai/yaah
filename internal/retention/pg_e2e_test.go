package retention

import (
	"context"
	"errors"
	"os"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	"github.com/goatherdai/yaah/internal/tracestore"
)

// newPGE2E opens the PostgreSQL trace store named by YAAH_PG_DSN. Skipped
// unless the env var names a server:
//
//	docker run -d --name yaah-pg-e2e \
//	  -e POSTGRES_USER=goatherd -e POSTGRES_PASSWORD=goatherd -e POSTGRES_DB=goatherd \
//	  -p 54329:5432 postgres:16-alpine
//	YAAH_PG_DSN='postgres://goatherd:goatherd@127.0.0.1:54329/goatherd?sslmode=disable' \
//	  go test -run TestPG -count=1 ./internal/retention/
func newPGE2E(t *testing.T) *pgE2E {
	t.Helper()
	dsn := os.Getenv("YAAH_PG_DSN")
	if dsn == "" {
		t.Skip("YAAH_PG_DSN not set; PostgreSQL e2e skipped")
	}
	cfg, err := tracestore.NewConfig(tracestore.BackendPostgres, dsn)
	if err != nil {
		t.Fatalf("tracestore.NewConfig: %v", err)
	}
	store, err := tracestore.Open(cfg)
	if err != nil {
		t.Fatalf("tracestore.Open (postgres): %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return &pgE2E{t: t, cfg: cfg, store: store}
}

// pgE2E is the SQLite fixture's server-backend counterpart: the trace side
// resolves through the same seam the runtime uses, while the state side is
// deliberately out of scope (its own backend move is sequenced separately)
// — EvictSession tolerates a nil db, which loses only the sub-agent owner
// mappings this flow does not depend on.
type pgE2E struct {
	t     *testing.T
	cfg   tracestore.Config
	store *kerneltrace.SQLiteTraceStore
}

func (p *pgE2E) appendFacts(intent, owner, parentSession string) {
	p.t.Helper()
	if _, err := p.store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: intent,
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID: owner,
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode:      kerneltrace.Capture,
				SchemaRef: "yaah.test.turn.v1",
				Payload:   map[string]any{"session": parentSession},
			}},
		}},
	}); err != nil {
		p.t.Fatalf("Append(%s): %v", intent, err)
	}
}

func (p *pgE2E) wantReadable(owner string) {
	p.t.Helper()
	slice, err := p.store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, throughMax, kerneltrace.ModeBoth)
	if err != nil {
		p.t.Fatalf("pre-delete read owner %s: %v", owner, err)
	}
	if got := len(slice.FactIDs()); got == 0 {
		p.t.Fatalf("pre-delete read owner %s: want facts, got none", owner)
	}
}

func wantEvictedOwnerPG(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner string) {
	t.Helper()
	if _, err := store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, throughMax, kerneltrace.ModeBoth); !errors.Is(err, kerneltrace.ErrOwnerEvicted) {
		t.Fatalf("path %s after delete: want ErrOwnerEvicted, got %v", owner, err)
	}
}

// TestPGEvictSweepEndToEnd drives the delete flow through a live PostgreSQL
// store: append real facts, evict kernel-first, sweep garbage under the
// single-flight guard, and confirm the paths are actually gone. This is the
// PG counterpart of the SQLite fixture tests — the same evictor, the same
// backend|DSN sweep keying, the other dialect.
func TestPGEvictSweepEndToEnd(t *testing.T) {
	p := newPGE2E(t)

	const (
		sessionID = "sess-pg-e2e"
		subOwner  = "sub-researcher-" + sessionID + "-1730000000"
	)
	p.appendFacts("intent:pg-e2e-main", sessionID, sessionID)
	p.appendFacts("intent:pg-e2e-sub", subOwner, sessionID)
	p.wantReadable(sessionID)
	p.wantReadable(subOwner)

	evictor := NewEvictor(p.cfg)
	if err := evictor.EvictSession(nil, sessionID); err != nil {
		t.Fatalf("EvictSession: %v", err)
	}
	wantEvictedOwnerPG(t, p.store, sessionID)

	// The nil-db evictor cannot know the sub-agent mapping, so the sub path
	// is evicted directly — the sweep must reclaim both.
	if _, err := p.store.EvictOwnerPath(context.Background(), kerneltrace.TrustedAppendContext, subOwner, true); err != nil {
		t.Fatalf("EvictOwnerPath(sub): %v", err)
	}
	wantEvictedOwnerPG(t, p.store, subOwner)

	// SweepNow opens its own connection through the seam, under the
	// single-flight guard keyed backend|DSN — the PG leg of that keying.
	if err := evictor.SweepNow(); err != nil {
		t.Fatalf("SweepNow: %v", err)
	}
	if err := evictor.SweepNow(); err != nil {
		t.Fatalf("second SweepNow (guarded path): %v", err)
	}
}

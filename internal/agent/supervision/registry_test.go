package supervision

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistry_InjectDeliversToRegisteredRun(t *testing.T) {
	r := NewRegistry()
	ch := make(chan string, SteerBuffer)
	r.Register("scope:sub-1", Run{SteerCh: ch})

	if err := r.Inject("scope:sub-1", "try a different approach"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	select {
	case got := <-ch:
		if got != "try a different approach" {
			t.Errorf("delivered = %q, want the guidance verbatim", got)
		}
	default:
		t.Fatal("Inject reported success but nothing arrived on the channel")
	}
	if !r.Has("scope:sub-1") {
		t.Error("Has must report the registered run")
	}
}

func TestRegistry_InjectWithoutRun(t *testing.T) {
	r := NewRegistry()
	if err := r.Inject("scope:gone", "x"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Inject at an unregistered scope = %v, want ErrNoRun", err)
	}
}

func TestRegistry_UnregisterThenInject(t *testing.T) {
	r := NewRegistry()
	ch := make(chan string, SteerBuffer)
	r.Register("scope:sub-2", Run{SteerCh: ch})
	r.Unregister("scope:sub-2")
	if err := r.Inject("scope:sub-2", "x"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Inject after Unregister = %v, want ErrNoRun", err)
	}
	r.Unregister("scope:sub-2") // idempotent; a deferred unregister races session close
	if r.Has("scope:sub-2") {
		t.Error("Has after double Unregister must be false")
	}
}

func TestRegistry_InjectFullBufferIsBusyNotBlock(t *testing.T) {
	r := NewRegistry()
	// A full buffer must report busy, not block the supervisor: the
	// run is not consuming guidance (wedged between turns).
	ch := make(chan string, 1)
	r.Register("scope:wedged", Run{SteerCh: ch})
	if err := r.Inject("scope:wedged", "first"); err != nil {
		t.Fatalf("first Inject: %v", err)
	}
	if err := r.Inject("scope:wedged", "second"); !errors.Is(err, ErrSteerBusy) {
		t.Errorf("second Inject = %v, want ErrSteerBusy", err)
	}
}

func TestRegistry_UnregisterBeforeClosePreventsSendOnClosed(t *testing.T) {
	// The lifecycle contract: a run (or session close) unregisters BEFORE
	// its channel closes, so a registry send can never land on a closed
	// channel. This test pins the ordering's safety net — the send side
	// takes the registry lock, so an Unregister that has returned means
	// no concurrent send is in flight.
	r := NewRegistry()
	ch := make(chan string, SteerBuffer)
	r.Register("scope:closing", Run{SteerCh: ch})
	r.Unregister("scope:closing")
	close(ch) // would panic if a send raced it; the unregister guarantees none
	if err := r.Inject("scope:closing", "x"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Inject after close-adjacent unregister = %v, want ErrNoRun", err)
	}
}

// TestRegistry_RegisterNilIsNoOp pins the honest-bridge contract: a nil
// channel registers nothing — a fabricated undrained channel would let
// Inject claim delivery into a void while status listed the scope as
// bridged.
func TestRegistry_RegisterNilIsNoOp(t *testing.T) {
	r := NewRegistry()
	r.Register("scope:nowhere", Run{})
	if r.Has("scope:nowhere") {
		t.Error("a nil channel must not register")
	}
	if err := r.Inject("scope:nowhere", "x"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Inject at a nil-registered scope = %v, want ErrNoRun", err)
	}
	if ids := r.ScopeIDs(); len(ids) != 0 {
		t.Errorf("status must not list a nil registration, got %v", ids)
	}
}

func TestRegistry_ScopeIDsSorted(t *testing.T) {
	r := NewRegistry()
	r.Register("scope:c", Run{SteerCh: make(chan string, SteerBuffer)})
	r.Register("scope:a", Run{SteerCh: make(chan string, SteerBuffer)})
	r.Register("scope:b", Run{SteerCh: make(chan string, SteerBuffer)})
	got := r.ScopeIDs()
	want := []string{"scope:a", "scope:b", "scope:c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ScopeIDs = %v, want %v", got, want)
		}
	}
}

// TestRegistry_CancelInvokesTheRunsCancellation pins the A3 halt bridge:
// Cancel calls the cancellation the run registered, is idempotent by
// context semantics, and reports honestly for scopes without a live,
// haltable run.
func TestRegistry_CancelInvokesTheRunsCancellation(t *testing.T) {
	r := NewRegistry()
	ch := make(chan string, SteerBuffer)

	cancelled := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		cancelled <- struct{}{}
	}()
	quiesceSrc := make(chan struct{})
	r.Register("scope:run-1", Run{SteerCh: ch, Cancel: cancel, Done: quiesceSrc})

	quiesce, err := r.Cancel("scope:run-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("the registered cancellation was never invoked")
	}
	// The quiesce channel: closed by the run when it stops — the
	// caller's wait-before-teardown handle.
	if quiesce == nil {
		t.Fatal("Cancel must return the run's quiesce channel")
	}
	select {
	case <-quiesce:
		t.Error("the quiesce channel must not be closed while the run is live")
	default:
	}

	// Idempotent: a second cancel on the same run is the same call on
	// the same context — no error, no second signal needed.
	if _, err := r.Cancel("scope:run-1"); err != nil {
		t.Errorf("second Cancel = %v, want nil (context cancellation is idempotent)", err)
	}

	// No run: honest refusal, never a phantom halt.
	if _, err := r.Cancel("scope:gone"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Cancel at an unregistered scope = %v, want ErrNoRun", err)
	}

	// A run registered without a cancellation is injectable but not
	// haltable — reported, not silently treated as halted.
	r.Register("scope:inject-only", Run{SteerCh: ch})
	if _, err := r.Cancel("scope:inject-only"); !errors.Is(err, ErrNotHaltable) {
		t.Errorf("Cancel at an inject-only run = %v, want ErrNotHaltable", err)
	}

	// After unregister, the run is finished: Cancel reports ErrNoRun.
	// The run stops and closes its quiesce channel, then unregisters:
	// the caller that waited on quiesce knows teardown is safe, and a
	// later Cancel reports ErrNoRun — never a phantom halt.
	close(quiesceSrc)
	r.Unregister("scope:run-1")
	if _, err := r.Cancel("scope:run-1"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Cancel after Unregister = %v, want ErrNoRun", err)
	}
}

// TestRegistry_GenerationIncrementsAcrossRegisters pins the
// generation-swap model's counter: each Register on one scope stamps a
// strictly larger generation, and an unregistered scope has none.
func TestRegistry_GenerationIncrementsAcrossRegisters(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Generation("scope:gen"); ok {
		t.Fatal("Generation on an unregistered scope must report false")
	}

	var prev uint64
	for i := 1; i <= 3; i++ {
		r.Register("scope:gen", Run{SteerCh: make(chan string, SteerBuffer)})
		gen, ok := r.Generation("scope:gen")
		if !ok {
			t.Fatalf("Register #%d: Generation must report the live registration", i)
		}
		if gen <= prev {
			t.Fatalf("Generation after Register #%d = %d, want strictly greater than %d", i, gen, prev)
		}
		prev = gen
	}
}

// TestRegistry_UnregisterGeneration pins the run-end half of the
// generation-swap model: the right generation removes, a stale
// generation is a no-op that leaves the successor registration intact.
// Each case builds its own successor scope so the table stays
// order-independent.
func TestRegistry_UnregisterGeneration(t *testing.T) {
	newSuccessorScope := func(t *testing.T) (r *Registry, staleGen, liveGen uint64) {
		t.Helper()
		r = NewRegistry()
		ch := make(chan string, SteerBuffer)
		r.Register("scope:swap", Run{SteerCh: ch})
		staleGen, _ = r.Generation("scope:swap")
		// A successor Register supersedes the first incarnation.
		r.Register("scope:swap", Run{SteerCh: ch})
		liveGen, _ = r.Generation("scope:swap")
		return r, staleGen, liveGen
	}

	tests := []struct {
		name        string
		gen         func(stale, live uint64) uint64
		wantRemoved bool
		wantLive    bool
	}{
		{
			name:        "stale generation is a no-op",
			gen:         func(stale, _ uint64) uint64 { return stale },
			wantRemoved: false,
			wantLive:    true,
		},
		{
			name:        "matching generation removes the successor",
			gen:         func(_, live uint64) uint64 { return live },
			wantRemoved: true,
			wantLive:    false,
		},
		{
			name:        "unknown generation never removes",
			gen:         func(_, _ uint64) uint64 { return 9999 },
			wantRemoved: false,
			wantLive:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, staleGen, liveGen := newSuccessorScope(t)
			gen := tt.gen(staleGen, liveGen)
			if got := r.UnregisterGeneration("scope:swap", gen); got != tt.wantRemoved {
				t.Errorf("UnregisterGeneration(%d) = %v, want %v", gen, got, tt.wantRemoved)
			}
			if liveNow := r.Has("scope:swap"); liveNow != tt.wantLive {
				t.Errorf("Has after UnregisterGeneration = %v, want %v", liveNow, tt.wantLive)
			}
			if tt.wantLive {
				if genNow, ok := r.Generation("scope:swap"); !ok || genNow != liveGen {
					t.Errorf("successor Generation = (%d, %v), want (%d, true) — the stale call must leave it intact", genNow, ok, liveGen)
				}
			}
		})
	}
}

// TestRegistry_RedirectConcurrentClaim pins the compare-and-swap: many
// concurrent Redirect calls at the same scope and generation yield
// exactly one winner; every loser reports ErrAlreadyClaimed.
func TestRegistry_RedirectConcurrentClaim(t *testing.T) {
	const claimants = 8
	r := NewRegistry()
	r.Register("scope:race", Run{SteerCh: make(chan string, SteerBuffer)})
	gen, ok := r.Generation("scope:race")
	if !ok {
		t.Fatal("Generation must report the live registration")
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wins := make(chan RedirectHandle, claimants)
	losses := make(chan error, claimants)
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			handle, err := r.Redirect("scope:race", gen, RedirectRequest{})
			if err != nil {
				losses <- err
				return
			}
			wins <- handle
		}()
	}
	close(start)
	wg.Wait()
	close(wins)
	close(losses)

	if len(wins) != 1 {
		t.Fatalf("winners = %d, want exactly 1", len(wins))
	}
	handle := <-wins
	if handle.ScopeID != "scope:race" || handle.Gen != gen {
		t.Errorf("handle = {ScopeID:%q Gen:%d}, want {ScopeID:%q Gen:%d}", handle.ScopeID, handle.Gen, "scope:race", gen)
	}
	if len(losses) != claimants-1 {
		t.Fatalf("losers = %d, want %d", len(losses), claimants-1)
	}
	for err := range losses {
		if !errors.Is(err, ErrAlreadyClaimed) {
			t.Errorf("loser error = %v, want ErrAlreadyClaimed", err)
		}
	}
}

// TestRegistry_RedirectUnregisteredScope: Redirect at a scope with no
// live registration reports ErrNoRun — before and after Unregister.
func TestRegistry_RedirectUnregisteredScope(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Redirect("scope:gone", 1, RedirectRequest{}); !errors.Is(err, ErrNoRun) {
		t.Errorf("Redirect at an unregistered scope = %v, want ErrNoRun", err)
	}

	r.Register("scope:gone", Run{SteerCh: make(chan string, SteerBuffer)})
	r.Unregister("scope:gone")
	if _, err := r.Redirect("scope:gone", 1, RedirectRequest{}); !errors.Is(err, ErrNoRun) {
		t.Errorf("Redirect after Unregister = %v, want ErrNoRun", err)
	}
}

// TestRegistry_RedirectGenerationMismatch: a fromGen that no longer
// names the live incarnation reports ErrNoRun — both a wrong number and
// a superseded generation — while the current generation claims cleanly.
func TestRegistry_RedirectGenerationMismatch(t *testing.T) {
	r := NewRegistry()
	r.Register("scope:mismatch", Run{SteerCh: make(chan string, SteerBuffer)})
	firstGen, _ := r.Generation("scope:mismatch")

	if _, err := r.Redirect("scope:mismatch", firstGen+1, RedirectRequest{}); !errors.Is(err, ErrNoRun) {
		t.Errorf("Redirect with fromGen+1 = %v, want ErrNoRun", err)
	}

	// A successor Register supersedes the first incarnation: the old
	// generation no longer names a live run.
	r.Register("scope:mismatch", Run{SteerCh: make(chan string, SteerBuffer)})
	successorGen, _ := r.Generation("scope:mismatch")
	if _, err := r.Redirect("scope:mismatch", firstGen, RedirectRequest{}); !errors.Is(err, ErrNoRun) {
		t.Errorf("Redirect with the superseded generation = %v, want ErrNoRun", err)
	}

	handle, err := r.Redirect("scope:mismatch", successorGen, RedirectRequest{})
	if err != nil {
		t.Fatalf("Redirect with the current generation: %v", err)
	}
	if handle.ScopeID != "scope:mismatch" || handle.Gen != successorGen {
		t.Errorf("handle = {ScopeID:%q Gen:%d}, want {ScopeID:%q Gen:%d}", handle.ScopeID, handle.Gen, "scope:mismatch", successorGen)
	}
}

// TestRegistry_RedirectGuidanceRoundTrip pins the claim → consume →
// close lifecycle (PR #398 review findings): the claim publishes the
// COMPLETE payload atomically — no empty-slot window — the
// single-winner mark outlives the payload slot, and the final empty
// consume closes the generation so nothing can land after teardown and
// be silently dropped. A successor Register is a fresh, unclaimed
// generation.
func TestRegistry_RedirectGuidanceRoundTrip(t *testing.T) {
	r := NewRegistry()
	r.Register("scope:handoff", Run{SteerCh: make(chan string, SteerBuffer)})
	gen, ok := r.Generation("scope:handoff")
	if !ok {
		t.Fatal("Generation must report the live registration")
	}

	// The claim carries the complete payload in one publication.
	if _, err := r.Redirect("scope:handoff", gen, RedirectRequest{Guidance: "plan B"}); err != nil {
		t.Fatalf("Redirect: %v", err)
	}

	// The single-winner mark refuses a second claim outright.
	if _, err := r.Redirect("scope:handoff", gen, RedirectRequest{Guidance: "plan C"}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Errorf("second Redirect = %v, want ErrAlreadyClaimed", err)
	}

	// Consume returns the guidance once and clears the payload slot.
	req, consumed := r.ConsumeRedirectOrClose("scope:handoff", gen)
	if !consumed || req.Guidance != "plan B" {
		t.Errorf("ConsumeRedirectOrClose = ({%q}, %v), want {\"plan B\"}, true", req.Guidance, consumed)
	}

	// The mark outlives the slot: a claim after the consume, before
	// the close, is still refused.
	if _, err := r.Redirect("scope:handoff", gen, RedirectRequest{Guidance: "plan C"}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Errorf("Redirect after consume = %v, want ErrAlreadyClaimed (the mark outlives the slot)", err)
	}

	// The final empty consume closes the generation. Nothing may land
	// after it (review finding: a late redirect used to succeed,
	// promise a successor, and be silently dropped at teardown).
	if _, consumed := r.ConsumeRedirectOrClose("scope:handoff", gen); consumed {
		t.Error("ConsumeRedirectOrClose must report nothing on an empty slot")
	}
	if _, err := r.Redirect("scope:handoff", gen, RedirectRequest{Guidance: "late"}); !errors.Is(err, ErrNoRun) {
		t.Errorf("Redirect after the final consume = %v, want ErrNoRun", err)
	}

	// A wrong generation never consumes.
	if _, consumed := r.ConsumeRedirectOrClose("scope:handoff", gen+1); consumed {
		t.Error("ConsumeRedirectOrClose at a wrong generation must report nothing")
	}

	// A successor Register is a fresh, unclaimed generation: no stale
	// slot rides across the swap.
	r.Register("scope:handoff", Run{SteerCh: make(chan string, SteerBuffer)})
	next, ok := r.Generation("scope:handoff")
	if !ok || next == gen {
		t.Fatalf("successor Generation = (%d, %v), want a new generation after %d", next, ok, gen)
	}
	if _, consumed := r.ConsumeRedirectOrClose("scope:handoff", next); consumed {
		t.Error("a fresh generation must carry no pending redirect")
	}
}

// TestRegistry_ClaimRacesFinalConsume_NoEmptyPayloadNoDrop pins the
// atomic claim → consume → close window (yaah-96rj, yaah-74zs,
// yaah-srl6 — PR #398 review findings 1/2/14): a redirect racing the
// loop's final ConsumeRedirectOrClose either claims BEFORE the close
// and is delivered complete, or is refused — it can never be accepted
// and then silently dropped at teardown, and a delivered request is
// never empty.
func TestRegistry_ClaimRacesFinalConsume_NoEmptyPayloadNoDrop(t *testing.T) {
	const claimants = 8
	for iter := 0; iter < 50; iter++ {
		r := NewRegistry()
		r.Register("scope:race-close", Run{SteerCh: make(chan string, SteerBuffer)})
		gen, ok := r.Generation("scope:race-close")
		if !ok {
			t.Fatal("Generation must report the live registration")
		}

		start := make(chan struct{})
		wins := make(chan RedirectHandle, claimants)
		var wg sync.WaitGroup
		for i := 0; i < claimants; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				handle, err := r.Redirect("scope:race-close", gen, RedirectRequest{Guidance: "handoff"})
				switch {
				case err == nil:
					wins <- handle
				case errors.Is(err, ErrAlreadyClaimed), errors.Is(err, ErrNoRun):
				default:
					t.Errorf("unexpected Redirect error: %v", err)
				}
			}()
		}
		type consumedPayload struct {
			req      RedirectRequest
			consumed bool
		}
		got := make(chan consumedPayload, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req, consumed := r.ConsumeRedirectOrClose("scope:race-close", gen)
			if consumed && req.Guidance != "handoff" {
				t.Errorf("consumed an incomplete payload: %+v", req)
			}
			got <- consumedPayload{req, consumed}
		}()
		close(start)
		wg.Wait()
		close(wins)
		p := <-got

		switch {
		case len(wins) > 1:
			t.Fatalf("iter %d: %d claims won the CAS, want at most one", iter, len(wins))
		case p.consumed && len(wins) != 1:
			t.Fatalf("iter %d: payload consumed but no claim won", iter)
		case !p.consumed && len(wins) != 0:
			// The exact bug shape the fix forbids: a claim was accepted
			// but the parent loop's read saw nothing — the handoff would
			// be promised and silently dropped.
			t.Fatalf("iter %d: a claim won but the consumer saw nothing — accepted then dropped", iter)
		}

		// Once the window settled (consumed-and-marked, or closed),
		// every late claim is refused — nothing lands after teardown.
		if _, err := r.Redirect("scope:race-close", gen, RedirectRequest{Guidance: "late"}); err == nil {
			t.Fatalf("iter %d: a claim succeeded after the window closed", iter)
		}
	}
}

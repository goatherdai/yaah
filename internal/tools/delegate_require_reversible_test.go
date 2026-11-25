package tools

// delegate_require_reversible_test.go pins the tool half of
// supervisor.require_reversible (design §4.2, §4.9; yaah-ugtl review findings
// M2 and M1-nested) and the D5 precedence of the per-call supervision override
// in one table: the session posture decides by default, an explicit
// `supervise: true` opts in, an explicit `supervise: false` opts out — unless
// the operator turned require_reversible on, in which case a supervised
// session's dispatch may not opt out and a nested dispatch may not run at all.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// TestSupervisedDispatchPrecedence pins the D5 precedence and its consequence:
// what the runner is told (the marker) and where the dispatch runs (the lane).
func TestSupervisedDispatchPrecedence(t *testing.T) {
	f := false
	tr := true
	for _, tc := range []struct {
		name       string
		session    bool // the tool's session posture (supervisor.enabled: true)
		requireRev bool // supervisor.require_reversible
		nested     bool // a delegate tool registered inside a dispatch
		override   *bool
		wantRefuse error  // nil means the dispatch runs
		refuseText string // matched against the refusal when it carries no sentinel
		wantMarker *bool  // nil means "not reached"
		wantLane   string // "" means "not reached"
	}{
		{
			// D5 default: supervision off, no override — today's behavior.
			name: "default off, no override", wantMarker: &f, wantLane: string(LaneShared),
		},
		{
			// Opting in per call cannot conjure the host tick that would watch
			// the dispatch, so it is refused rather than silently honored.
			name: "default off, supervise true", override: &tr, refuseText: "supervision enabled for the session",
		},
		{
			name: "default on, no override", session: true, wantLane: string(LaneReport),
		},
		{
			// The opt-out wins over the session default, untouched.
			name: "default on, supervise false", session: true, override: &f, wantMarker: &f, wantLane: string(LaneShared),
		},
		{
			// Operator authority: with the key on, the same opt-out is refused
			// before any world is provisioned.
			name:    "default on, supervise false, require_reversible",
			session: true, requireRev: true, override: &f, wantRefuse: supervisionapi.ErrSupervisedNotReversible,
		},
		{
			// A nested dispatch runs on the session tree, outside the parent's
			// rollback, so the key refuses it.
			name:    "nested, require_reversible",
			session: true, requireRev: true, nested: true, wantRefuse: supervisionapi.ErrSupervisedNotReversible,
		},
		{
			// ...and without the key it runs exactly as before.
			name: "nested, no require_reversible", session: true, nested: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &sessionRunner{responses: []runnerResponse{{result: "ok"}}}
			rt := newRetainedTestEnv(t, runner)
			var marker *bool
			tt := &DelegateTool{
				RoleNames:         []string{"counter"},
				Runner:            runner.run(),
				Retained:          &rt,
				LaneForRole:       func(string) DelegateLane { return LaneShared },
				Supervised:        tc.session,
				RequireReversible: tc.requireRev,
				Nested:            tc.nested,
			}
			// The marker the runner sees is the resolved posture, whatever the
			// lane path does with it.
			tt.Runner = func(ctx context.Context, _ string, _ SubAgentParams) (string, error) {
				m := SupervisedDispatch(ctx)
				marker = &m
				return "ok", nil
			}

			args := `{"description":"d","prompt":"p","role":"counter"}`
			if tc.override != nil {
				if *tc.override {
					args = `{"description":"d","prompt":"p","role":"counter","supervise":true}`
				} else {
					args = `{"description":"d","prompt":"p","role":"counter","supervise":false}`
				}
			}
			res, err := tt.Execute(context.Background(), args)

			if tc.wantRefuse != nil || tc.refuseText != "" {
				if err == nil {
					t.Fatal("the dispatch was accepted; it must be refused")
				}
				if tc.wantRefuse != nil && !errors.Is(err, tc.wantRefuse) {
					t.Fatalf("Execute = %v, want %v", err, tc.wantRefuse)
				}
				if tc.refuseText != "" && !strings.Contains(err.Error(), tc.refuseText) {
					t.Fatalf("Execute = %v, want it to mention %q", err, tc.refuseText)
				}
				if marker != nil {
					t.Error("a refused dispatch must never reach the runner")
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if tc.wantMarker != nil {
				if marker == nil {
					t.Fatal("the dispatch never reached the runner")
				}
				if *marker != *tc.wantMarker {
					t.Errorf("supervised marker = %v, want %v", *marker, *tc.wantMarker)
				}
			}
			_, lane, _ := decodeStatusLane(t, res)
			if tc.wantLane != "" && lane != tc.wantLane {
				t.Errorf("lane = %q, want %q", lane, tc.wantLane)
			}
		})
	}
}

// TestRequireReversibleOptOutRefusalIsActionable pins that the refusal names the
// config key and both ways out, because the model cannot change config and has
// to be told the reachable one.
func TestRequireReversibleOptOutRefusalIsActionable(t *testing.T) {
	tt := &DelegateTool{
		RoleNames:         []string{"counter"},
		Runner:            func(context.Context, string, SubAgentParams) (string, error) { return "ok", nil },
		Supervised:        true,
		RequireReversible: true,
	}
	_, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter","supervise":false}`)
	if err == nil {
		t.Fatal(`supervise: false under require_reversible must be refused`)
	}
	for _, want := range []string{"supervisor.require_reversible", `"supervise": false`, "remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q, got: %v", want, err)
		}
	}
}

// TestNestedRefusalExplainsWhy pins the nested refusal's content: it names the
// key, says where the grandchild would run instead, and offers the actionable
// alternative.
func TestNestedRefusalExplainsWhy(t *testing.T) {
	tt := &DelegateTool{
		RoleNames:         []string{"counter"},
		Runner:            func(context.Context, string, SubAgentParams) (string, error) { return "ok", nil },
		Supervised:        true,
		RequireReversible: true,
		Nested:            true,
	}
	_, err := tt.Execute(context.Background(), `{"description":"d","prompt":"p","role":"counter"}`)
	if !errors.Is(err, supervisionapi.ErrSupervisedNotReversible) {
		t.Fatalf("nested dispatch = %v, want ErrSupervisedNotReversible", err)
	}
	for _, want := range []string{"session tree", "supervisor.require_reversible", "nested dispatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("nested refusal must mention %q, got: %v", want, err)
		}
	}
}
